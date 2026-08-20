package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MaturityService is the read side of the maturity dashboard. It only reads
// frozen snapshot rows and report envelopes — historical scores are never
// recomputed from raw events (SDD §1.2).
type MaturityService struct {
	queries *db.Queries
	cfg     maturity.ConfigV1
	prices  maturity.PriceMap
}

var errMaturityInvalidQuery = errors.New("invalid maturity query")

// IsMaturityInvalidQuery distinguishes client input failures from storage or
// corrupted-projection failures, which must remain server errors.
func IsMaturityInvalidQuery(err error) bool {
	return errors.Is(err, errMaturityInvalidQuery)
}

func invalidMaturityQuery(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errMaturityInvalidQuery, fmt.Sprintf(format, args...))
}

// NewMaturityService wires the read service with the generated config copy.
func NewMaturityService(queries *db.Queries, cfg maturity.ConfigV1, prices maturity.PriceMap) *MaturityService {
	return &MaturityService{queries: queries, cfg: cfg, prices: prices}
}

// Overall returns the latest org bucket (or the requested date) with scores.
func (s *MaturityService) Overall(ctx context.Context, workspaceID pgtype.UUID, date *time.Time) (*maturity.MaturityOverallResponse, error) {
	var row db.MaturitySnapshot
	var err error
	if date != nil {
		row, err = s.queries.GetMaturitySnapshot(ctx, db.GetMaturitySnapshotParams{
			WorkspaceID: workspaceID,
			BucketDate:  pgtype.Date{Time: *date, Valid: true},
			Scope:       scopeOrg,
			ScopeID:     orgSentinel,
		})
	} else {
		row, err = s.queries.LatestMaturitySnapshot(ctx, db.LatestMaturitySnapshotParams{
			WorkspaceID: workspaceID, Scope: scopeOrg, ScopeID: orgSentinel,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &maturity.MaturityOverallResponse{DataStatus: "empty", Dimensions: []maturity.MaturityDimension{}, Governance: []maturity.MaturityGovernanceDatum{}}, nil
		}
		return nil, err
	}

	metrics, err := decodeSnapshotMetrics(row.Metrics)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot metrics: %w", err)
	}
	scores, err := decodeSnapshotScores(row.Scores)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot scores: %w", err)
	}

	resp := &maturity.MaturityOverallResponse{
		BucketDate: row.BucketDate.Time.Format("2006-01-02"),
		ConfigRev:  row.ConfigRev,
		Headline:   &metrics.Headline,
		Dimensions: []maturity.MaturityDimension{},
		Governance: []maturity.MaturityGovernanceDatum{},
		DataStatus: "ready",
	}
	for _, k := range maturity.AllGovernanceKeys {
		resp.Governance = append(resp.Governance, maturity.MaturityGovernanceDatum{Key: k, Datum: metrics.Governance[k]})
	}
	if len(scores.MetricScores) == 0 {
		// observation or unavailable scope: all scores null
		for _, dim := range []maturity.DimensionKey{maturity.DimAIF, maturity.DimSII, maturity.DimOFI, maturity.DimEPC, maturity.DimACM} {
			d := maturity.MaturityDimension{Key: dim, Score: nil, DataStatus: "empty", Metrics: []maturity.MaturityDimensionMetric{}}
			for _, mk := range s.cfg.Dimensions[dim] {
				d.Metrics = append(d.Metrics, maturity.MaturityDimensionMetric{Key: mk, Raw: metrics.MetricValues[mk], Score: nil})
			}
			resp.Dimensions = append(resp.Dimensions, d)
		}
	} else {
		total := scores.TotalScore
		resp.TotalScore = &total
		for _, dim := range []maturity.DimensionKey{maturity.DimAIF, maturity.DimSII, maturity.DimOFI, maturity.DimEPC, maturity.DimACM} {
			ds := scores.DimensionScores[dim]
			d := maturity.MaturityDimension{Key: dim, Score: &ds, DataStatus: "ready", Metrics: []maturity.MaturityDimensionMetric{}}
			for _, mk := range s.cfg.Dimensions[dim] {
				ms := scores.MetricScores[mk]
				d.Metrics = append(d.Metrics, maturity.MaturityDimensionMetric{Key: mk, Raw: metrics.MetricValues[mk], Score: &ms})
			}
			resp.Dimensions = append(resp.Dimensions, d)
		}
	}

	first, err := s.queries.MaturitySnapshotFirstBucket(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load first maturity bucket: %w", err)
	}
	if first.Valid {
		elapsed := int(row.BucketDate.Time.Sub(first.Time).Hours() / 24)
		resp.Observation = &maturity.Observation{
			Active:            maturity.ObservationActive(first.Time, time.Now(), s.cfg),
			CalibrationStatus: s.cfg.CalibrationStatus,
			ObservationWeeks:  s.cfg.ObservationWeeks,
			FirstBucketDate:   first.Time.Format("2006-01-02"),
			ElapsedDays:       elapsed,
		}
	}
	return resp, nil
}

