package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/maturity"
)

// TestRollupMaturityWorkspaceIntegration exercises the full write path
// against a migrated PostgreSQL (DATABASE_URL, default local dev DB). Skipped
// when no database is reachable — the pure-logic coverage lives in
// maturity_rollup_test.go.
func TestRollupMaturityWorkspaceIntegration(t *testing.T) {
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

	wsID := seedMaturityFixture(t, ctx, pool)
	planTime := time.Date(2026, 8, 20, 0, 30, 0, 0, shanghaiLoc)
	target := previousLocalDate(planTime)

	rows, err := RollupMaturityWorkspace(ctx, pool, wsID, planTime)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if rows < 1 {
		t.Fatalf("expected at least the org row, inserted %d", rows)
	}

	// Rerun same plan: watermark no-op, zero new rows, history unchanged.
	again, err := RollupMaturityWorkspace(ctx, pool, wsID, planTime)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again != 0 {
		t.Fatalf("rerun inserted %d rows, want 0 (watermark no-op)", again)
	}

	var configRev string
	var metricsJSON, scoresJSON []byte
	err = pool.QueryRow(ctx, `
		SELECT config_rev, metrics, scores FROM maturity_snapshot
		WHERE workspace_id = $1 AND bucket_date = $2 AND scope = 'org' AND scope_id = '·'`,
		wsID, target,
	).Scan(&configRev, &metricsJSON, &scoresJSON)
	if err != nil {
		t.Fatalf("read org row: %v", err)
	}
	if configRev != maturity.GeneratedConfigRev() {
		t.Fatalf("config_rev = %q, want %q", configRev, maturity.GeneratedConfigRev())
	}
	var metrics maturity.SnapshotMetricsV1
	if err := json.Unmarshal(metricsJSON, &metrics); err != nil {
		t.Fatalf("metrics JSON: %v", err)
	}
	if err := ValidateSnapshotMetrics(metrics); err != nil {
		t.Fatalf("stored metrics invalid: %v", err)
	}
	var scores map[string]any
	if err := json.Unmarshal(scoresJSON, &scores); err != nil {
		t.Fatalf("scores JSON: %v", err)
	}
	// Observing seed config -> scores must stay the empty object.
	if len(scores) != 0 {
		t.Fatalf("observing period must store empty scores, got %v", scores)
	}
}

// seedMaturityFixture creates a minimal workspace with one member, one
// project, one agent, one attributed task with usage on the target day, and
// one archived CR (with a free-text owner — which must flip
// project_collab_scale to unavailable, not crash the rollup).
func seedMaturityFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	wsID := uuid.New()
	userID := uuid.New()
	agentID := uuid.New()
	projectID := uuid.New()
	issueID := uuid.New()
	taskID := uuid.New()
	usageID := uuid.New()
	crRowID := uuid.New()
	crID := "CR-FIX-" + uuid.NewString()[:8]
	target := time.Date(2026, 8, 19, 0, 0, 0, 0, shanghaiLoc)
	dayFrom := target.UTC()
	dayTo := target.AddDate(0, 0, 1).UTC()

	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug) VALUES ($1, $2, $3)`, wsID, "maturity-fixture", "maturity-fixture-"+wsID.String()[:8])
	exec(`INSERT INTO "user" (id, name, email) VALUES ($1, $2, $3)`, userID, "fixture-user", "fixture-"+userID.String()+"@example.test")
	exec(`INSERT INTO member (id, workspace_id, user_id, role) VALUES ($1, $2, $3, 'admin')`, uuid.New(), wsID, userID)
	exec(`INSERT INTO project (id, workspace_id, title, status) VALUES ($1, $2, 'p1', 'in_progress')`, projectID, wsID)
	exec(`INSERT INTO issue (id, workspace_id, project_id, title, creator_type, creator_id) VALUES ($1, $2, $3, 'shell', 'member', $4)`, issueID, wsID, projectID, userID)
	exec(`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES ($1, $2, 'a1', 'local')`, agentID, wsID)
	exec(`INSERT INTO agent_task_queue (id, agent_id, initiator_user_id, project_id, issue_id, status, created_at, completed_at)
	      VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7)`, taskID, agentID, userID, projectID, issueID, dayFrom, dayTo)
	exec(`INSERT INTO task_usage (id, task_id, provider, model, input_tokens, output_tokens, cost_usd_ticks, created_at)
	      VALUES ($1, $2, 'openai', 'gpt-5.6', 100, 50, 1500, $3)`, usageID, taskID, dayFrom)
	exec(`INSERT INTO cr (id, workspace_id, cr_id, title, status, owners, shell_issue_id)
	      VALUES ($1, $2, $3, 'fixture cr', 'archived', $4, $5)`,
		crRowID, wsID, crID, json.RawMessage(`{"requirement":{"id":"Ray","assigned-at":"2026-08-20T00:00:00+08:00"}}`), issueID)
	exec(`INSERT INTO cr_sync_event (cr_id, commit_sha, event_kind, payload, occurred_at)
	      VALUES ($1, $2, 'status', $3, $4)`,
		crID, "seed-"+crID, json.RawMessage(`{"to_status":"archived"}`), dayFrom)
	return pgtype.UUID{Bytes: wsID, Valid: true}
}
