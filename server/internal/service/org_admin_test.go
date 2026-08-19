package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBuildReportEnvelope(t *testing.T) {
	wsID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	taskID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	sessionID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	body := []byte("# Weekly\n\nFive sections.\n")

	env, err := BuildReportEnvelope(wsID, "2026-W34", body, taskID, sessionID, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ReportKey != uuid.UUID(wsID.Bytes).String()+":2026-W34" {
		t.Fatalf("report_key = %q", env.ReportKey)
	}
	if env.RelativePath != "docs/org-admin/maturity-review-2026-W34.md" {
		t.Fatalf("relative_path = %q", env.RelativePath)
	}
	if !VerifyReportSHA(body, env.ContentSha256) {
		t.Fatal("envelope SHA must verify")
	}
	// Tampered body must fail verification.
	if VerifyReportSHA([]byte("# altered"), env.ContentSha256) {
		t.Fatal("tampered body must not verify")
	}
	// Bad week / empty body rejected.
	if _, err := BuildReportEnvelope(wsID, "2026-34", body, taskID, sessionID, nil); err == nil {
		t.Fatal("bad week must be rejected")
	}
	if _, err := BuildReportEnvelope(wsID, "2026-W34", nil, taskID, sessionID, nil); err == nil {
		t.Fatal("empty markdown must be rejected")
	}
}

func TestEnsureOrgAdminWorkspaceIdempotent(t *testing.T) {
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

	wsID := uuid.New()
	ownerID := uuid.New()
	runtimeID := uuid.New()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug) VALUES ($1,'oa-fixture',$2)`, wsID, "oa-fixture-"+wsID.String()[:8])
	exec(`INSERT INTO "user" (id, name, email) VALUES ($1,'oa-owner',$2)`, ownerID, "oa-"+ownerID.String()+"@example.test")
	exec(`INSERT INTO member (id, workspace_id, user_id, role) VALUES ($1,$2,$3,'admin')`, uuid.New(), wsID, ownerID)
	exec(`INSERT INTO agent_runtime (id, workspace_id, name, runtime_mode, provider, status) VALUES ($1,$2,'oa-runtime','local','openai','online')`, runtimeID, wsID)

	queries := db.New(pool)
	wsp := pgtype.UUID{Bytes: wsID, Valid: true}
	ownp := pgtype.UUID{Bytes: ownerID, Valid: true}
	runp := pgtype.UUID{Bytes: runtimeID, Valid: true}

	first, err := EnsureOrgAdminWorkspace(ctx, queries, pool, wsp, ownp, runp)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	second, err := EnsureOrgAdminWorkspace(ctx, queries, pool, wsp, ownp, runp)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if first.ProjectID.Bytes != second.ProjectID.Bytes ||
		first.AgentID.Bytes != second.AgentID.Bytes ||
		first.AutopilotID.Bytes != second.AutopilotID.Bytes ||
		first.TriggerID.Bytes != second.TriggerID.Bytes {
		t.Fatalf("bootstrap not idempotent: %+v vs %+v", first, second)
	}

	// Row-count invariants: one system-key project, one system-key agent,
	// one autopilot + one schedule trigger.
	var projectCount, agentCount, autopilotCount, triggerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project WHERE workspace_id=$1 AND settings->>'system_key'='org-admin-workspace'`, wsID).Scan(&projectCount); err != nil || projectCount != 1 {
		t.Fatalf("projects = %d, %v", projectCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND system_key='org-admin'`, wsID).Scan(&agentCount); err != nil || agentCount != 1 {
		t.Fatalf("agents = %d, %v", agentCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM autopilot WHERE workspace_id=$1 AND project_id=$2`, wsID, first.ProjectID.Bytes).Scan(&autopilotCount); err != nil || autopilotCount != 1 {
		t.Fatalf("autopilots = %d, %v", autopilotCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM autopilot_trigger WHERE autopilot_id=$1 AND kind='schedule'`, first.AutopilotID.Bytes).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("triggers = %d, %v", triggerCount, err)
	}
}
