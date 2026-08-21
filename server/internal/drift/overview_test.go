package drift

// AIFIRST: CR-2026-049 TASK-11 — health state table test + overview live-PG
// test (SDD §3.6/§4.2 AC-14). Six states incl. config_rev drift and cursor
// coverage; counts only open|acknowledged; empty latency sample → null.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/commitprefix"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func plan(status string, at time.Time, result map[string]any) *PlanRecord {
	return &PlanRecord{Status: status, StartedAt: at, Result: result}
}

func goodResult(extra func(m map[string]any)) map[string]any {
	m := map[string]any{
		"v": 1, "config_rev": "rev-x",
		"repository_ids": []any{"ai-first-platform-docs", "multica", "tools"},
		"scan_cursors":   map[string]any{"ai-first-platform-docs": "s1", "multica": "s2", "tools": "s3"},
		"finding_count":  0,
	}
	if extra != nil {
		extra(m)
	}
	return m
}

var declIDs = []string{"ai-first-platform-docs", "multica", "tools"}

func TestHealthStateSixStates(t *testing.T) {
	now := ts("2026-08-20T12:00:00Z")
	cases := []struct {
		name       string
		configured bool
		latest     *PlanRecord
		success    *PlanRecord
		configRev  string
		want       string
	}{
		{"not_configured", false, nil, nil, "rev-x", "not_configured"},
		{"uninitialized", true, nil, nil, "rev-x", "uninitialized"},
		{"failed_newest", true, plan("FAILED", ts("2026-08-20T11:00:00Z"), nil), plan("SUCCESS", ts("2026-08-20T10:00:00Z"), goodResult(nil)), "rev-x", "failed"},
		{"stale_age", true, plan("SUCCESS", ts("2026-08-20T09:00:00Z"), goodResult(nil)), plan("SUCCESS", ts("2026-08-20T09:00:00Z"), goodResult(nil)), "rev-x", "stale"},
		{"stale_config_rev", true, plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(nil)), plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(nil)), "rev-y", "stale"},
		{"stale_cursor_coverage", true, plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(func(m map[string]any) {
			delete(m["scan_cursors"].(map[string]any), "tools")
		})), plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(func(m map[string]any) {
			delete(m["scan_cursors"].(map[string]any), "tools")
		})), "rev-x", "stale"},
		{"ok", true, plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(nil)), plan("SUCCESS", ts("2026-08-20T11:30:00Z"), goodResult(nil)), "rev-x", "ok"},
	}
	for _, c := range cases {
		got := HealthState(now, c.configured, c.latest, c.success, c.configRev, declIDs)
		if got != c.want {
			t.Errorf("%s: health = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecodeResultV1Shape(t *testing.T) {
	if _, ok := DecodeResultV1(map[string]any{}); ok {
		t.Error("empty result must not decode")
	}
	rv, ok := DecodeResultV1(goodResult(nil))
	if !ok || rv.ConfigRev != "rev-x" || len(rv.ScanCursors) != 3 {
		t.Errorf("decode = %+v ok=%v", rv, ok)
	}
}

func openDriftPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://multica:multica@localhost:5432/multica?sslmode=disable")
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("db not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedOverviewWorkspace(t *testing.T, pool Querier) string {
	t.Helper()
	ctx := context.Background()
	reposJSON := `[{"url":"` + commitprefix.GeneratedPrefixes()["ai-first-platform-docs"].CanonicalURL + `"},{"url":"https://github.com/OldBoy405/AI-First-multica.git"},{"url":"https://github.com/OldBoy405/AI-First-tools.git"}]`
	var ws string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, repos) VALUES ('drift-tests', 'drift-tests', $1::jsonb)
		ON CONFLICT (slug) DO UPDATE SET repos = EXCLUDED.repos, updated_at = now()
		RETURNING id::text`, reposJSON).Scan(&ws); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM drift_finding WHERE workspace_id = $1::uuid`, ws)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sys_cron_executions WHERE scope_id = $1 AND job_name = 'commit_prefix_scan'`, ws)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1::uuid`, ws)
	})
	return ws
}

