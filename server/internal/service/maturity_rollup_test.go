package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestPreviousLocalDate(t *testing.T) {
	// plan at 2026-08-20 00:30 +08 -> previous local day 2026-08-19.
	plan := time.Date(2026, 8, 20, 0, 30, 0, 0, shanghaiLoc)
	got := previousLocalDate(plan)
	want := time.Date(2026, 8, 19, 0, 0, 0, 0, shanghaiLoc)
	if !got.Equal(want) {
		t.Fatalf("previousLocalDate = %v, want %v", got, want)
	}
}

func TestDayWindowUTC(t *testing.T) {
	d := time.Date(2026, 8, 19, 0, 0, 0, 0, shanghaiLoc)
	from, to := dayWindowUTC(d)
	wantFrom := time.Date(2026, 8, 18, 16, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("window = [%v, %v), want [%v, %v)", from, to, wantFrom, wantTo)
	}
}

func TestValidateSnapshotMetrics(t *testing.T) {
	all := maturity.SnapshotMetricsV1{
		Schema:       maturityMetricsSchema,
		MetricValues: map[maturity.MetricKey]maturity.MetricValue{},
		Governance:   map[maturity.GovernanceMetricKey]maturity.MetricValue{},
	}
	for _, k := range maturity.AllMetricKeys {
		all.MetricValues[k] = maturity.MetricValue{DataStatus: maturity.StatusReady}
	}
	for _, k := range maturity.AllGovernanceKeys {
		all.Governance[k] = maturity.MetricValue{DataStatus: maturity.StatusNotApplicable}
	}
	if err := ValidateSnapshotMetrics(all); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}

	clone := func() maturity.SnapshotMetricsV1 {
		c := maturity.SnapshotMetricsV1{
			Schema:       all.Schema,
			MetricValues: make(map[maturity.MetricKey]maturity.MetricValue, 8),
			Governance:   make(map[maturity.GovernanceMetricKey]maturity.MetricValue, 6),
		}
		for k, v := range all.MetricValues {
			c.MetricValues[k] = v
		}
		for k, v := range all.Governance {
			c.Governance[k] = v
		}
		return c
	}

	bad := clone()
	delete(bad.MetricValues, maturity.MetricTeamAgentDepth)
	if err := ValidateSnapshotMetrics(bad); err == nil {
		t.Fatal("missing metric key must fail")
	}

	badGov := clone()
	delete(badGov.Governance, maturity.GovApprovalLatencyP90)
	if err := ValidateSnapshotMetrics(badGov); err == nil {
		t.Fatal("missing governance key must fail")
	}

	ownerReady := clone()
	reason := reasonOwnerUnresolved
	ownerReady.MetricValues[maturity.MetricProjectCollabScale] = maturity.MetricValue{
		DataStatus: maturity.StatusReady, Reason: &reason,
	}
	if err := ValidateSnapshotMetrics(ownerReady); err == nil {
		t.Fatal("owner-unresolved reason on a ready metric must fail")
	}

	ownerUnavailable := clone()
	ownerUnavailable.MetricValues[maturity.MetricProjectCollabScale] = maturity.MetricValue{
		DataStatus: maturity.StatusUnavailable, Reason: &reason,
	}
	if err := ValidateSnapshotMetrics(ownerUnavailable); err != nil {
		t.Fatalf("owner-unresolved unavailable payload rejected: %v", err)
	}
}

func TestPercentileCont(t *testing.T) {
	cases := []struct {
		samples []float64
		p, want float64
	}{
		{[]float64{1, 2, 3, 4}, 0.50, 2.5},
		{[]float64{1, 2, 3, 4}, 0.90, 3.7},
		{[]float64{5}, 0.50, 5},
		{[]float64{}, 0.50, 0},
		{[]float64{100, 200}, 0.50, 150},
	}
	for _, tc := range cases {
		if got := percentileCont(tc.samples, tc.p); got != tc.want {
			t.Errorf("percentileCont(%v, %v) = %v, want %v", tc.samples, tc.p, got, tc.want)
		}
	}
}

func TestCostFromTokenRows(t *testing.T) {
	mk := func(ticks *int64, in, out int64, model string) db.MaturityTaskTokenRowsRow {
		r := db.MaturityTaskTokenRowsRow{
			Provider: "openai", Model: model, InputTokens: in, OutputTokens: out,
		}
		if ticks != nil {
			r.CostUsdTicks = pgtype.Int8{Int64: *ticks, Valid: true}
		}
		return r
	}
	// No price map committed: any uncosted token -> unavailable.
	rows := []db.MaturityTaskTokenRowsRow{
		mk(i64p(1000), 100, 200, "gpt-5.6"),
		mk(nil, 50, 0, "gpt-5.6"),
	}
	usd, status := costFromTokenRows(rows)
	if usd != nil || status != "unavailable" {
		t.Fatalf("uncosted without price map: got (%v, %s), want (nil, unavailable)", usd, status)
	}

	// All authoritative: ticks sum * 1e-10.
	rows = []db.MaturityTaskTokenRowsRow{mk(i64p(1000), 1, 1, "gpt-5.6")}
	usd, status = costFromTokenRows(rows)
	if usd == nil || status != "authoritative" || *usd < 0.999e-7 || *usd > 1.001e-7 {
		t.Fatalf("authoritative: got (%v, %s), want (~1e-7, authoritative)", usd, status)
	}
}

func i64p(v int64) *int64 { return &v }

func TestTaskCoverage(t *testing.T) {
	u1 := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	task1 := pgtype.UUID{Bytes: [16]byte{10}, Valid: true}
	task2 := pgtype.UUID{Bytes: [16]byte{11}, Valid: true}
	rows := []db.MaturityTaskTokenRowsRow{
		{TaskID: task1, InitiatorUserID: u1},
		{TaskID: task1, InitiatorUserID: u1},
		{TaskID: task2},
	}
	cov, a, u := taskCoverage(rows)
	if cov == nil || *cov != 0.5 || a != 1 || u != 1 {
		t.Fatalf("coverage = (%v, %d, %d), want (0.5, 1, 1)", cov, a, u)
	}
}

func TestNormalizeModel(t *testing.T) {
	if got := normalizeModel("OpenAI", "GPT-5.6"); got != "gpt-5.6" {
		t.Fatalf("normalizeModel = %q", got)
	}
}
