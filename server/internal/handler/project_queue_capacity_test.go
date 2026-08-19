package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CR-2026-004: shared Team Agent queue capacity governance. These tests run
// the production enqueue paths against a project whose settings pin
// team_agent_queue_limit to a small value, so "queue full" is reachable
// without seeding dozens of rows.

// createCapacityTestProject inserts a project with the given settings JSON
// (pass "" for the default empty bag) and returns its id.
func createCapacityTestProject(t *testing.T, title, settingsJSON string) string {
	t.Helper()
	if settingsJSON == "" {
		settingsJSON = "{}"
	}
	var projectID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO project (workspace_id, title, settings)
		VALUES ($1, $2, $3::jsonb)
		RETURNING id
	`, testWorkspaceID, title, settingsJSON).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID) })
	return projectID
}

// createCapacityTestIssue inserts an issue linked to the project, assigned to
// the agent, created by creatorID, and returns its id.
func createCapacityTestIssue(t *testing.T, projectID, agentID, creatorID string, number int) string {
	t.Helper()
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_type, creator_id, assignee_type, assignee_id, number, position)
		VALUES ($1, $2, $3, 'todo', 'medium', 'member', $4, 'agent', $5, $6, 0)
		RETURNING id
	`, testWorkspaceID, projectID, fmt.Sprintf("qcap-issue-%d", number), creatorID, agentID, number).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })
	return issueID
}

