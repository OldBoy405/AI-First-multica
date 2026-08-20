package drift

// AIFIRST: CR-2026-049 TASK-11 — drift overview health (SDD §3.6/§4.2).
// Health reads sys_cron_executions only: the latest plan (any status) decides
// failed, the latest SUCCESS result decides uninitialized/stale/ok (config_rev
// drift and cursor coverage are validated against the committed declaration).
// Counts include open|acknowledged; resolve latency only resolved rows with
// resolved_at, empty sample → null p50/p90.

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/multica-ai/multica/server/internal/commitprefix"
)

// OverviewDTO is the GET /api/drift/overview response (SDD §3.6).
type OverviewDTO struct {
	V                int        `json:"v"`
	ScanHealth       string     `json:"scan_health"`
	LastPlanStatus   string     `json:"last_plan_status"`
	LastSuccessAt    *string    `json:"last_success_at"`
	RepositoryIDs    []string   `json:"repository_ids"`
	BypassCount      int64      `json:"bypass_count"`
	WipOnTrunkCount  int64      `json:"wip_on_trunk_count"`
	ResolveLatencyMS *LatencyMS `json:"resolve_latency_ms"`
}

type LatencyMS struct {
	SampleCount int      `json:"sample_count"`
	P50         *float64 `json:"p50"`
	P90         *float64 `json:"p90"`
}

// HealthState is the pure six-state decision (SDD §3.6); inputs come from the
// scheduler audit trail and the committed declaration.
func HealthState(now time.Time, configured bool, latestPlan *PlanRecord, latestSuccess *PlanRecord, configRev string, declRepoIDs []string) string {
	if !configured {
		return "not_configured"
	}
	if latestSuccess == nil {
		if latestPlan != nil && latestPlan.Status == "FAILED" {
			return "failed"
		}
		return "uninitialized"
	}
	if latestPlan != nil && latestPlan.Status == "FAILED" && latestPlan.StartedAt.After(latestSuccess.StartedAt) {
		return "failed"
	}
	if latestPlan != nil && latestPlan.Status == "RUNNING" {
		return "ok" // an in-flight plan is not a failure; the success result still governs
	}
	if now.Sub(latestSuccess.StartedAt) > 2*time.Hour {
		return "stale"
	}
	rv, ok := DecodeResultV1(latestSuccess.Result)
	if !ok || rv.ConfigRev != configRev {
		return "stale"
	}
	for _, id := range declRepoIDs {
		if rv.ScanCursors[id] == "" {
			return "stale"
		}
	}
	return "ok"
}

// PlanRecord is the minimal sys_cron_executions row the health needs.
type PlanRecord struct {
	Status    string
	StartedAt time.Time
	Result    map[string]any
}

// OverviewRepo performs the health + counts + latency queries.
type OverviewRepo struct {
	pool Querier
}

func NewOverviewRepo(pool Querier) *OverviewRepo { return &OverviewRepo{pool: pool} }

// WorkspaceConfigured reports whether workspace.repos contains the canonical
// knowledge-base URL (same eligibility rule as the scan scope provider).
func (r *OverviewRepo) WorkspaceConfigured(ctx context.Context, workspaceID string) (bool, error) {
	var reposRaw []byte
	if err := r.pool.QueryRow(ctx, `SELECT repos FROM workspace WHERE id = $1::uuid`, workspaceID).Scan(&reposRaw); err != nil {
		return false, err
	}
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(reposRaw, &repos); err != nil {
		return false, nil // malformed repos: not configured, not an error
	}
	kb := commitprefix.GeneratedPrefixes()["ai-first-platform-docs"].CanonicalURL
	for _, repo := range repos {
		if canonicalURL(repo.URL) == kb {
			return true, nil
		}
	}
	return false, nil
}