func insertSysCronSuccess(t *testing.T, pool Querier, ws string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO sys_cron_executions (job_name, scope_kind, scope_id, plan_time, status, result, started_at)
		VALUES ('commit_prefix_scan', 'workspace', $1, now(), 'SUCCESS', $2::jsonb, $3)`,
		ws, EncodeResultV1(commitprefix.GeneratedConfigRev(), declIDs, map[string]string{"ai-first-platform-docs": "s1", "multica": "s2", "tools": "s3"}, 0), at)
	if err != nil {
		t.Fatalf("seed sys_cron: %v", err)
	}
}

func seedFindings(t *testing.T, pool Querier, ws string) {
	t.Helper()
	evidence := `{"repository_id":"tools","trunk":"custom/main","commit_sha":"abc","commit_subject":"wip: x","scanned_at":"2026-08-20T00:00:00Z"}`
	rows := []struct {
		repo, kind, severity, summary, status string
		resolved                              bool
	}{
		{"tools", "wip-on-trunk", "info", "wip on trunk", "acknowledged", false},
		{"multica", "bypass-commit", "warn", "bypass commit", "open", false},
		{"tools", "bypass-commit", "warn", "resolved bypass", "resolved", true},
	}
	for _, row := range rows {
		resolvedExpr := "NULL"
		if row.resolved {
			resolvedExpr = "now() - interval '1 hour'"
		}
		_, err := pool.Exec(context.Background(), `
			INSERT INTO drift_finding (workspace_id, repository_id, spec_id, cr_id, kind, severity, summary, evidence, status, found_at, resolved_at)
			VALUES ($1::uuid, $2, NULL, NULL, $3, $4, $5, $6::jsonb, $7, now() - interval '2 hours', `+resolvedExpr+`)`,
			ws, row.repo, row.kind, row.severity, row.summary, evidence, row.status)
		if err != nil {
			t.Fatalf("seed finding: %v", err)
		}
	}
}

func TestOverviewLivePG(t *testing.T) {
	pool := openDriftPool(t)
	ctx := context.Background()
	ws := seedOverviewWorkspace(t, pool)

	repo := NewOverviewRepo(pool)
	overview, err := repo.Overview(ctx, ws, time.Now())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if overview.ScanHealth != "uninitialized" {
		t.Errorf("fresh workspace health = %s, want uninitialized", overview.ScanHealth)
	}
	if overview.ResolveLatencyMS.P50 != nil || overview.ResolveLatencyMS.P90 != nil || overview.ResolveLatencyMS.SampleCount != 0 {
		t.Errorf("empty latency sample must be null: %+v", overview.ResolveLatencyMS)
	}

	insertSysCronSuccess(t, pool, ws, time.Now().Add(-30*time.Minute))
	seedFindings(t, pool, ws)

	overview, err = repo.Overview(ctx, ws, time.Now())
	if err != nil {
		t.Fatalf("overview2: %v", err)
	}
	if overview.ScanHealth != "ok" {
		t.Errorf("health = %s, want ok", overview.ScanHealth)
	}
	if overview.BypassCount != 1 || overview.WipOnTrunkCount != 1 {
		t.Errorf("counts = bypass %d wip %d, want 1/1 (resolved excluded)", overview.BypassCount, overview.WipOnTrunkCount)
	}
	if overview.ResolveLatencyMS.SampleCount != 1 || overview.ResolveLatencyMS.P50 == nil {
		t.Errorf("latency = %+v", overview.ResolveLatencyMS)
	}

	// Age the success plan > 2h → stale.
	if _, err := pool.Exec(ctx, `
		UPDATE sys_cron_executions SET started_at = now() - interval '3 hours'
		WHERE job_name = 'commit_prefix_scan' AND scope_id = $1 AND status = 'SUCCESS'`, ws); err != nil {
		t.Fatal(err)
	}
	overview, err = repo.Overview(ctx, ws, time.Now())
	if err != nil {
		t.Fatalf("overview3: %v", err)
	}
	if overview.ScanHealth != "stale" {
		t.Errorf("aged health = %s, want stale", overview.ScanHealth)
	}
}
