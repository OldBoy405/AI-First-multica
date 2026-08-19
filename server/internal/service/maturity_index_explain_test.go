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

// TestMaturityIndexesServeTheirQueries pins migrations 378/379 to the read
// queries they were built for (SDD §2.1, TASK-02 acceptance 3): the report
// history keyset must hit idx_atq_maturity_report_history — never the active-
// task index from migration 369 — and the scope/date trend read must hit
// maturity_snapshot_scope_date_idx.
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

	// Seed one completed report task so the partial index has at least one
	// candidate row for the planner.
	wsID := uuid.New()
	projectID := uuid.New()
	agentID := uuid.New()
	taskID := uuid.New()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug) VALUES ($1,'idx-fixture',$2)`, wsID, "idx-fixture-"+wsID.String()[:8])
	exec(`INSERT INTO project (id, workspace_id, title, status, priority) VALUES ($1,$2,'idx-p','in_progress','none')`, projectID, wsID)
	exec(`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES ($1,$2,'idx-a','local')`, agentID, wsID)
	exec(`INSERT INTO agent_task_queue (id, agent_id, project_id, status, completed_at, result)
	      VALUES ($1,$2,$3,'completed',$4,$5)`,
		taskID, agentID, projectID, time.Now(), []byte(`{"schema":"ai-first.maturity-report/v1","report_key":"k","content_sha256":"","markdown":"x"}`))

	planHistory := explain(t, ctx, pool, `
		EXPLAIN SELECT id FROM agent_task_queue
		WHERE project_id = $1 AND status = 'completed'
		  AND result->>'schema' = 'ai-first.maturity-report/v1'
		ORDER BY completed_at DESC, id DESC LIMIT 12`, projectID)
	if !strings.Contains(planHistory, "idx_atq_maturity_report_history") {
		t.Fatalf("report history plan must use idx_atq_maturity_report_history:\n%s", planHistory)
	}
	if strings.Contains(planHistory, "idx_atq_project_active") {
		t.Fatalf("report history plan must not fall back to the active-task index (369):\n%s", planHistory)
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