// TokenTrend returns dated token series per dimension. Project/user series
// read frozen snapshots; model series reads raw usage (no scores).
func (s *MaturityService) TokenTrend(ctx context.Context, workspaceID pgtype.UUID, req maturity.TokenTrendQuery) (*maturity.TokenTrendResponse, error) {
	from := toPgDate(req.From)
	to := toPgDate(req.To)
	resp := &maturity.TokenTrendResponse{
		Dimension: req.Dimension, From: req.From.Format("2006-01-02"), To: req.To.Format("2006-01-02"),
		Series: []maturity.TokenTrendSeries{}, DataStatus: "empty",
	}

	switch req.Dimension {
	case "model":
		rows, err := s.queries.MaturityModelCostRows(ctx, db.MaturityModelCostRowsParams{
			WorkspaceID: workspaceID,
			FromUtc:     pgtype.Timestamptz{Time: req.From, Valid: true},
			ToUtc:       pgtype.Timestamptz{Time: req.To.AddDate(0, 0, 1), Valid: true},
		})
		if err != nil {
			return nil, err
		}
		series := map[string]*maturity.TokenTrendSeries{}
		for _, r := range rows {
			key := r.Provider + ":" + r.Model
			if req.DimensionID != "" && r.Model != req.DimensionID && key != req.DimensionID {
				continue
			}
			ser := series[key]
			if ser == nil {
				ser = &maturity.TokenTrendSeries{ID: key, Label: r.Model, Points: []maturity.TokenTrendPoint{}}
				series[key] = ser
			}
			var cost *float64
			status := "unavailable"
			if req.IncludeCost {
				cost, status = costFromModelRow(r)
			}
			ser.Points = append(ser.Points, maturity.TokenTrendPoint{
				Date:    r.BucketDate.Time.Format("2006-01-02"),
				Tokens:  r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens,
				CostUSD: cost, CostStatus: status,
			})
		}
		for _, ser := range series {
			resp.Series = append(resp.Series, *ser)
		}
		sort.Slice(resp.Series, func(i, j int) bool { return resp.Series[i].ID < resp.Series[j].ID })
	case "user", "project":
		scope := scopeProject
		scopeID := req.DimensionID
		if req.Dimension == "user" {
			scope = scopeUser
		}
		var rows []db.MaturitySnapshot
		if scopeID != "" {
			r, err := s.queries.ListMaturitySnapshots(ctx, db.ListMaturitySnapshotsParams{
				WorkspaceID: workspaceID, Scope: scope, ScopeID: scopeID, BucketDate: from, BucketDate_2: to, Limit: 366,
			})
			if err != nil {
				return nil, err
			}
			rows = r
		} else {
			// all projects: read every project row in range
			r, err := s.queries.ListMaturitySnapshotsByScope(ctx, db.ListMaturitySnapshotsByScopeParams{
				WorkspaceID: workspaceID, Scope: scopeProject, BucketDate: from, BucketDate_2: to, Limit: 10000,
			})
			if err != nil {
				return nil, err
			}
			rows = r
		}
		series := map[string]*maturity.TokenTrendSeries{}
		names := map[string]string{}
		if req.Dimension == "project" {
			projects, err := s.queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID})
			if err != nil {
				return nil, fmt.Errorf("list maturity trend projects: %w", err)
			}
			for _, p := range projects {
				names[uuid.UUID(p.ID.Bytes).String()] = p.Title
			}
		}
		for _, r := range rows {
			m, err := decodeSnapshotMetrics(r.Metrics)
			if err != nil {
				return nil, fmt.Errorf("decode maturity trend metrics for %s/%s: %w", r.Scope, r.ScopeID, err)
			}
			key := r.ScopeID
			label := key
			if n, ok := names[key]; ok && n != "" {
				label = n
			}
			ser := series[key]
			if ser == nil {
				ser = &maturity.TokenTrendSeries{ID: key, Label: label, Points: []maturity.TokenTrendPoint{}}
				series[key] = ser
			}
			pt := maturity.TokenTrendPoint{
				Date: r.BucketDate.Time.Format("2006-01-02"), Tokens: m.Headline.TotalTokens,
				CostStatus: "unavailable", ConfigRev: r.ConfigRev,
			}
			if req.IncludeCost {
				pt.CostUSD = m.Headline.CostUSD
				pt.CostStatus = m.Headline.CostStatus
			}
			ser.Points = append(ser.Points, pt)
		}
		for _, ser := range series {
			sort.Slice(ser.Points, func(i, j int) bool { return ser.Points[i].Date < ser.Points[j].Date })
			resp.Series = append(resp.Series, *ser)
		}
		sort.Slice(resp.Series, func(i, j int) bool { return resp.Series[i].ID < resp.Series[j].ID })
	default:
		return nil, invalidMaturityQuery("unsupported dimension %q", req.Dimension)
	}
	if len(resp.Series) > 0 {
		resp.DataStatus = "ready"
	}
	return resp, nil
}

