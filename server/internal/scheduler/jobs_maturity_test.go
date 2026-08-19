package scheduler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// shanghaiTime builds a wall-clock time in Asia/Shanghai.
func shanghaiTime(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, maturityShanghaiLoc)
}

func newMaturityTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMaturityPlansForScopeFirstStart(t *testing.T) {
	pool := newMaturityTestPool(t)
	q := db.New(pool)
	hook := maturityPlansForScope(q)
	ctx := context.Background()

	// First deployment: no stored plan rows -> only the most recent due plan.
	for _, tc := range []struct {
		now     time.Time
		wantDay int
	}{
		{shanghaiTime(2026, 8, 20, 0, 15, 0), 19}, // 00:15 -> previous day 00:30
		{shanghaiTime(2026, 8, 20, 0, 31, 0), 20}, // 00:31 -> same day 00:30
	} {
		plans, err := hook(ctx, ScopeGlobal, tc.now.UTC(), LatestPlanInfo{})
		if err != nil {
			t.Fatalf("hook: %v", err)
		}
		if len(plans) != 1 {
			t.Fatalf("now=%v: %d plans, want exactly 1", tc.now, len(plans))
		}
		if got := plans[0].In(maturityShanghaiLoc).Day(); got != tc.wantDay {
			t.Fatalf("now=%v: plan day = %d, want %d", tc.now, got, tc.wantDay)
		}
		if got := plans[0].In(maturityShanghaiLoc); got.Hour() != 0 || got.Minute() != 30 {
			t.Fatalf("now=%v: plan time = %v, want 00:30", tc.now, got)
		}
	}
}

func TestMaturityPlansForScopeCatchUp(t *testing.T) {
	pool := newMaturityTestPool(t)
	q := db.New(pool)
	hook := maturityPlansForScope(q)
	ctx := context.Background()

	// 3-day downtime: latest stored plan 3 days back -> 3 catch-up plans.
	latestPlan := shanghaiTime(2026, 8, 17, 0, 30, 0).UTC()
	now := shanghaiTime(2026, 8, 20, 0, 31, 0).UTC()
	plans, err := hook(ctx, ScopeGlobal, now, LatestPlanInfo{Found: true, PlanTime: latestPlan})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("3-day downtime: %d plans, want 3", len(plans))
	}
	for i, p := range plans {
		if got := p.In(maturityShanghaiLoc).Day(); got != 18+i {
			t.Fatalf("plan %d on day %d, want %d", i, got, 18+i)
		}
	}

	// 8-day downtime: window clamped to 7 days, oldest-first.
	latestPlan = shanghaiTime(2026, 8, 12, 0, 30, 0).UTC()
	plans, err = hook(ctx, ScopeGlobal, now, LatestPlanInfo{Found: true, PlanTime: latestPlan})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(plans) != 7 {
		t.Fatalf("8-day downtime: %d plans, want 7 (window clamp)", len(plans))
	}
	for i := 1; i < len(plans); i++ {
		if !plans[i-1].Before(plans[i]) {
			t.Fatalf("plans not oldest-first at %d", i)
		}
	}
}

func TestMaturityPlansForScopeRetryMerge(t *testing.T) {
	pool := newMaturityTestPool(t)
	q := db.New(pool)
	hook := maturityPlansForScope(q)
	ctx := context.Background()
	now := shanghaiTime(2026, 8, 20, 0, 31, 0).UTC()

	// Older FAILED (retry-eligible) + newer SUCCESS: the older plan must
	// survive in the returned set (never stranded by latest-only logic).
	older := shanghaiTime(2026, 8, 18, 0, 30, 0).UTC()
	newer := shanghaiTime(2026, 8, 19, 0, 30, 0).UTC()
	seed := func(plan time.Time, status string, attempt int) {
		_, err := pool.Exec(ctx, `
			INSERT INTO sys_cron_executions (id, job_name, scope_kind, scope_id, plan_time, status, attempt, max_attempts, next_retry_at)
			VALUES ($1, 'maturity_snapshot', 'global', 'global', $2, $3, $4, 3, $5)`,
			uuid.New(), plan, status, attempt, now.Add(-time.Minute))
		if err != nil {
			t.Fatalf("seed execution: %v", err)
		}
	}
	seed(older, "FAILED", 1)
	seed(newer, "SUCCESS", 1)

	plans, err := hook(ctx, ScopeGlobal, now, LatestPlanInfo{
		Found: true, PlanTime: newer, Status: "SUCCESS",
	})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	found := false
	for _, p := range plans {
		if p.Equal(older) {
			found = true
		}
	}
	if !found {
		t.Fatalf("older FAILED plan %v missing from %v", older, plans)
	}
	// Latest retry-eligible FAILED must also be present via the same set.
	plans2, err := hook(ctx, ScopeGlobal, now, LatestPlanInfo{
		Found: true, PlanTime: older, Status: "FAILED", Attempt: 1, MaxAttempts: 3, NextRetryAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	found = false
	for _, p := range plans2 {
		if p.Equal(older) {
			found = true
		}
	}
	if !found {
		t.Fatalf("latest FAILED plan %v missing from %v", older, plans2)
	}
}

func TestMaturitySnapshotHandlerBucketDate(t *testing.T) {
	// The handler's target bucket is the Shanghai-local day BEFORE the plan.
	// Verified through the rollup integration test; here we pin the date
	// arithmetic the handler relies on.
	plan := shanghaiTime(2026, 8, 20, 0, 30, 0)
	if got := plan.AddDate(0, 0, -1).Format("2006-01-02"); got != "2026-08-19" {
		t.Fatalf("target bucket = %s, want 2026-08-19", got)
	}
}

// TestMaturitySnapshotJobSpecGuards the pinned construction: hook-driven
// planning fields, the 7-plan cap, and lease timing mirroring the autopilot
// job. CatchUpMode/Window values must never leak into planning (the hook does
// not read the spec at all — this test pins the spec shape only).
func TestMaturitySnapshotJobSpec(t *testing.T) {
	pool := newMaturityTestPool(t)
	spec := MaturitySnapshotJob(pool)
	if spec.Name != JobNameMaturitySnapshot {
		t.Fatalf("name = %q", spec.Name)
	}
	if spec.PlansForScope == nil {
		t.Fatal("PlansForScope must be set (hook-driven planning)")
	}
	if spec.Cadence != 0 {
		t.Fatalf("cadence = %v, want 0", spec.Cadence)
	}
	if spec.MaxPlansPerTick != 7 {
		t.Fatalf("MaxPlansPerTick = %d, want 7", spec.MaxPlansPerTick)
	}
	if spec.MaxAttempts != 3 || len(spec.RetryBackoff) != 3 {
		t.Fatalf("retry config mismatch: %d attempts, %d backoffs", spec.MaxAttempts, len(spec.RetryBackoff))
	}
	if spec.RunTimeout <= 0 || spec.StaleTimeout <= spec.RunTimeout || spec.HeartbeatInterval >= spec.StaleTimeout {
		t.Fatalf("lease timing invalid: run=%v stale=%v heartbeat=%v", spec.RunTimeout, spec.StaleTimeout, spec.HeartbeatInterval)
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("spec.validate: %v", err)
	}
}
