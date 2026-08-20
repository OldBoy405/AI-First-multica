package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Metric helpers are kept separate from the transaction orchestration so the
// rollup entry point stays reviewable (CR-2026-047 code-review attempt 1).

func scoringInputsReady(values map[maturity.MetricKey]maturity.MetricValue) bool {
	for _, key := range maturity.AllMetricKeys {
		value, ok := values[key]
		if !ok || value.DataStatus != maturity.StatusReady || value.Value == nil {
			return false
		}
	}
	return true
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
		price, known := modelPrice(prices, r.Provider, r.Model)
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

// modelPrice resolves provider/model first, then falls back to the declared
// bare model key. This lets provider-specific prices override a shared model
// name without double-pricing authoritative task_usage ticks.
func modelPrice(prices maturity.PriceMap, provider, model string) (maturity.ModelPrice, bool) {
	model = strings.ToLower(model)
	if price, ok := prices.Models[strings.ToLower(provider)+"/"+model]; ok {
		return price, true
	}
	price, ok := prices.Models[model]
	return price, ok
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
