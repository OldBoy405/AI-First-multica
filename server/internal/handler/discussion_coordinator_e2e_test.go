package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ─── CR-2026-012 TASK-08: end-to-end acceptance chains ───────────────────
//
// These tests chain the real handler/service paths against the test DB the
// same way the production request flow does (trigger compute → enqueue →
// daemon claim → complete → next trigger), producing the DB-level evidence
// for AC-1/AC-2/AC-3 (silent boundary, activation with visible output,
// route-to-execution) and AC-4 (merge-forward end-to-end, claim side).

// dcE2EFixture wires a fully configured project: Discussion Coordinator +
// Team Agent bindings, both containers, and a member-authored discussion
// stream. The coordinator is the seeded workspace agent (runtime-bound,
// claim-able); the Team Agent is a fresh workspace-invocable agent.
type dcE2EFixture struct {
	ProjectID         string
	DiscussionIssueID string
	CoordinatorID     string
	TeamAgentID       string
}

func newDCE2EFixture(t *testing.T) dcE2EFixture {
	t.Helper()
	ctx := context.Background()

	var coordinatorID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&coordinatorID); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}
	teamAgentID := createHandlerTestAgent(t, "dc e2e team agent", []byte("{}"))

	settings, err := json.Marshal(map[string]string{
		service.ProjectSettingDiscussionCoordinatorID: coordinatorID,
		service.ProjectSettingTeamAgentID:             teamAgentID,
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, 'dc e2e fixture', $2)
		RETURNING id
	`, testWorkspaceID, settings).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	discussion, err := testHandler.IssueService.EnsureProjectDiscussionIssue(ctx, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(projectID), util.MustParseUUID(testUserID))
	if err != nil {
		t.Fatalf("ensure discussion container: %v", err)
	}
	return dcE2EFixture{
		ProjectID:         projectID,
		DiscussionIssueID: uuidToString(discussion.ID),
		CoordinatorID:     coordinatorID,
		TeamAgentID:       teamAgentID,
	}
}

func (fx dcE2EFixture) insertComment(t *testing.T, issueID, authorType, authorID, content string, parentID *string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id)
		VALUES ($1, $2, $3, $4, $5, 'comment', $6)
		RETURNING id
	`, issueID, testWorkspaceID, authorType, authorID, content, parentID).Scan(&id); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	return id
}

func (fx dcE2EFixture) discussionIssue(t *testing.T) (issueID string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT id::text FROM issue WHERE id = $1
	`, fx.DiscussionIssueID).Scan(&issueID); err != nil {
		t.Fatalf("load discussion issue: %v", err)
	}
	return issueID
}

func (fx dcE2EFixture) chatIssueID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id::text FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'
	`, fx.ProjectID).Scan(&id); err != nil {
		t.Fatalf("load chat container: %v", err)
	}
	return id
}

// noEscalationDelay matches the enqueue dispatch signature for tests that
// never exercise the deferred-fallback branch.
func noEscalationDelay() time.Duration { return 0 }

