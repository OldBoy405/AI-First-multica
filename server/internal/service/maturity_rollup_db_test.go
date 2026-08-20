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

	// This user has no task created in the target bucket. Their older task emits
	// usage on the target day and must still produce a user snapshot.
	usageOnlyUserID := uuid.New()
	usageOnlyTaskID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO "user" (id,name,email) VALUES ($1,'usage-only',$2)`, usageOnlyUserID, "usage-only-"+usageOnlyUserID.String()+"@example.test"); err != nil {
		t.Fatalf("seed usage-only user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (id,workspace_id,user_id,role) VALUES ($1,$2,$3,'member')`, uuid.New(), wsID, usageOnlyUserID); err != nil {
		t.Fatalf("seed usage-only member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_task_queue (id,agent_id,initiator_user_id,status,created_at,completed_at)
		SELECT $1,id,$2,'completed','2026-08-18 01:00:00+08','2026-08-19 02:00:00+08'
		FROM agent WHERE workspace_id=$3 LIMIT 1`, usageOnlyTaskID, usageOnlyUserID, wsID); err != nil {
		t.Fatalf("seed usage-only task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_usage (id,task_id,provider,model,input_tokens,output_tokens,created_at)
		VALUES ($1,$2,'openai','gpt-5.6',25,5,'2026-08-19 02:00:00+08')`, uuid.New(), usageOnlyTaskID); err != nil {
		t.Fatalf("seed usage-only tokens: %v", err)
	}

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
	penetration := metrics.MetricValues[maturity.MetricAIPenetration]
	if penetration.Value == nil || penetration.Numerator == nil || *penetration.Numerator != 2 || penetration.Denominator == nil || *penetration.Denominator != 3 {
		t.Fatalf("ai penetration = %+v, want 2 current-day task initiators / 3 members", penetration)
	}

	var usageOnlyMetricsJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT metrics FROM maturity_snapshot
		WHERE workspace_id=$1 AND bucket_date=$2 AND scope='user' AND scope_id=$3`,
		wsID, target, usageOnlyUserID.String(),
	).Scan(&usageOnlyMetricsJSON); err != nil {
		t.Fatalf("read usage-only user row: %v", err)
	}
	var usageOnlyMetrics maturity.SnapshotMetricsV1
	if err := json.Unmarshal(usageOnlyMetricsJSON, &usageOnlyMetrics); err != nil {
		t.Fatalf("usage-only metrics JSON: %v", err)
	}
	if got := usageOnlyMetrics.Headline.TotalTokens; got != 30 {
		t.Fatalf("usage-only user tokens = %d, want 30", got)
	}

	var userMetricsJSON, userScoresJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT metrics, scores FROM maturity_snapshot
		WHERE workspace_id = $1 AND bucket_date = $2 AND scope = 'user'
		ORDER BY scope_id LIMIT 1`, wsID, target).Scan(&userMetricsJSON, &userScoresJSON); err != nil {
		t.Fatalf("read user row: %v", err)
	}
	var userMetrics maturity.SnapshotMetricsV1
	if err := json.Unmarshal(userMetricsJSON, &userMetrics); err != nil {
		t.Fatalf("user metrics JSON: %v", err)
	}
	for _, key := range maturity.AllMetricKeys {
		if key == maturity.MetricTokenIntensity || key == maturity.MetricTeamAgentDepth {
			continue
		}
		if got := userMetrics.MetricValues[key].DataStatus; got != maturity.StatusNotApplicable {
			t.Fatalf("user metric %s status = %s, want not_applicable", key, got)
		}
	}
	if string(userScoresJSON) != "{}" {
		t.Fatalf("user scores = %s, want {}", userScoresJSON)
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

func TestRollupMaturityWorkspaceFillsOlderMissingBucket(t *testing.T) {
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
	defer pool.Close()

	wsID := seedMaturityFixture(t, ctx, pool)
	newer := time.Date(2026, 8, 21, 0, 30, 0, 0, shanghaiLoc) // target Aug 20
	older := time.Date(2026, 8, 20, 0, 30, 0, 0, shanghaiLoc) // target Aug 19
	if rows, err := RollupMaturityWorkspace(ctx, pool, wsID, newer); err != nil || rows == 0 {
		t.Fatalf("newer rollup = rows %d, err %v", rows, err)
	}
	if rows, err := RollupMaturityWorkspace(ctx, pool, wsID, older); err != nil || rows == 0 {
		t.Fatalf("older retry = rows %d, err %v; an exact-bucket check must fill the hole", rows, err)
	}
	var dates int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT bucket_date) FROM maturity_snapshot WHERE workspace_id=$1 AND scope='org'`, wsID).Scan(&dates); err != nil {
		t.Fatal(err)
	}
	if dates != 2 {
		t.Fatalf("org buckets = %d, want 2", dates)
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
	noUsageUserID := uuid.New()
	noUsageTaskID := uuid.New()
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
	exec(`INSERT INTO "user" (id, name, email) VALUES ($1, $2, $3)`, noUsageUserID, "no-usage-user", "fixture-"+noUsageUserID.String()+"@example.test")
	exec(`INSERT INTO member (id, workspace_id, user_id, role) VALUES ($1, $2, $3, 'admin')`, uuid.New(), wsID, userID)
	exec(`INSERT INTO member (id, workspace_id, user_id, role) VALUES ($1, $2, $3, 'member')`, uuid.New(), wsID, noUsageUserID)
	exec(`INSERT INTO project (id, workspace_id, title, status) VALUES ($1, $2, 'p1', 'in_progress')`, projectID, wsID)
	exec(`INSERT INTO issue (id, workspace_id, project_id, title, creator_type, creator_id) VALUES ($1, $2, $3, 'shell', 'member', $4)`, issueID, wsID, projectID, userID)
	exec(`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES ($1, $2, 'a1', 'local')`, agentID, wsID)
	exec(`INSERT INTO agent_task_queue (id, agent_id, initiator_user_id, project_id, issue_id, status, created_at, completed_at)
	      VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7)`, taskID, agentID, userID, projectID, issueID, dayFrom, dayTo)
	exec(`INSERT INTO agent_task_queue (id, agent_id, initiator_user_id, project_id, issue_id, status, created_at, completed_at)
	      VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7)`, noUsageTaskID, agentID, noUsageUserID, projectID, issueID, dayFrom, dayTo)
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
