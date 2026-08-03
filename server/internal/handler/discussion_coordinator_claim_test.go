package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestClaimTaskByRuntime_DiscussionContainerTaskIsAskOnly pins the CR-2026-012
// read-only sandbox rule (SDD §4.4): an issue task hanging on the project
// Discussion container (origin_type='project_discussion') claims with
// ask_only=true, keyed on the CONTAINER rather than the agent. A task on an
// ordinary issue claimed by the same runtime stays unrestricted — the rule
// must not leak onto normal work.
func TestClaimTaskByRuntime_DiscussionContainerTaskIsAskOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}
	// Two in-flight tasks (one per issue) must not block each other on the
	// agent concurrency limit.
	if _, err := testPool.Exec(ctx, `UPDATE agent SET max_concurrent_tasks = 5 WHERE id = $1`, agentID); err != nil {
		t.Fatalf("raise max_concurrent_tasks: %v", err)
	}

	insertIssue := func(title string, originType *string) string {
		t.Helper()
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, creator_type, creator_id, title, origin_type, status, priority, number, position)
			VALUES ($1, 'member', $2, $3, $4, 'in_progress', 'none', $5, 0)
			RETURNING id
		`, testWorkspaceID, testUserID, title, originType, number).Scan(&id); err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}

	// origin_type has a CHECK constraint on a fixed value list: the "no
	// origin" regression-control issue must insert SQL NULL, not an empty
	// string (same trap as discussion_trigger_exemption_test.go).
	discussionOrigin := "project_discussion"
	discussionIssueID := insertIssue("dc ask-only discussion container", &discussionOrigin)
	ordinaryIssueID := insertIssue("dc ask-only regression control", nil)

	seedQueuedTask := func(issueID string) string {
		t.Helper()
		var taskID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
			VALUES ($1, $2, $3, 'queued', 0)
			RETURNING id
		`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
			t.Fatalf("seed queued task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
		return taskID
	}

	discussionTaskID := seedQueuedTask(discussionIssueID)
	ordinaryTaskID := seedQueuedTask(ordinaryIssueID)

	claimAskOnly := func() (taskID string, askOnly bool, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
			testWorkspaceID, "dc-ask-only-review")
		req = withURLParam(req, "runtimeId", runtimeID)
		testHandler.ClaimTaskByRuntime(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Task *struct {
				ID      string `json:"id"`
				AskOnly bool   `json:"ask_only"`
			} `json:"task"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode claim response: %v", err)
		}
		if resp.Task == nil {
			t.Fatalf("expected a claimable task, got none: %s", w.Body.String())
		}
		return resp.Task.ID, resp.Task.AskOnly, w.Body.String()
	}

	// FIFO claim order (same priority, created_at ASC): the discussion task
	// was seeded first, so it claims first.
	taskID, askOnly, body := claimAskOnly()
	if taskID != discussionTaskID {
		t.Fatalf("first claim = task %s, want discussion task %s: %s", taskID, discussionTaskID, body)
	}
	if !askOnly {
		t.Fatalf("discussion-container task must claim ask_only=true: %s", body)
	}

	taskID, askOnly, body = claimAskOnly()
	if taskID != ordinaryTaskID {
		t.Fatalf("second claim = task %s, want ordinary task %s: %s", taskID, ordinaryTaskID, body)
	}
	if askOnly {
		t.Fatalf("ordinary issue task must NOT claim ask_only: %s", body)
	}
}

// TestCompleteTask_DiscussionContainer_DoesNotSuppressTrivialDoneOutput pins
// the TSUG-002 exemption: a comment-triggered task on the Discussion
// container whose final output is trivial ("Done.") still lands a visible
// agent comment — "a DC activation always produces visible output" is a
// mechanism guarantee, not a prompt convention. The ordinary-issue companion
// test (TestCompleteTask_CommentTriggered_SuppressesTrivialDoneOutput) keeps
// the suppression for every other container.
func TestCompleteTask_DiscussionContainer_DoesNotSuppressTrivialDoneOutput(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	var number int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
		WHERE id = $1 RETURNING issue_counter
	`, testWorkspaceID).Scan(&number); err != nil {
		t.Fatalf("next issue number: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, origin_type, status, priority, number, position)
		VALUES ($1, 'member', $2, 'dc trivial exemption fixture', 'project_discussion', 'in_progress', 'none', $3, 0)
		RETURNING id
	`, testWorkspaceID, testUserID, number).Scan(&issueID); err != nil {
		t.Fatalf("setup: create discussion container issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	var triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, '@DiscussionCoordinator please summarize', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("setup: create trigger comment: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, trigger_comment_id,
			status, priority, started_at
		)
		VALUES ($1, $2, $3, $4, 'running', 0, now())
		RETURNING id
	`, agentID, runtimeID, issueID, triggerCommentID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create comment-triggered task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": "Done."},
		testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var content string
	if err := testPool.QueryRow(ctx, `
		SELECT content FROM comment
		WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2
		ORDER BY created_at DESC LIMIT 1
	`, issueID, agentID).Scan(&content); err != nil {
		t.Fatalf("expected a synthesized agent comment despite trivial output: %v", err)
	}
	if content != "Done." {
		t.Fatalf("synthesized comment content = %q, want Done.", content)
	}
}