// TestDiscussionCoordinator_SilentBoundaryAndActivationChain is the AC-1 +
// AC-2 + AC-3 evidence chain:
//
//	AC-1: ordinary text and plain-text coordinator names enqueue NOTHING;
//	AC-2: @DC enqueues an ASK-ONLY task on the Discussion container, and a
//	      trivial completion output STILL lands a visible agent comment;
//	AC-3: the coordinator's @TeamAgent output re-targets onto the chat
//	      container (routing comment + task) with the activation human as
//	      the task originator.
func TestDiscussionCoordinator_SilentBoundaryAndActivationChain(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDCE2EFixture(t)
	ctx := context.Background()
	discussionID := fx.DiscussionIssueID

	// AC-1: silence. Neither an ordinary message nor a plain-text mention of
	// the coordinator's name may enqueue anything (DB-level zero-delta, the
	// CR-2026-009 AC-3 reading reapplied to the configured project).
	issue := loadIssueForE2E(t, discussionID)
	for _, content := range []string{
		"just chatting about the roadmap",
		"Handler Test Agent what do you think?", // plain text, no @-mention markup
	} {
		triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, content, nil, "member", testUserID, commentTriggerComputeOptions{})
		if len(triggers) != 0 {
			t.Fatalf("AC-1 violated: %q produced %d triggers", content, len(triggers))
		}
	}
	assertTaskCountForE2E(t, fx.ProjectID, 0)

	// AC-2 step 1: @DC activates — exactly one trigger, enqueued on the
	// Discussion container.
	activationID := fx.insertComment(t, discussionID, "member", testUserID,
		"[@DC](mention://agent/"+fx.CoordinatorID+") please coordinate this thread", nil)
	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue,
		"[@DC](mention://agent/"+fx.CoordinatorID+") please coordinate this thread",
		nil, "member", testUserID, commentTriggerComputeOptions{})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.CoordinatorID {
		t.Fatalf("AC-2 activation: expected exactly one DC trigger, got %+v", triggers)
	}
	triggerCommentUUID := util.MustParseUUID(activationID)
	// Enqueue through the real service path (the same call the comment handler
	// makes for the activation class) so the task carries its trigger comment.
	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, triggers[0].Agent.ID, triggerCommentUUID); err != nil {
		t.Fatalf("AC-2 activation enqueue: %v", err)
	}

	var dcTaskID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, discussionID, fx.CoordinatorID).Scan(&dcTaskID); err != nil {
		t.Fatalf("AC-2: expected an enqueued DC task on the Discussion container: %v", err)
	}

	// AC-2 step 2: the claim is ASK-ONLY (the read-only sandbox rides the
	// CR-2026-008 chain).
	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+testRuntimeID+"/tasks/claim", nil,
		testWorkspaceID, "dc-e2e-claim")
	req = withURLParam(req, "runtimeId", testRuntimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var claimResp struct {
		Task *struct {
			ID      string `json:"id"`
			AskOnly bool   `json:"ask_only"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &claimResp); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claimResp.Task == nil || claimResp.Task.ID != dcTaskID {
		t.Fatalf("AC-2: claimed task = %+v, want DC task %s", claimResp.Task, dcTaskID)
	}
	if !claimResp.Task.AskOnly {
		t.Fatalf("AC-2 violated: Discussion-container task must claim ask_only=true")
	}

	// Simulate the daemon's StartTask handshake flipping the claim to running
	// (CompleteTask only finalizes running tasks).
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, dcTaskID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// AC-2 step 3: completion with a TRIVIAL output must still produce a
	// visible agent comment on the Discussion container (DD-4 exemption —
	// "a DC activation always produces visible output" is mechanism, not
	// prompt convention).
	cw := httptest.NewRecorder()
	creq := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+dcTaskID+"/complete",
		map[string]any{"output": "Done."}, testWorkspaceID, "dc-e2e-daemon")
	cctx := chi.NewRouteContext()
	cctx.URLParams.Add("taskId", dcTaskID)
	creq = creq.WithContext(context.WithValue(creq.Context(), chi.RouteCtxKey, cctx))
	testHandler.CompleteTask(cw, creq)
	if cw.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", cw.Code, cw.Body.String())
	}
	var visibleOutput string
	if err := testPool.QueryRow(ctx, `
		SELECT content FROM comment
		WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2
		ORDER BY created_at DESC LIMIT 1
	`, discussionID, fx.CoordinatorID).Scan(&visibleOutput); err != nil {
		t.Fatalf("AC-2 violated: no visible DC output on the Discussion container: %v", err)
	}
	if visibleOutput != "Done." {
		t.Fatalf("AC-2: visible output = %q, want the trivial output preserved", visibleOutput)
	}

	// AC-3: the coordinator's @TeamAgent output (its completion comment
	// hangs under the human's activation comment) re-targets onto the chat
	// container: a routing comment + a Team Agent task, originator = the
	// activation human.
	routeContent := "[@Team](mention://agent/" + fx.TeamAgentID + ") please implement what we converged on"
	routeCommentID := fx.insertComment(t, discussionID, "agent", fx.CoordinatorID, routeContent, &activationID)
	routeTriggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, routeContent, nil, "agent", fx.CoordinatorID, commentTriggerComputeOptions{})
	if len(routeTriggers) != 1 {
		t.Fatalf("AC-3: expected one routing trigger, got %+v", routeTriggers)
	}
	testHandler.enqueueSingleCommentTrigger(ctx, issue, util.MustParseUUID(routeCommentID), routeTriggers[0], noEscalationDelay)

	chatID := fx.chatIssueID(t)
	var routeOnChat string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM comment WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2
	`, chatID, fx.CoordinatorID).Scan(&routeOnChat); err != nil {
		t.Fatalf("AC-3 violated: no routing comment on the chat container: %v", err)
	}
	var teamTaskAgent, teamTaskTrigger, originator *string
	if err := testPool.QueryRow(ctx, `
		SELECT agent_id::text, trigger_comment_id::text, originator_user_id::text FROM agent_task_queue
		WHERE issue_id = $1
	`, chatID).Scan(&teamTaskAgent, &teamTaskTrigger, &originator); err != nil {
		t.Fatalf("AC-3 violated: no Team Agent task on the chat container: %v", err)
	}
	if teamTaskAgent == nil || *teamTaskAgent != fx.TeamAgentID {
		t.Fatalf("AC-3: chat task agent = %v, want Team Agent %s", teamTaskAgent, fx.TeamAgentID)
	}
	if teamTaskTrigger == nil || *teamTaskTrigger != routeOnChat {
		t.Fatalf("AC-3: chat task trigger = %v, want routing comment %s", teamTaskTrigger, routeOnChat)
	}
	if originator == nil || *originator != testUserID {
		t.Fatalf("AC-3: chat task originator = %v, want activation human %s", originator, testUserID)
	}
	// And nothing executable leaked onto the Discussion container itself.
	var discussionTasks int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, discussionID, fx.TeamAgentID).Scan(&discussionTasks); err != nil {
		t.Fatalf("count discussion tasks: %v", err)
	}
	if discussionTasks != 0 {
		t.Fatalf("AC-3 violated: %d Team Agent tasks hang on the Discussion container", discussionTasks)
	}
}

