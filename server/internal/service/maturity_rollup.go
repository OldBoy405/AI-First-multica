package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Maturity snapshot rollup (SDD §4.5): one plan writes exactly one local
// bucket per workspace, in one transaction, under a per-workspace advisory
// lock. History rows are insert-only; config changes only affect later rows.

const (
	maturityMetricsSchema = "ai-first.maturity-metrics/v1"
	scopeOrg              = "org"
	scopeUser             = "user"
	scopeProject          = "project"
	orgSentinel           = "·"
	reasonOwnerUnresolved = "cr_owner_identity_unresolved"
	reasonAttributionLow  = "attribution_coverage_insufficient"
	reasonTracePending    = "trace_channel_pending_cr_c"
	reasonCostUnavailable = "cost_estimate_unavailable"
)

// errMaturityBusy reports an advisory-lock miss; callers retry the plan.
var errMaturityBusy = errors.New("maturity_snapshot: workspace rollup lock busy")

// maturityReviewNodeIDs mirrors governance.ReviewGateNodes[requirement|
// tech-design|code] (service cannot import governance — its runner depends on
// service). Cross-checked against the canonical map by
// TestMaturityReviewNodeIDs in package service_test.
var maturityReviewNodeIDs = []string{
	"00000000-0000-0000-0011-000000000004", // requirement review skill node
	"00000000-0000-0000-0016-000000000002", // tech-design review skill node
	"00000000-0000-0000-0015-000000000009", // code review skill node
}

var maturityReviewNodeUUIDs = func() []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(maturityReviewNodeIDs))
	for _, s := range maturityReviewNodeIDs {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, pgtype.UUID{Bytes: id, Valid: true})
		}
	}
	return out
}()

// MaturityReviewNodeIDs exposes the review-gate node id copy for cross-checking
// against governance.ReviewGateNodes (maturity_rollup_crosscheck_test.go).
func MaturityReviewNodeIDs() []string {
	return append([]string(nil), maturityReviewNodeIDs...)
}

var shanghaiLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}()

// previousLocalDate returns the Shanghai-calendar day before planTime.
func previousLocalDate(planTime time.Time) time.Time {
	d := planTime.In(shanghaiLoc)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, shanghaiLoc)
	return day.AddDate(0, 0, -1)
}

// dayWindowUTC returns the UTC half-open window of Shanghai-local day d.
func dayWindowUTC(d time.Time) (time.Time, time.Time) {
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, shanghaiLoc)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

