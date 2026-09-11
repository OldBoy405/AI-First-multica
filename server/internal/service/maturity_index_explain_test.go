package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMaturityIndexesServeTheirQueries pins the maturity read indexes to the
// queries they were built for (SDD §2.1, TASK-02 acceptance 3): the report
// history keyset must hit idx_atq_maturity_report_history (migration 464) —
// never the active-task index from migration 454 — and the scope/date trend
// read must hit maturity_snapshot_scope_date_idx (migration 463).
func TestMaturityIndexesServeTheirQueries(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Seed a realistic number of completed report tasks so the partial index has
	// both candidates and enough weight to win the plan: on a one-row table a
	// sequential scan genuinely is cheaper, so the assertion below would depend
	// on how much data the shared test database happened to hold rather than on
	// the query — which is exactly how this test flaked.
	wsID := uuid.New()
	projectID := uuid.New()
	agentID := uuid.New()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug) VALUES ($1,'idx-fixture',$2)`, wsID, "idx-fixture-"+wsID.String()[:8])
	exec(`INSERT INTO project (id, workspace_id, title, status, priority) VALUES ($1,$2,'idx-p','in_progress','none')`, projectID, wsID)
	exec(`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES ($1,$2,'idx-a','local')`, agentID, wsID)
	exec(`INSERT INTO agent_task_queue (id, agent_id, project_id, status, completed_at, result)
	      SELECT gen_random_uuid(), $1, $2, 'completed', now() - (g || ' seconds')::interval, $3
	      FROM generate_series(1, 3000) AS g`,
		agentID, projectID, []byte(`{"schema":"ai-first.maturity-report/v1","report_key":"k","content_sha256":"","markdown":"x"}`))
	// The scope/date read is asserted against maturity_snapshot next. Seed the
	// workspace's own org buckets plus filler rows in other scopes: with a few
	// hundred rows a sequential scan is still cheaper than the index scan, so
	// the assertion would measure the table's size instead of the query.
	exec(`INSERT INTO maturity_snapshot (workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev)
	      SELECT $1, DATE '2026-01-01' + g, 'org', '·', '{}'::jsonb, '{}'::jsonb, repeat('0', 40)
	      FROM generate_series(0, 364) AS g`, wsID)
	exec(`INSERT INTO maturity_snapshot (workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev)
	      SELECT $1, DATE '2026-01-01' + (g % 365), 'project', 'p' || (g / 365), '{}'::jsonb, '{}'::jsonb, repeat('0', 40)
	      FROM generate_series(0, 10949) AS g`, wsID)
	// Refresh the statistics the planner reads.
	exec(`ANALYZE agent_task_queue`)
	exec(`ANALYZE maturity_snapshot`)

	planHistory := explain(t, ctx, pool, `
		EXPLAIN SELECT id FROM agent_task_queue
		WHERE project_id = $1 AND status = 'completed'
		  AND result->>'schema' = 'ai-first.maturity-report/v1'
		ORDER BY completed_at DESC, id DESC LIMIT 12`, projectID)
	if !strings.Contains(planHistory, "idx_atq_maturity_report_history") {
		t.Fatalf("report history plan must use idx_atq_maturity_report_history:\n%s", planHistory)
	}
	if strings.Contains(planHistory, "idx_atq_project_active") {
		t.Fatalf("report history plan must not fall back to the active-task index (454):\n%s", planHistory)
	}

	planScope := explain(t, ctx, pool, `
		EXPLAIN SELECT bucket_date FROM maturity_snapshot
		WHERE workspace_id = $1 AND scope = 'org' AND scope_id = '·'
		  AND bucket_date >= '2026-01-01' AND bucket_date <= '2026-12-31'
		ORDER BY bucket_date ASC LIMIT 366`, wsID)
	if !strings.Contains(planScope, "maturity_snapshot_scope_date_idx") {
		t.Fatalf("scope/date read must use maturity_snapshot_scope_date_idx:\n%s", planScope)
	}
}

func explain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