// canonicalURL normalizes an http(s)/ssh GitHub URL to the canonical HTTPS form.
func canonicalURL(raw string) string {
	u := raw
	if len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	if len(u) > 4 && u[len(u)-4:] == ".git" {
		u = u[:len(u)-4]
	}
	const ssh = "git@github.com:"
	if len(u) >= len(ssh) && u[:len(ssh)] == ssh {
		return "https://github.com/" + u[len(ssh):] + ".git"
	}
	const https = "https://github.com/"
	if len(u) >= len(https) && u[:len(https)] == https {
		return u + ".git"
	}
	return u
}

func (r *OverviewRepo) latestPlan(ctx context.Context, workspaceID, status string) (*PlanRecord, error) {
	query := `SELECT status, COALESCE(started_at, created_at), result
		FROM sys_cron_executions
		WHERE job_name = 'commit_prefix_scan' AND scope_kind = 'workspace' AND scope_id = $1`
	args := []any{workspaceID}
	if status != "" {
		query += ` AND status = $2`
		args = append(args, status)
	}
	query += ` ORDER BY started_at DESC LIMIT 1`
	var rec PlanRecord
	var resultRaw []byte
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&rec.Status, &rec.StartedAt, &resultRaw); err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	_ = json.Unmarshal(resultRaw, &rec.Result)
	return &rec, nil
}

// Overview assembles the overview DTO for one workspace.
func (r *OverviewRepo) Overview(ctx context.Context, workspaceID string, now time.Time) (*OverviewDTO, error) {
	configured, err := r.WorkspaceConfigured(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	latestPlan, err := r.latestPlan(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	latestSuccess, err := r.latestPlan(ctx, workspaceID, "SUCCESS")
	if err != nil {
		return nil, err
	}
	decls := commitprefix.GeneratedPrefixes()
	declIDs := make([]string, 0, len(decls))
	for id := range decls {
		declIDs = append(declIDs, id)
	}
	sort.Strings(declIDs)

	health := HealthState(now, configured, latestPlan, latestSuccess, commitprefix.GeneratedConfigRev(), declIDs)

	out := &OverviewDTO{
		V:                1,
		ScanHealth:       health,
		LastPlanStatus:   "",
		RepositoryIDs:    []string{},
		ResolveLatencyMS: &LatencyMS{},
	}
	if latestPlan != nil {
		out.LastPlanStatus = latestPlan.Status
	}
	if latestSuccess != nil {
		at := latestSuccess.StartedAt.Format(time.RFC3339)
		out.LastSuccessAt = &at
		if rv, ok := DecodeResultV1(latestSuccess.Result); ok {
			out.RepositoryIDs = rv.RepositoryIDs
		}
	}

	// Counts: open|acknowledged only (SDD §3.6).
	rows, err := r.pool.Query(ctx, `
		SELECT kind, count(*) FROM drift_finding
		WHERE workspace_id = $1::uuid AND status IN ('open','acknowledged')
		GROUP BY kind`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		switch kind {
		case "bypass-commit":
			out.BypassCount = n
		case "wip-on-trunk":
			out.WipOnTrunkCount = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Resolve latency: resolved rows with resolved_at; p50/p90 in ms.
	latRows, err := r.pool.Query(ctx, `
		SELECT extract(epoch FROM (resolved_at - found_at)) * 1000
		FROM drift_finding
		WHERE workspace_id = $1::uuid AND status = 'resolved' AND resolved_at IS NOT NULL
		ORDER BY 1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer latRows.Close()
	var samples []float64
	for latRows.Next() {
		var ms float64
		if err := latRows.Scan(&ms); err != nil {
			return nil, err
		}
		samples = append(samples, ms)
	}
	if err := latRows.Err(); err != nil {
		return nil, err
	}
	out.ResolveLatencyMS.SampleCount = len(samples)
	if len(samples) > 0 {
		p50 := percentile(samples, 0.5)
		p90 := percentile(samples, 0.9)
		out.ResolveLatencyMS.P50 = &p50
		out.ResolveLatencyMS.P90 = &p90
	} else {
		out.ResolveLatencyMS.P50 = nil
		out.ResolveLatencyMS.P90 = nil
	}
	return out, nil
}

func percentile(sorted []float64, q float64) float64 {
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}