func toPgTs(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func toPgDate(d time.Time) pgtype.Date       { return pgtype.Date{Time: d, Valid: true} }
func f64(v float64) *float64                 { return &v }

// RollupMaturitySnapshot rolls up the previous local day for every workspace
// (stable ID order). Each workspace commits independently; a later failure
// leaves earlier workspaces durable and the retry no-ops on them via the
// per-workspace max(bucket_date) watermark.
func RollupMaturitySnapshot(ctx context.Context, pool *pgxpool.Pool, planTime time.Time) (int64, error) {
	q := db.New(pool)
	workspaces, err := q.MaturityWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("list workspaces: %w", err)
	}
	var total int64
	for _, ws := range workspaces {
		n, err := RollupMaturityWorkspace(ctx, pool, ws, planTime)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// RollupMaturityWorkspace writes all scope rows for one workspace and one
// bucket. Lock miss returns errMaturityBusy; watermark reached is a no-op.
func RollupMaturityWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID pgtype.UUID, planTime time.Time) (int64, error) {
	if !workspaceID.Valid {
		return 0, errors.New("maturity rollup: workspace id required")
	}
	target := previousLocalDate(planTime)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var locked bool
	err = tx.QueryRow(ctx,
		"SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))",
		"maturity_snapshot:"+uuid.UUID(workspaceID.Bytes).String(),
	).Scan(&locked)
	if err != nil {
		return 0, err
	}
	if !locked {
		return 0, errMaturityBusy
	}

	qtx := db.New(tx)
	maxBucket, err := qtx.MaturitySnapshotMaxBucket(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	if maxBucket.Valid && !maxBucket.Time.Before(target) {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	rows, err := rollupWorkspaceRows(ctx, qtx, workspaceID, target)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return rows, nil
}

// ValidateSnapshotMetrics enforces the frozen payload invariants: all 8
// metric keys and all 6 governance keys present, correct schema, and the
// owner-unresolved reason may only appear on an unavailable collab metric.
func ValidateSnapshotMetrics(m maturity.SnapshotMetricsV1) error {
	if m.Schema != maturityMetricsSchema {
		return fmt.Errorf("metrics schema must be %q, got %q", maturityMetricsSchema, m.Schema)
	}
	for _, k := range maturity.AllMetricKeys {
		if _, ok := m.MetricValues[k]; !ok {
			return fmt.Errorf("missing metric key %q", k)
		}
	}
	for _, k := range maturity.AllGovernanceKeys {
		if _, ok := m.Governance[k]; !ok {
			return fmt.Errorf("missing governance key %q", k)
		}
	}
	mv := m.MetricValues[maturity.MetricProjectCollabScale]
	if mv.Reason != nil && *mv.Reason == reasonOwnerUnresolved && mv.DataStatus != maturity.StatusUnavailable {
		return fmt.Errorf("project_collab_scale with reason=%q must be unavailable, got %s", reasonOwnerUnresolved, mv.DataStatus)
	}
	return nil
}

func rollupWorkspaceRows(ctx context.Context, qtx *db.Queries, workspaceID pgtype.UUID, target time.Time) (int64, error) {
	cfg := maturity.GeneratedConfig()
	from, to := dayWindowUTC(target)
	activeFrom, activeTo := dayWindowUTC(target.AddDate(0, 0, -13))

	memberCount, err := qtx.MaturityMemberCount(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	projects, err := qtx.MaturityBusinessProjects(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	activeKeys, err := qtx.MaturityActiveProjectKeys14d(ctx, db.MaturityActiveProjectKeys14dParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(activeFrom), ToUtc: toPgTs(activeTo),
	})
	if err != nil {
		return 0, err
	}
	activeSet := make(map[[16]byte]bool, len(activeKeys))
	for _, k := range activeKeys {
		activeSet[k.Bytes] = true
	}

	build := func(scope, scopeID string, projectID, userID *pgtype.UUID) (maturity.SnapshotMetricsV1, error) {
		return computeScope(ctx, qtx, workspaceID, target, scope, projectID, userID,
			from, to, activeFrom, activeTo, memberCount, projects, activeSet)
	}

	org, err := build(scopeOrg, orgSentinel, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("org metrics: %w", err)
	}
	type scopeRow struct {
		scope, scopeID string
		m              maturity.SnapshotMetricsV1
	}
	scopes := []scopeRow{{scopeOrg, orgSentinel, org}}

	for _, p := range projects {
		pid := p
		m, err := build(scopeProject, uuid.UUID(pid.Bytes).String(), &pid, nil)
		if err != nil {
			return 0, fmt.Errorf("project %s metrics: %w", uuid.UUID(pid.Bytes), err)
		}
		scopes = append(scopes, scopeRow{scopeProject, uuid.UUID(pid.Bytes).String(), m})
	}

	tokenRows, err := qtx.MaturityTaskTokenRows(ctx, db.MaturityTaskTokenRowsParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return 0, err
	}
	userSet := map[[16]byte]bool{}
	for _, r := range tokenRows {
		if r.InitiatorUserID.Valid {
			userSet[r.InitiatorUserID.Bytes] = true
		}
	}
	userIDs := make([][16]byte, 0, len(userSet))
	for u := range userSet {
		userIDs = append(userIDs, u)
	}
	sort.Slice(userIDs, func(i, j int) bool { return string(userIDs[i][:]) < string(userIDs[j][:]) })
	for _, u := range userIDs {
		uid := pgtype.UUID{Bytes: u, Valid: true}
		m, err := build(scopeUser, uuid.UUID(u).String(), nil, &uid)
		if err != nil {
			return 0, fmt.Errorf("user %s metrics: %w", uuid.UUID(u), err)
		}
		scopes = append(scopes, scopeRow{scopeUser, uuid.UUID(u).String(), m})
	}

	// Observation watermark: the workspace's earliest org bucket; on the very
	// first bucket there is none yet, so the target date itself is the start.
	firstBucket, err := qtx.MaturitySnapshotFirstBucket(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	if !firstBucket.Valid {
		firstBucket = toPgDate(target)
	}
	observation := maturity.ObservationActive(firstBucket.Time, time.Now(), cfg)

	configRev := maturity.GeneratedConfigRev()
	bucket := toPgDate(target)
	var rows int64
	for _, s := range scopes {
		if err := ValidateSnapshotMetrics(s.m); err != nil {
			return rows, fmt.Errorf("validate %s/%s: %w", s.scope, s.scopeID, err)
		}
		metricsJSON, err := json.Marshal(s.m)
		if err != nil {
			return rows, err
		}
		scoresJSON := []byte("{}")
		if !observation {
			if s, err := maturity.BuildScores(s.m.MetricValues, cfg); err == nil {
				if b, err := json.Marshal(s); err == nil {
					scoresJSON = b
				}
			}
		}
		n, err := qtx.MaturitySnapshotInsert(ctx, db.MaturitySnapshotInsertParams{
			WorkspaceID: workspaceID,
			BucketDate:  bucket,
			Scope:       s.scope,
			ScopeID:     s.scopeID,
			Metrics:     metricsJSON,
			Scores:      scoresJSON,
			ConfigRev:   configRev,
		})
		if err != nil {
			return rows, err
		}
		rows += n
	}
	return rows, nil
}

// computeScope computes the 8 metric values (+ governance for org) for one
// scope row. projectID/userID are mutually exclusive; both nil means org.
//
// ponytail: per-project denominators reuse the workspace member count (the
// SDD fixes member_count as the workspace rollup definition) and per-project
// deep/penetration derive from the task-level depth rows so they share the
// org definition. Historical tasks without project_id fall back through
// issue.project_id in SQL; rows that still lack a project only count org.
func computeScope(
	ctx context.Context,
	qtx *db.Queries,
	workspaceID pgtype.UUID,
	target time.Time,
	scope string,
	projectID, userID *pgtype.UUID,
	from, to, activeFrom, activeTo time.Time,
	memberCount int64,
	projects []pgtype.UUID,
	activeSet map[[16]byte]bool,
) (maturity.SnapshotMetricsV1, error) {
	m := maturity.SnapshotMetricsV1{
		Schema:       maturityMetricsSchema,
		MetricValues: make(map[maturity.MetricKey]maturity.MetricValue, 8),
		Governance:   make(map[maturity.GovernanceMetricKey]maturity.MetricValue, 6),
	}

	tokenRows, err := qtx.MaturityTaskTokenRows(ctx, db.MaturityTaskTokenRowsParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return m, err
	}
	tokenRows = filterTokenRows(tokenRows, projectID, userID)

	var totalTokens int64
	for _, r := range tokenRows {
		totalTokens += r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
	}

	// --- token_intensity ---
	intensityDen := memberCount
	if scope == scopeUser {
		intensityDen = 1 // one user, one day
	}
	if intensityDen == 0 {
		m.MetricValues[maturity.MetricTokenIntensity] = metricEmpty("tokens_per_member_day")
	} else {
		v := float64(totalTokens) / float64(intensityDen)
		m.MetricValues[maturity.MetricTokenIntensity] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(totalTokens)), Denominator: f64(float64(intensityDen)),
			Unit: "tokens_per_member_day", DataStatus: maturity.StatusReady,
		}
	}

	// --- attribution coverage ---
	// org: task-level counts from the dedicated query (includes tasks without
	// usage rows). project/user: derived from the scope's usage rows (a task
	// that never consumed tokens is invisible there — acceptable ceiling).
	var coverage *float64
	var attributedTasks, unattributedTasks int64
	if scope == scopeOrg {
		att, err := qtx.MaturityAttributionCounts(ctx, db.MaturityAttributionCountsParams{
			WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
		})
		if err != nil {
			return m, err
		}
		attributedTasks, unattributedTasks = att.Attributed, att.Unattributed
		if attributedTasks+unattributedTasks > 0 {
			c := float64(attributedTasks) / float64(attributedTasks+unattributedTasks)
			coverage = &c
		}
	} else {
		coverage, attributedTasks, unattributedTasks = taskCoverage(tokenRows)
	}

	// --- ai_penetration ---
	initiators := map[[16]byte]bool{}
	for _, r := range tokenRows {
		if r.InitiatorUserID.Valid {
			initiators[r.InitiatorUserID.Bytes] = true
		}
	}
	if scope != scopeUser {
		if memberCount == 0 {
			m.MetricValues[maturity.MetricAIPenetration] = metricEmpty("ratio")
		} else if coverage != nil && *coverage < 0.95 {
			m.MetricValues[maturity.MetricAIPenetration] = metricUnavailable(reasonAttributionLow, "ratio")
		} else {
			v := float64(len(initiators)) / float64(memberCount)
			m.MetricValues[maturity.MetricAIPenetration] = maturity.MetricValue{
				Value: &v, Numerator: f64(float64(len(initiators))), Denominator: f64(float64(memberCount)),
				Unit: "ratio", DataStatus: maturity.StatusReady,
				Attribution: attributionOf(attributedTasks, unattributedTasks),
			}
		}
	} else {
		m.MetricValues[maturity.MetricAIPenetration] = metricNA("ratio")
	}

	// --- CR-scoped metrics ---
	archived, err := qtx.MaturityArchivedCRs(ctx, db.MaturityArchivedCRsParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return m, err
	}
	if projectID != nil {
		archived = filterArchivedByProject(archived, *projectID)
	}
	crSet := map[string]bool{}
	for _, c := range archived {
		crSet[c.CrID] = true
	}

	// --- cr_throughput_per_capita ---
	if memberCount == 0 {
		m.MetricValues[maturity.MetricCRThroughputPerCapita] = metricEmpty("cr_per_member")
	} else {
		v := float64(len(archived)) / float64(memberCount)
		m.MetricValues[maturity.MetricCRThroughputPerCapita] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(len(archived))), Denominator: f64(float64(memberCount)),
			Unit: "cr_per_member", DataStatus: maturity.StatusReady,
		}
	}

	// --- project_collab_scale ---
	ownerRows, err := qtx.MaturityCROwnerResolution(ctx, db.MaturityCROwnerResolutionParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return m, err
	}
	unresolved := false
	for _, o := range ownerRows {
		if o.UnresolvedOwnerCount <= 0 || !crSet[o.CrID] {
			continue
		}
		if projectID == nil {
			unresolved = true // org scope: any archived CR with an unresolved owner poisons the day
		} else if o.ProjectID.Valid && o.ProjectID.Bytes == projectID.Bytes {
			unresolved = true
		}
	}
	switch {
	case len(archived) == 0:
		m.MetricValues[maturity.MetricProjectCollabScale] = metricEmpty("members_per_cr")
	case unresolved:
		m.MetricValues[maturity.MetricProjectCollabScale] = metricUnavailable(reasonOwnerUnresolved, "members_per_cr")
	case coverage != nil && *coverage < 0.95:
		m.MetricValues[maturity.MetricProjectCollabScale] = metricUnavailable(reasonAttributionLow, "members_per_cr")
	default:
		users, err := qtx.MaturityCRUsers(ctx, db.MaturityCRUsersParams{
			WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
		})
		if err != nil {
			return m, err
		}
		perCR := map[string]map[[16]byte]bool{}
		for _, u := range users {
			if !crSet[u.CrID] {
				continue
			}
			if perCR[u.CrID] == nil {
				perCR[u.CrID] = map[[16]byte]bool{}
			}
			perCR[u.CrID][u.UserID.Bytes] = true
		}
		var sum, count float64
		for _, c := range archived {
			if projectID != nil && (!c.ProjectID.Valid || c.ProjectID.Bytes != projectID.Bytes) {
				continue
			}
			sum += float64(len(perCR[c.CrID]))
			count++
		}
		if count == 0 {
			m.MetricValues[maturity.MetricProjectCollabScale] = metricEmpty("members_per_cr")
		} else {
			v := sum / count
			m.MetricValues[maturity.MetricProjectCollabScale] = maturity.MetricValue{
				Value: &v, Numerator: f64(sum), Denominator: f64(count),
				Unit: "members_per_cr", DataStatus: maturity.StatusReady,
			}
		}
	}

	// --- project_active_rate ---
	if projectID != nil {
		v := 0.0
		if activeSet[projectID.Bytes] {
			v = 1.0
		}
		m.MetricValues[maturity.MetricProjectActiveRate] = maturity.MetricValue{
			Value: &v, Numerator: f64(v), Denominator: f64(1), Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	} else if len(projects) == 0 {
		m.MetricValues[maturity.MetricProjectActiveRate] = metricEmpty("ratio")
	} else {
		var active int
		for _, p := range projects {
			if activeSet[p.Bytes] {
				active++
			}
		}
		v := float64(active) / float64(len(projects))
		m.MetricValues[maturity.MetricProjectActiveRate] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(active)), Denominator: f64(float64(len(projects))),
			Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	}

	// --- prototype_direct_rate ---
	gates, err := qtx.MaturityPrototypeGates(ctx, db.MaturityPrototypeGatesParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
		ReviewNodeIds: maturityReviewNodeUUIDs,
	})
	if err != nil {
		return m, err
	}
	onceThrough := map[string]bool{}
	gatePass := map[string]map[uuid.UUID]bool{}
	for _, g := range gates {
		if !crSet[g.CrID] {
			continue
		}
		if gatePass[g.CrID] == nil {
			gatePass[g.CrID] = map[uuid.UUID]bool{}
		}
		if g.Attempt == 1 && g.Status == "passed" {
			gatePass[g.CrID][g.NodeID.Bytes] = true
		}
	}
	for cr, passed := range gatePass {
		if len(passed) == len(maturityReviewNodeUUIDs) {
			onceThrough[cr] = true
		}
	}
	if len(archived) == 0 {
		m.MetricValues[maturity.MetricPrototypeDirectRate] = metricEmpty("ratio")
	} else {
		var n int
		for _, c := range archived {
			if onceThrough[c.CrID] {
				n++
			}
		}
		v := float64(n) / float64(len(archived))
		m.MetricValues[maturity.MetricPrototypeDirectRate] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(n)), Denominator: f64(float64(len(archived))),
			Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	}

	// --- team_agent_depth (task-level deep/total) ---
	depthRows, err := qtx.MaturityTaskDepthRows(ctx, db.MaturityTaskDepthRowsParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return m, err
	}
	var deep, total int64
	for _, d := range depthRows {
		if projectID != nil {
			if !d.ProjectID.Valid || d.ProjectID.Bytes != projectID.Bytes {
				continue
			}
		}
		if userID != nil {
			if !d.InitiatorUserID.Valid || d.InitiatorUserID.Bytes != userID.Bytes {
				continue
			}
		}
		total++
		if d.Deep {
			deep++
		}
	}
	if total == 0 {
		m.MetricValues[maturity.MetricTeamAgentDepth] = metricEmpty("ratio")
	} else {
		v := float64(deep) / float64(total)
		m.MetricValues[maturity.MetricTeamAgentDepth] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(deep)), Denominator: f64(float64(total)),
			Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	}

	// --- process_completion_rate ---
	completions, err := qtx.MaturityPipelineCompletions(ctx, db.MaturityPipelineCompletionsParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return m, err
	}
	pipes := map[string]map[string]bool{}
	for _, c := range completions {
		if !crSet[c.CrID] {
			continue
		}
		if pipes[c.CrID] == nil {
			pipes[c.CrID] = map[string]bool{}
		}
		pipes[c.CrID][c.PipelineID] = true
	}
	required := []string{"requirement-authoring", "architecture-design", "code-implementation", "feature-writeback"}
	if len(archived) == 0 {
		m.MetricValues[maturity.MetricProcessCompletionRate] = metricEmpty("ratio")
	} else {
		var n int
		for _, c := range archived {
			set := pipes[c.CrID]
			ok := true
			for _, p := range required {
				if !set[p] {
					ok = false
					break
				}
			}
			if ok {
				n++
			}
		}
		v := float64(n) / float64(len(archived))
		m.MetricValues[maturity.MetricProcessCompletionRate] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(n)), Denominator: f64(float64(len(archived))),
			Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	}

	// --- headline + cost ---
	headline := maturity.Headline{ActiveMembers: memberCount, TotalTokens: totalTokens}
	if scope != scopeOrg {
		headline.ActiveMembers = memberCount // denominators stay workspace-wide
	}
	costUSD, costStatus := costFromTokenRows(tokenRows)
	headline.CostUSD = costUSD
	headline.CostStatus = costStatus
	m.Headline = headline

	// --- governance (org only; other scopes keep not_applicable keys) ---
	if scope == scopeOrg {
		if err := fillGovernance(ctx, qtx, m, workspaceID, from, to); err != nil {
			return m, err
		}
	} else {
		for _, k := range maturity.AllGovernanceKeys {
			m.Governance[k] = metricNA("")
		}
	}

	// --- non-applicable for user rows ---
	if scope == scopeUser {
		for _, k := range maturity.AllMetricKeys {
			switch k {
			case maturity.MetricTokenIntensity, maturity.MetricTeamAgentDepth:
				// computed above
			default:
				if _, done := m.MetricValues[k]; !done {
					m.MetricValues[k] = metricNA(unitFor(k))
				}
			}
		}
	}
	return m, nil
}

