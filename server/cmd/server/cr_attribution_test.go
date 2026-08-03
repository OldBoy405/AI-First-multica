package main

// AIFIRST: tests for CR-2026-011 TASK-04's cr_id attribution path
// (SetTaskCRAttributionIfValid, SDD DD-4). Follows rerun_session_test.go's
// fixture convention.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestSetTaskCRAttributionIfValidAcceptsSameWorkspaceCR is the positive case:
// a cr row exists in the task's own workspace, so the daemon's self-reported
// cr_id is written.
func TestSetTaskCRAttributionIfValidAcceptsSameWorkspaceCR(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	issueID, agentID, runtimeID := setupRerunTestFixture(t)
	t.Cleanup(func() { cleanupRerunFixture(t, issueID) })
	ctx := context.Background()

	crID := "CR-9006-001"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, title, status, projected_commit)
		VALUES ($1::uuid, $2, 'attribution test', 'developing', 'deadbeef')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID); err != nil {
		t.Fatalf("cr fixture: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM cr WHERE cr_id = $1`, crID) })

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'running', 0) RETURNING id::text`,
		agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("task fixture: %v", err)
	}

	queries := db.New(testPool)
	rows, err := queries.SetTaskCRAttributionIfValid(ctx, db.SetTaskCRAttributionIfValidParams{
		ID:   pgtype.UUID{Bytes: parseUUIDBytes(taskID), Valid: true},
		CrID: pgtype.Text{String: crID, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetTaskCRAttributionIfValid failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected 1 row affected, got %d", rows)
	}
	var gotCrID string
	if err := testPool.QueryRow(ctx, `SELECT cr_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&gotCrID); err != nil {
		t.Fatalf("select cr_id: %v", err)
	}
	if gotCrID != crID {
		t.Fatalf("expected cr_id=%s, got %q", crID, gotCrID)
	}
}

// TestSetTaskCRAttributionIfValidRejectsUnknownCR is the negative case: a
// self-reported cr_id with no matching cr row in this task's workspace is
// silently ignored (zero rows affected, cr_id stays NULL) — the daemon's
// self-report is not a security boundary (SDD DD-4).
func TestSetTaskCRAttributionIfValidRejectsUnknownCR(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	issueID, agentID, runtimeID := setupRerunTestFixture(t)
	t.Cleanup(func() { cleanupRerunFixture(t, issueID) })
	ctx := context.Background()

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'running', 0) RETURNING id::text`,
		agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("task fixture: %v", err)
	}

	queries := db.New(testPool)
	rows, err := queries.SetTaskCRAttributionIfValid(ctx, db.SetTaskCRAttributionIfValidParams{
		ID:   pgtype.UUID{Bytes: parseUUIDBytes(taskID), Valid: true},
		CrID: pgtype.Text{String: "CR-9999-999", Valid: true},
	})
	if err != nil {
		t.Fatalf("SetTaskCRAttributionIfValid failed: %v", err)
	}
	if rows != 0 {
		t.Fatalf("expected 0 rows affected for an unknown cr_id, got %d", rows)
	}
	var gotCrID pgtype.Text
	if err := testPool.QueryRow(ctx, `SELECT cr_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&gotCrID); err != nil {
		t.Fatalf("select cr_id: %v", err)
	}
	if gotCrID.Valid {
		t.Fatalf("expected cr_id to stay NULL, got %q", gotCrID.String)
	}
}