// Rankings returns project rankings for one bucket and metric.
func (s *MaturityService) Rankings(ctx context.Context, workspaceID pgtype.UUID, date *time.Time, metric string, limit int, cursor *string) (*maturity.ProjectRankingsResponse, error) {
	if metric != "total" {
		valid := false
		for _, key := range maturity.AllMetricKeys {
			if metric == string(key) {
				valid = true
				break
			}
		}
		if !valid {
			return nil, invalidMaturityQuery("unsupported ranking metric %q", metric)
		}
	}
	bucket := time.Now().In(shanghaiLoc).AddDate(0, 0, -1)
	if date != nil {
		bucket = *date
	}
	offset := 0
	if cursor != nil && *cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(*cursor)
		if err != nil {
			return nil, invalidMaturityQuery("invalid ranking cursor")
		}
		offset, err = strconv.Atoi(string(raw))
		if err != nil || offset < 0 {
			return nil, invalidMaturityQuery("invalid ranking cursor")
		}
	}
	rows, err := s.queries.ListMaturitySnapshotsByScope(ctx, db.ListMaturitySnapshotsByScopeParams{
		WorkspaceID: workspaceID, Scope: scopeProject, BucketDate: toPgDate(bucket), BucketDate_2: toPgDate(bucket), Limit: 10000,
	})
	if err != nil {
		return nil, err
	}
	items := make([]maturity.ProjectRankingsItem, 0, len(rows))
	names := map[string]string{}
	projects, err := s.queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID})
	if err != nil {
		return nil, fmt.Errorf("list maturity ranking projects: %w", err)
	}
	for _, p := range projects {
		names[uuid.UUID(p.ID.Bytes).String()] = p.Title
	}
	for _, r := range rows {
		m, err := decodeSnapshotMetrics(r.Metrics)
		if err != nil {
			return nil, fmt.Errorf("decode maturity ranking metrics for %s: %w", r.ScopeID, err)
		}
		scores, err := decodeSnapshotScores(r.Scores)
		if err != nil {
			return nil, fmt.Errorf("decode maturity ranking scores for %s: %w", r.ScopeID, err)
		}
		item := maturity.ProjectRankingsItem{ProjectID: r.ScopeID, ProjectName: r.ScopeID}
		if n, ok := names[r.ScopeID]; ok && n != "" {
			item.ProjectName = n
		}
		if metric == "total" {
			if len(scores.MetricScores) == 0 {
				item.DataStatus = maturity.StatusUnavailable
			} else {
				v := scores.TotalScore
				item.Value = &v
				item.DataStatus = maturity.StatusReady
			}
		} else {
			mk := maturity.MetricKey(metric)
			mv, ok := m.MetricValues[mk]
			if !ok || mv.Value == nil {
				item.DataStatus = mv.DataStatus
			} else {
				v := *mv.Value
				item.Value = &v
				item.DataStatus = mv.DataStatus
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Value == nil && items[j].Value == nil {
			return items[i].ProjectID < items[j].ProjectID
		}
		if items[i].Value == nil {
			return false
		}
		if items[j].Value == nil {
			return true
		}
		return *items[i].Value > *items[j].Value
	})
	total := len(items)
	if offset >= total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := items[offset:end]
	for i := range page {
		page[i].Rank = offset + i + 1
	}
	nextCursor := ""
	if end < total {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	status := "ready"
	if len(items) == 0 {
		status = "empty"
	}
	return &maturity.ProjectRankingsResponse{
		Scope: "project", BucketDate: bucket.Format("2006-01-02"), Metric: metric,
		Items: page, NextCursor: nextCursor, DataStatus: status,
	}, nil
}

// Suggestions returns the latest valid weekly report envelope.
func (s *MaturityService) Suggestions(ctx context.Context, workspaceID pgtype.UUID) (*maturity.SuggestionResponse, error) {
	projectID, err := s.queries.MaturityOrgAdminProjectID(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !projectID.Valid) {
		return &maturity.SuggestionResponse{DataStatus: "empty"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Org Admin project: %w", err)
	}
	row, err := s.queries.MaturityReportLatest(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &maturity.SuggestionResponse{DataStatus: "empty"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest maturity report: %w", err)
	}
	report, ok := decodeReport(row.Result)
	if !ok {
		return nil, errors.New("decode latest maturity report: corrupt envelope or digest")
	}
	return &maturity.SuggestionResponse{Latest: &report, DataStatus: "ready"}, nil
}

// SuggestionHistory lists report envelopes newest-first with a stable report-key cursor.
func (s *MaturityService) SuggestionHistory(ctx context.Context, workspaceID pgtype.UUID, limit int, cursor *string) (*maturity.SuggestionHistoryResponse, error) {
	before := ""
	if cursor != nil && *cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(*cursor)
		if err != nil || len(raw) == 0 {
			return nil, invalidMaturityQuery("invalid report cursor")
		}
		before = string(raw)
	}
	projectID, err := s.queries.MaturityOrgAdminProjectID(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !projectID.Valid) {
		return &maturity.SuggestionHistoryResponse{Items: []maturity.MaturityReport{}, DataStatus: "empty"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Org Admin project: %w", err)
	}
	rows, err := s.queries.MaturityReportHistory(ctx, db.MaturityReportHistoryParams{
		ProjectID: projectID, Schema: maturityReportSchema,
		BeforeReportKey: before, PageLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("load maturity report history: %w", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]maturity.MaturityReport, 0, len(rows))
	for _, r := range rows {
		report, ok := decodeReport(r.Result)
		if !ok {
			return nil, errors.New("decode maturity report history: corrupt envelope or digest")
		}
		items = append(items, report)
	}
	nextCursor := ""
	if hasMore && len(items) > 0 {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(items[len(items)-1].ReportKey))
	}
	return &maturity.SuggestionHistoryResponse{Items: items, NextCursor: nextCursor, DataStatus: dataStatusOf(items)}, nil
}

func decodeSnapshotMetrics(raw []byte) (maturity.SnapshotMetricsV1, error) {
	var metrics maturity.SnapshotMetricsV1
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return metrics, err
	}
	if err := ValidateSnapshotMetrics(metrics); err != nil {
		return metrics, err
	}
	return metrics, nil
}

func decodeSnapshotScores(raw []byte) (maturity.SnapshotScoresV1, error) {
	var scores maturity.SnapshotScoresV1
	if err := json.Unmarshal(raw, &scores); err != nil {
		return scores, err
	}
	if len(scores.MetricScores) == 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return scores, err
		}
		if len(object) != 0 {
			return scores, errors.New("score payload is neither empty observation data nor a full score projection")
		}
		return scores, nil
	}
	if scores.Schema != "ai-first.maturity-scores/v1" || len(scores.MetricScores) != len(maturity.AllMetricKeys) || len(scores.DimensionScores) != 5 {
		return scores, errors.New("incomplete maturity score projection")
	}
	for _, key := range maturity.AllMetricKeys {
		if _, ok := scores.MetricScores[key]; !ok {
			return scores, fmt.Errorf("missing maturity metric score %q", key)
		}
	}
	return scores, nil
}