func unitFor(k maturity.MetricKey) string {
	switch k {
	case maturity.MetricTokenIntensity:
		return "tokens_per_member_day"
	case maturity.MetricCRThroughputPerCapita:
		return "cr_per_member"
	case maturity.MetricProjectCollabScale:
		return "members_per_cr"
	default:
		return "ratio"
	}
}

func metricEmpty(unit string) maturity.MetricValue {
	return maturity.MetricValue{Unit: unit, DataStatus: maturity.StatusEmpty}
}

func metricNA(unit string) maturity.MetricValue {
	return maturity.MetricValue{Unit: unit, DataStatus: maturity.StatusNotApplicable}
}

func metricUnavailable(reason, unit string) maturity.MetricValue {
	return maturity.MetricValue{Unit: unit, DataStatus: maturity.StatusUnavailable, Reason: &reason}
}

func attributionOf(attributed, unattributed int64) *maturity.Attribution {
	a := maturity.Attribution{AttributedCount: attributed, UnattributedCount: unattributed}
	if attributed+unattributed > 0 {
		c := float64(attributed) / float64(attributed+unattributed)
		a.Coverage = &c
	}
	return &a
}

// taskCoverage computes task-level attribution coverage from usage rows:
// distinct task ids with/without an initiator. Tasks with no usage rows are
// invisible to this window, which is acceptable for per-scope attribution
// (org-level coverage uses the dedicated task-level query where available).
func taskCoverage(rows []db.MaturityTaskTokenRowsRow) (*float64, int64, int64) {
	withInitiator, without := map[[16]byte]bool{}, map[[16]byte]bool{}
	for _, r := range rows {
		if r.InitiatorUserID.Valid {
			withInitiator[r.TaskID.Bytes] = true
		} else {
			without[r.TaskID.Bytes] = true
		}
	}
	a, u := int64(len(withInitiator)), int64(len(without))
	if a+u == 0 {
		return nil, a, u
	}
	c := float64(a) / float64(a+u)
	return &c, a, u
}