// TestMergeForward_EndToEndClaimCarriesMergedStructure is the AC-4 evidence:
// the merge-forward endpoint creates exactly ONE comment + ONE task on the
// chat container, and the daemon claim for that task carries the full merged
// structure as its trigger content.
func TestMergeForward_EndToEndClaimCarriesMergedStructure(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	wireChatCatalogPort()
	fx := newDCE2EFixture(t)
	ctx := context.Background()
	discussionID := fx.DiscussionIssueID

	c1 := fx.insertComment(t, discussionID, "member", testUserID, "the API needs retries", nil)
	c2 := fx.insertComment(t, discussionID, "member", testUserID, "agreed, plus a metric", nil)

	req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/merge-forward", map[string]any{
		"comment_ids": []string{c1, c2},
		"register_cr": false,
	})
	req = withURLParam(req, "id", fx.ProjectID)
	rr := httptest.NewRecorder()
	testHandler.MergeForwardDiscussion(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp SendProjectChatMessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	chatID := fx.chatIssueID(t)
	var commentCount, taskCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, chatID).Scan(&commentCount); err != nil {
		t.Fatalf("count chat comments: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, chatID).Scan(&taskCount); err != nil {
		t.Fatalf("count chat tasks: %v", err)
	}
	if commentCount != 1 || taskCount != 1 {
		t.Fatalf("AC-4 violated: one confirmation must yield 1 comment + 1 task, got %d comments / %d tasks", commentCount, taskCount)
	}

	// The claim carries the full merged structure as trigger content.
	w := httptest.NewRecorder()
	creq := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+testRuntimeID+"/tasks/claim", nil,
		testWorkspaceID, "dc-e2e-merge-claim")
	creq = withURLParam(creq, "runtimeId", testRuntimeID)
	testHandler.ClaimTaskByRuntime(w, creq)
	if w.Code != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var claimResp struct {
		Task *struct {
			ID                    string `json:"id"`
			TriggerCommentID      string `json:"trigger_comment_id"`
			TriggerCommentContent string `json:"trigger_comment_content"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &claimResp); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claimResp.Task == nil {
		t.Fatalf("AC-4: expected a claimable merged task, got none: %s", w.Body.String())
	}
	if claimResp.Task.TriggerCommentID != resp.CommentID {
		t.Fatalf("AC-4: claim trigger = %s, want merged comment %s", claimResp.Task.TriggerCommentID, resp.CommentID)
	}
	content := claimResp.Task.TriggerCommentContent
	for _, want := range []string{
		"## Trigger message",
		"> the API needs retries",
		"## Conversation history (2 messages)",
		"the API needs retries",
		"agreed, plus a metric",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("AC-4: merged trigger content missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "升级为 CR") {
		t.Fatalf("AC-4: register_cr=false must not append the instruction block:\n%s", content)
	}
}

// loadIssueForE2E fetches the issue row for trigger computation.
func loadIssueForE2E(t *testing.T, issueID string) db.Issue {
	t.Helper()
	row, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue %s: %v", issueID, err)
	}
	return row
}

// assertTaskCountForE2E pins the DB-level zero-delta reading (AC-1).
func assertTaskCountForE2E(t *testing.T, projectID string, want int) {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)
	`, projectID).Scan(&count); err != nil {
		t.Fatalf("count project tasks: %v", err)
	}
	if count != want {
		t.Fatalf("AC-1 violated: project task count = %d, want %d", count, want)
	}
}