func dataStatusOf(items []maturity.MaturityReport) string {
	if len(items) == 0 {
		return "empty"
	}
	return "ready"
}

// Config returns the generated config declaration (all members can read).
func (s *MaturityService) Config(ctx context.Context, workspaceID pgtype.UUID) (*maturity.MaturityConfigResponse, error) {
	resp := &maturity.MaturityConfigResponse{
		ConfigRev:           maturity.GeneratedConfigRev(),
		ObservationWeeks:    s.cfg.ObservationWeeks,
		CalibrationStatus:   s.cfg.CalibrationStatus,
		Dimensions:          []maturity.MaturityConfigDimension{},
		Metrics:             []maturity.MaturityConfigMetric{},
		BaselineSuggestions: []maturity.MaturityBaselineSuggestion{},
	}
	if _, ok := maturity.GeneratedPriceMap(); ok {
		rev := maturity.GeneratedConfigRev()
		resp.PriceConfigRev = &rev
	}
	for _, dim := range []maturity.DimensionKey{maturity.DimAIF, maturity.DimSII, maturity.DimOFI, maturity.DimEPC, maturity.DimACM} {
		resp.Dimensions = append(resp.Dimensions, maturity.MaturityConfigDimension{Key: dim, Metrics: s.cfg.Dimensions[dim]})
	}
	for _, k := range maturity.AllMetricKeys {
		mc := s.cfg.Metrics[k]
		resp.Metrics = append(resp.Metrics, maturity.MaturityConfigMetric{
			Key: k, Weight: mc.Weight, Floor: mc.Floor, Target: mc.Target,
			Unit: unitForRead(k), KnownGameability: knownGameability(k),
		})
	}
	baseline, err := s.queries.MaturityBaselinePercentiles(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for _, row := range baseline {
		resp.BaselineSuggestions = append(resp.BaselineSuggestions, maturity.MaturityBaselineSuggestion{
			MetricKey: maturity.MetricKey(row.MetricKey), SampleCount: row.SampleCount,
			FloorP10: row.P10, TargetP75: row.P75,
		})
	}
	return resp, nil
}

func knownGameability(k maturity.MetricKey) string {
	return map[maturity.MetricKey]string{
		maturity.MetricTokenIntensity:        "Can be inflated by verbose prompts or unnecessary generations.",
		maturity.MetricAIPenetration:         "Can be inflated by trivial one-off Agent tasks.",
		maturity.MetricCRThroughputPerCapita: "Can be inflated by splitting changes into undersized CRs.",
		maturity.MetricProjectCollabScale:    "Can be inflated by adding nominal participants without substantive work.",
		maturity.MetricProjectActiveRate:     "Any qualifying task or status change can mark a project active.",
		maturity.MetricPrototypeDirectRate:   "Can be inflated by pre-reviewing outside the recorded gate flow.",
		maturity.MetricTeamAgentDepth:        "Can be inflated by attaching shallow tasks to issues or CRs.",
		maturity.MetricProcessCompletionRate: "Can be inflated by mechanically completing pipelines without outcome quality.",
	}[k]
}

func unitForRead(k maturity.MetricKey) string {
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

// decodeReport parses one report envelope and rejects SHA-mismatched bodies.
func decodeReport(result []byte) (maturity.MaturityReport, bool) {
	var r maturity.MaturityReport
	if err := json.Unmarshal(result, &r); err != nil {
		return r, false
	}
	if r.Schema != maturityReportSchema || r.ReportKey == "" || r.ContentSha256 == "" {
		return r, false
	}
	sum := sha256.Sum256([]byte(r.Markdown))
	if hex.EncodeToString(sum[:]) != r.ContentSha256 {
		return r, false
	}
	return r, true
}

// costFromModelRow prices one model row exactly like the rollup does for
// task rows (authoritative ticks first, price map only for uncosted tokens).
func costFromModelRow(r db.MaturityModelCostRowsRow) (*float64, string) {
	prices, priceMapOK := maturity.GeneratedPriceMap()
	uncostedTokens := r.UncostedInputTokens + r.UncostedOutputTokens + r.UncostedCacheReadTokens + r.UncostedCacheWriteTokens
	usd := float64(r.CostUsdTicks) * 1e-10
	price, known := modelPrice(prices, r.Provider, r.Model)
	unpriced := uncostedTokens > 0 && (!priceMapOK || !known)
	if unpriced {
		return nil, "unavailable"
	}
	if known {
		usd += float64(r.UncostedInputTokens)*price.InputUSDPer1M/1e6 +
			float64(r.UncostedOutputTokens)*price.OutputUSDPer1M/1e6 +
			float64(r.UncostedCacheReadTokens)*price.CacheReadUSDPer1M/1e6 +
			float64(r.UncostedCacheWriteTokens)*price.CacheWriteUSDPer1M/1e6
	}
	status := "authoritative"
	switch {
	case r.AuthoritativeRows > 0 && uncostedTokens > 0:
		status = "mixed"
	case r.AuthoritativeRows == 0 && uncostedTokens > 0:
		status = "estimated"
	}
	return &usd, status
}