// fillProjectQueue inserts n queued tasks on fresh issues in the project so
// the shared queue reads as depth n.
func fillProjectQueue(t *testing.T, projectID, agentID string, n, numberBase int) {
	t.Helper()
	for i := 0; i < n; i++ {
		issueID := createCapacityTestIssue(t, projectID, agentID, testUserID, numberBase+i)
		var taskID string
		if err := testPool.QueryRow(context.Background(), `
			INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id)
			VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'queued', 0, $2)
			RETURNING id
		`, agentID, issueID).Scan(&taskID); err != nil {
			t.Fatalf("seed queued task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	}
}

// createPlainMemberUser inserts a user with role=member in the test workspace.
func createPlainMemberUser(t *testing.T, suffix string) string {
	t.Helper()
	var userID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "QCap Member "+suffix, "qcap-"+suffix+"@multica.test").Scan(&userID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')
	`, testWorkspaceID, userID); err != nil {
		t.Fatalf("create member row: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
	})
	return userID
}

func capacityTestIssueStruct(issueID, projectID, agentID, creatorID string) db.Issue {
	return db.Issue{
		ID:           parseUUID(issueID),
		WorkspaceID:  parseUUID(testWorkspaceID),
		ProjectID:    parseUUID(projectID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
		AssigneeID:   parseUUID(agentID),
		CreatorType:  "member",
		CreatorID:    parseUUID(creatorID),
		Priority:     "medium",
	}
}

// TestProjectQueueCapacity_MemberRejected: a plain member's enqueue into a
// full project queue returns ErrProjectQueueFull and inserts nothing (AC-1).
func TestProjectQueueCapacity_MemberRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "QCapAgentReject", []byte("[]"))
	memberID := createPlainMemberUser(t, "reject")
	projectID := createCapacityTestProject(t, "qcap-reject", `{"team_agent_queue_limit": 2}`)
	fillProjectQueue(t, projectID, agentID, 2, 93000)

	issueID := createCapacityTestIssue(t, projectID, agentID, memberID, 93050)
	_, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, capacityTestIssueStruct(issueID, projectID, agentID, memberID))
	var full *service.ErrProjectQueueFull
	if !errors.As(err, &full) {
		t.Fatalf("expected ErrProjectQueueFull, got %v", err)
	}
	if full.Depth < 2 || full.Limit != 2 {
		t.Fatalf("unexpected depth/limit: %d/%d", full.Depth, full.Limit)
	}

	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected enqueue left %d rows", count)
	}
}

// TestProjectQueueCapacity_OwnerPreempts: the workspace owner enqueues into
// the same full queue, lands with the preempt priority (AC-2 component).
func TestProjectQueueCapacity_OwnerPreempts(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "QCapAgentOwner", []byte("[]"))
	projectID := createCapacityTestProject(t, "qcap-owner", `{"team_agent_queue_limit": 2}`)
	fillProjectQueue(t, projectID, agentID, 2, 93100)

	issueID := createCapacityTestIssue(t, projectID, agentID, testUserID, 93150)
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, capacityTestIssueStruct(issueID, projectID, agentID, testUserID))
	if err != nil {
		t.Fatalf("owner enqueue on full queue should pass: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, uuidToString(task.ID)) })
	if task.Priority != service.PreemptPriorityOwnerAdmin {
		t.Fatalf("expected preempt priority %d, got %d", service.PreemptPriorityOwnerAdmin, task.Priority)
	}
}

// TestProjectQueueCapacity_InvalidLimitFallsBack: an invalid settings value
// falls back to the default (50), so depth 2 does not block (FR-3 fallback).
func TestProjectQueueCapacity_InvalidLimitFallsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "QCapAgentFallback", []byte("[]"))
	memberID := createPlainMemberUser(t, "fallback")
	projectID := createCapacityTestProject(t, "qcap-fallback", `{"team_agent_queue_limit": "not-a-number"}`)
	fillProjectQueue(t, projectID, agentID, 2, 93200)

	issueID := createCapacityTestIssue(t, projectID, agentID, memberID, 93250)
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, capacityTestIssueStruct(issueID, projectID, agentID, memberID))
	if err != nil {
		t.Fatalf("enqueue under default limit should pass: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, uuidToString(task.ID)) })
	if task.Priority != 2 { // priorityToInt("medium")
		t.Fatalf("plain member must keep ordinary priority, got %d", task.Priority)
	}
}

// TestProjectQueueCapacity_DeferredPathUnguarded: the deferred system
// compensation path ignores the capacity gate (SDD INSERT-point table).
func TestProjectQueueCapacity_DeferredPathUnguarded(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "QCapAgentDeferred", []byte("[]"))
	projectID := createCapacityTestProject(t, "qcap-deferred", `{"team_agent_queue_limit": 2}`)
	fillProjectQueue(t, projectID, agentID, 2, 93300)

	issueID := createCapacityTestIssue(t, projectID, agentID, testUserID, 93350)
	task, err := testHandler.TaskService.EnqueueDeferredAssigneeFallback(ctx,
		capacityTestIssueStruct(issueID, projectID, agentID, testUserID),
		parseUUID(agentID), pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("deferred path must bypass capacity gate: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, uuidToString(task.ID)) })
}

// TestCancelTaskByUser_PlainMember_NotOriginator_Returns403: the shared-queue
// stop rule — a plain member cannot cancel someone else's task (FR-4/AC-4).
func TestCancelTaskByUser_PlainMember_NotOriginator_Returns403(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "QCapAgentCancel", []byte("[]"))
	originatorID := createPlainMemberUser(t, "originator")
	strangerID := createPlainMemberUser(t, "stranger")
	projectID := createCapacityTestProject(t, "qcap-cancel", "")
	issueID := createCapacityTestIssue(t, projectID, agentID, originatorID, 93400)

	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id, originator_user_id, accountable_user_id)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'queued', 0, $2, $3, $3)
		RETURNING id
	`, agentID, issueID, originatorID).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	// Stranger member: rejected, row untouched, audit intact.
	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelTaskByUserRequest(t, strangerID, taskID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("stranger cancel: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_task_originator") {
		t.Fatalf("missing stable error code: %s", w.Body.String())
	}
	if got := taskStatus(t, taskID); got != "queued" {
		t.Fatalf("task mutated by rejected cancel: %q", got)
	}

	// Originator: allowed; soft delete keeps the row (audit).
	w = httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelTaskByUserRequest(t, originatorID, taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("originator cancel: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := taskStatus(t, taskID); got != "cancelled" {
		t.Fatalf("expected cancelled, got %q", got)
	}
}

// TestCancelTaskByUser_OwnerCancelsAny: workspace owner keeps the "stop
// anything" semantics over other members' tasks.
func TestCancelTaskByUser_OwnerCancelsAny(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "QCapAgentOwnerCancel", []byte("[]"))
	originatorID := createPlainMemberUser(t, "owner-cancel-target")
	projectID := createCapacityTestProject(t, "qcap-owner-cancel", "")
	issueID := createCapacityTestIssue(t, projectID, agentID, originatorID, 93500)

	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id, originator_user_id, accountable_user_id)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'queued', 0, $2, $3, $3)
		RETURNING id
	`, agentID, issueID, originatorID).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelTaskByUserRequest(t, testUserID, taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("owner cancel: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := taskStatus(t, taskID); got != "cancelled" {
		t.Fatalf("expected cancelled, got %q", got)
	}
}
