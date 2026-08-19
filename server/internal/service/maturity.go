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
	"strings"
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
		rows, listErr := s.queries.ListMaturitySnapshots(ctx, db.ListMaturitySnapshotsParams{
			WorkspaceID: workspaceID, Scope: scopeOrg, ScopeID: orgSentinel,
			BucketDate:   pgtype.Date{Time: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
			BucketDate_2: pgtype.Date{Time: time.Now().Add(24 * time.Hour), Valid: true},
			Limit:        1,
		})
		if listErr != nil {
			return nil, listErr
		}
		if len(rows) == 0 {
			err = pgx.ErrNoRows
		} else {
			row = rows[len(rows)-1]
		}
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &maturity.MaturityOverallResponse{DataStatus: "empty", Dimensions: []maturity.MaturityDimension{}, Governance: []maturity.MaturityGovernanceDatum{}}, nil
		}
		return nil, err
	}

	var metrics maturity.SnapshotMetricsV1
	if err := json.Unmarshal(row.Metrics, &metrics); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot metrics: %w", err)
	}
	var scores maturity.SnapshotScoresV1
	_ = json.Unmarshal(row.Scores, &scores)

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
	if err == nil && first.Valid {
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
			ToUtc:       pgtype.Timestamptz{Time: req.To, Valid: true},
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			key := r.Provider + ":" + r.Model
			if req.DimensionID != "" && r.Model != req.DimensionID && key != req.DimensionID {
				continue
			}
			cost, status := costFromModelRow(r)
			resp.Series = append(resp.Series, maturity.TokenTrendSeries{
				ID: key, Label: r.Model,
				Points: []maturity.TokenTrendPoint{{
					Date:       from.Time.Format("2006-01-02"),
					Tokens:     r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens,
					CostUSD:    cost, CostStatus: status,
				}},
			})
		}
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
			if projects, err := s.queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID}); err == nil {
				for _, p := range projects {
					names[uuid.UUID(p.ID.Bytes).String()] = p.Title
				}
			}
		}
		for _, r := range rows {
			var m maturity.SnapshotMetricsV1
			if err := json.Unmarshal(r.Metrics, &m); err != nil {
				continue
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
			pt := maturity.TokenTrendPoint{Date: r.BucketDate.Time.Format("2006-01-02"), Tokens: m.Headline.TotalTokens}
			pt.CostUSD = m.Headline.CostUSD
			pt.CostStatus = m.Headline.CostStatus
			ser.Points = append(ser.Points, pt)
		}
		for _, ser := range series {
			sort.Slice(ser.Points, func(i, j int) bool { return ser.Points[i].Date < ser.Points[j].Date })
			resp.Series = append(resp.Series, *ser)
		}
		sort.Slice(resp.Series, func(i, j int) bool { return resp.Series[i].ID < resp.Series[j].ID })
	default:
		return nil, fmt.Errorf("invalid_query: unsupported dimension %q", req.Dimension)
	}
	if len(resp.Series) > 0 {
		resp.DataStatus = "ready"
	}
	return resp, nil
}

// Rankings returns project rankings for one bucket and metric.
func (s *MaturityService) Rankings(ctx context.Context, workspaceID pgtype.UUID, date *time.Time, metric string, limit int, cursor *string) (*maturity.ProjectRankingsResponse, error) {
	bucket := time.Now().In(shanghaiLoc).AddDate(0, 0, -1)
	if date != nil {
		bucket = *date
	}
	offset := 0
	if cursor != nil && *cursor != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(*cursor); err == nil {
			offset, _ = strconv.Atoi(string(raw))
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
	if projects, err := s.queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID}); err == nil {
		for _, p := range projects {
			names[uuid.UUID(p.ID.Bytes).String()] = p.Title
		}
	}
	for _, r := range rows {
		var m maturity.SnapshotMetricsV1
		if err := json.Unmarshal(r.Metrics, &m); err != nil {
			continue
		}
		var scores maturity.SnapshotScoresV1
		_ = json.Unmarshal(r.Scores, &scores)
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
	if err != nil || !projectID.Valid {
		return &maturity.SuggestionResponse{DataStatus: "empty"}, nil
	}
	row, err := s.queries.MaturityReportLatest(ctx, projectID)
	if err != nil || len(row.Result) == 0 {
		return &maturity.SuggestionResponse{DataStatus: "empty"}, nil
	}
	report, ok := decodeReport(row.Result)
	if !ok {
		return &maturity.SuggestionResponse{DataStatus: "empty"}, nil
	}
	return &maturity.SuggestionResponse{Latest: &report, DataStatus: "ready"}, nil
}

// SuggestionHistory lists report envelopes newest-first (offset cursor).
func (s *MaturityService) SuggestionHistory(ctx context.Context, workspaceID pgtype.UUID, limit int, cursor *string) (*maturity.SuggestionHistoryResponse, error) {
	projectID, err := s.queries.MaturityOrgAdminProjectID(ctx, workspaceID)
	if err != nil || !projectID.Valid {
		return &maturity.SuggestionHistoryResponse{Items: []maturity.MaturityReport{}, DataStatus: "empty"}, nil
	}
	offset := 0
	if cursor != nil && *cursor != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(*cursor); err == nil {
			offset, _ = strconv.Atoi(string(raw))
		}
	}
	rows, err := s.queries.MaturityReportHistory(ctx, db.MaturityReportHistoryParams{
		ProjectID: projectID, Schema: "ai-first.maturity-report/v1", Limit: int32(limit + offset), Offset: 0,
	})
	if err != nil {
		return nil, err
	}
	items := []maturity.MaturityReport{}
	seen := map[string]bool{}
	for _, r := range rows {
		report, ok := decodeReport(r.Result)
		if !ok || seen[report.ReportKey] {
			continue
		}
		seen[report.ReportKey] = true
		items = append(items, report)
	}
	nextCursor := ""
	if len(rows) == limit+offset {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + len(rows))))
	}
	return &maturity.SuggestionHistoryResponse{Items: items, NextCursor: nextCursor, DataStatus: dataStatusOf(items)}, nil
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
		ConfigRev:         maturity.GeneratedConfigRev(),
		ObservationWeeks:  s.cfg.ObservationWeeks,
		CalibrationStatus: s.cfg.CalibrationStatus,
		Dimensions:        []maturity.MaturityConfigDimension{},
		Metrics:           []maturity.MaturityConfigMetric{},
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
			Key: k, Weight: mc.Weight, Floor: mc.Floor, Target: mc.Target, Unit: unitForRead(k), KnownGameability: "",
		})
	}
	return resp, nil
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
	if r.ReportKey == "" || r.ContentSha256 == "" {
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
	price, known := prices.Models[strings.ToLower(r.Model)]
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