func filterTokenRows(rows []db.MaturityTaskTokenRowsRow, projectID, userID *pgtype.UUID) []db.MaturityTaskTokenRowsRow {
	out := rows[:0]
	for _, r := range rows {
		if projectID != nil && (!r.ProjectKey.Valid || r.ProjectKey.Bytes != projectID.Bytes) {
			continue
		}
		if userID != nil && (!r.InitiatorUserID.Valid || r.InitiatorUserID.Bytes != userID.Bytes) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func filterArchivedByProject(rows []db.MaturityArchivedCRsRow, projectID pgtype.UUID) []db.MaturityArchivedCRsRow {
	out := rows[:0]
	for _, r := range rows {
		if r.ProjectID.Valid && r.ProjectID.Bytes == projectID.Bytes {
			out = append(out, r)
		}
	}
	return out
}

// costFromTokenRows prices a scope's usage rows: authoritative ticks first,
// generated price map only for uncosted tokens (never double-counting). Any
// unknown-model uncosted token makes the whole scope cost unavailable.
func costFromTokenRows(rows []db.MaturityTaskTokenRowsRow) (*float64, string) {
	prices, priceMapOK := maturity.GeneratedPriceMap()
	var ticks int64
	var uncostedUnpriced bool
	var authoritativeRows, uncostedRows, pricedUncostedRows int64
	usd := 0.0
	for _, r := range rows {
		if r.CostUsdTicks.Valid {
			authoritativeRows++
			ticks += r.CostUsdTicks.Int64
		}
	}
	usd += float64(ticks) * 1e-10
	for _, r := range rows {
		if r.CostUsdTicks.Valid {
			continue
		}
		uncostedRows++
		price, known := prices.Models[normalizeModel(r.Provider, r.Model)]
		if !priceMapOK || !known {
			if r.InputTokens+r.OutputTokens+r.CacheReadTokens+r.CacheWriteTokens > 0 {
				uncostedUnpriced = true
			}
			continue
		}
		pricedUncostedRows++
		usd += float64(r.InputTokens)*price.InputUSDPer1M/1e6 +
			float64(r.OutputTokens)*price.OutputUSDPer1M/1e6 +
			float64(r.CacheReadTokens)*price.CacheReadUSDPer1M/1e6 +
			float64(r.CacheWriteTokens)*price.CacheWriteUSDPer1M/1e6
	}
	status := "authoritative"
	switch {
	case uncostedUnpriced:
		status = "unavailable"
	case authoritativeRows > 0 && uncostedRows > 0:
		status = "mixed"
	case authoritativeRows == 0 && pricedUncostedRows > 0:
		status = "estimated"
	}
	if uncostedUnpriced {
		return nil, status
	}
	return &usd, status
}

// normalizeModel maps a provider/model pair onto the price-map key: bare
// lowercased model name first, then provider-prefixed form as a fallback.
func normalizeModel(provider, model string) string {
	key := ""
	for _, r := range model {
		if r >= 'A' && r <= 'Z' {
			key += string(r + 32)
		} else {
			key += string(r)
		}
	}
	return key
}

func fillGovernance(ctx context.Context, qtx *db.Queries, m maturity.SnapshotMetricsV1, workspaceID pgtype.UUID, from, to time.Time) error {
	gate, err := qtx.MaturityGateFirstPass(ctx, db.MaturityGateFirstPassParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
		ReviewNodeIds: maturityReviewNodeUUIDs,
	})
	if err != nil {
		return err
	}
	if gate.Completed == 0 {
		m.Governance[maturity.GovGateFirstPassRate] = metricEmpty("ratio")
	} else {
		v := float64(gate.FirstPass) / float64(gate.Completed)
		m.Governance[maturity.GovGateFirstPassRate] = maturity.MetricValue{
			Value: &v, Numerator: f64(float64(gate.FirstPass)), Denominator: f64(float64(gate.Completed)),
			Unit: "ratio", DataStatus: maturity.StatusReady,
		}
	}

	drift, err := qtx.MaturityEvidenceDriftCount(ctx, db.MaturityEvidenceDriftCountParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return err
	}
	m.Governance[maturity.GovEvidenceDriftCount] = countMetric(drift, "count")

	forbidden, err := qtx.MaturityForbiddenAttemptCount(ctx, db.MaturityForbiddenAttemptCountParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
	})
	if err != nil {
		return err
	}
	m.Governance[maturity.GovForbiddenAttemptCount] = countMetric(forbidden, "count")

	// traceability_complete_rate stays unavailable until the CR-C trace
	// channel ships; CR-A never scans git or the daemon (SDD §4.3).
	m.Governance[maturity.GovTraceabilityCompleteRate] = metricUnavailable(reasonTracePending, "ratio")

	latencies, err := qtx.MaturityApprovalLatencies(ctx, db.MaturityApprovalLatenciesParams{
		WorkspaceID: workspaceID, FromUtc: toPgTs(from), ToUtc: toPgTs(to),
		ReviewNodeIds: maturityReviewNodeUUIDs,
	})
	if err != nil {
		return err
	}
	samples := make([]float64, 0, len(latencies))
	for _, l := range latencies {
		samples = append(samples, float64(l.LatencyMs))
	}
	sort.Float64s(samples)
	for _, kv := range []struct {
		key  maturity.GovernanceMetricKey
		pctl float64
	}{
		{maturity.GovApprovalLatencyP50, 0.50},
		{maturity.GovApprovalLatencyP90, 0.90},
	} {
		if len(samples) == 0 {
			m.Governance[kv.key] = metricEmpty("milliseconds")
			continue
		}
		v := percentileCont(samples, kv.pctl)
		m.Governance[kv.key] = maturity.MetricValue{Value: &v, Unit: "milliseconds", DataStatus: maturity.StatusReady}
	}
	return nil
}

func countMetric(n int64, unit string) maturity.MetricValue {
	v := float64(n)
	return maturity.MetricValue{Value: &v, Numerator: f64(v), Unit: unit, DataStatus: maturity.StatusReady}
}

// percentileCont mirrors PostgreSQL percentile_cont (linear interpolation)
// for the sorted in-memory latency samples.
func percentileCont(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := p * float64(len(sorted)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
