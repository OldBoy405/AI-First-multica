package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// testChatCatalogPort is a static in-process ChatCatalogPort for handler
// tests: a one-model claude catalog on both the cache and live paths. The
// real port is only wired in cmd/server (Handler.WireChatCatalog); handler
// tests that drive the §4.3 validation install this one on the shared
// handler's services (idempotent — wiring twice is harmless).
type testChatCatalogPort struct{}

func (*testChatCatalogPort) CacheLoad(context.Context, string) (agent.Catalog, bool, error) {
	return agent.Catalog{Models: []agent.Model{{ID: "claude-opus-5", Default: true}}}, true, nil
}

func (*testChatCatalogPort) LiveLoad(context.Context, string) (agent.Catalog, error) {
	return agent.Catalog{Models: []agent.Model{{ID: "claude-opus-5", Default: true}}}, nil
}

// wireChatCatalogPort installs the static test catalog on the shared test
// handler's IssueService and TaskService so send/container/merge-forward
// requests can pass §4.3.
func wireChatCatalogPort() {
	if testHandler == nil {
		return
	}
	port := &testChatCatalogPort{}
	testHandler.IssueService.ChatCatalog = port
	testHandler.TaskService.ChatCatalog = port
}

// mergeForwardFixture wires a project with a bound Team Agent plus its
// Discussion container, so merge-forward selections can be validated and
// forwarded end to end through the handler.
type mergeForwardFixture struct {
	ProjectID         string
	TeamAgentID       string
	DiscussionIssueID string
}

func newMergeForwardFixture(t *testing.T, label string, settingsExtra map[string]any) mergeForwardFixture {
	t.Helper()
	ctx := context.Background()
	wireChatCatalogPort()

	// Agent names are unique per workspace — every fixture needs its own label.
	teamAgentID := createHandlerTestAgent(t, "merge-forward team agent "+label, []byte("{}"))

	settings := map[string]any{"team_agent_id": teamAgentID}
	for k, v := range settingsExtra {
		settings[k] = v
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, 'merge-forward fixture', $2)
		RETURNING id
	`, testWorkspaceID, raw).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project_presenter_grant WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project_chat_session WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	discussionIssue, err := testHandler.IssueService.EnsureProjectDiscussionIssue(ctx, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(projectID), util.MustParseUUID(testUserID))
	if err != nil {
		t.Fatalf("ensure discussion container: %v", err)
	}
	return mergeForwardFixture{
		ProjectID:         projectID,
		TeamAgentID:       teamAgentID,
		DiscussionIssueID: uuidToString(discussionIssue.ID),
	}
}

func (fx mergeForwardFixture) insertDiscussionComment(t *testing.T, content string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, $4, 'comment')
		RETURNING id
	`, fx.DiscussionIssueID, testWorkspaceID, testUserID, content).Scan(&id); err != nil {
		t.Fatalf("insert discussion comment: %v", err)
	}
	return id
}

func callMergeForward(t *testing.T, projectID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/projects/"+projectID+"/chat/merge-forward", body)
	req = withURLParam(req, "id", projectID)
	rr := httptest.NewRecorder()
	testHandler.MergeForwardDiscussion(rr, req)
	return rr
}

// chatPaneState loads the chat container's comments and team-agent task count
// for ghost assertions.
func chatPaneState(t *testing.T, projectID string) (commentIDs []string, taskCount int) {
	t.Helper()
	ctx := context.Background()
	rows, err := testPool.Query(ctx, `
		SELECT c.id::text FROM comment c JOIN issue i ON i.id = c.issue_id
		WHERE i.project_id = $1 AND i.origin_type = 'project_chat'
	`, projectID)
	if err != nil {
		t.Fatalf("query chat comments: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan chat comment: %v", err)
		}
		commentIDs = append(commentIDs, id)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id IN (
			SELECT id FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'
		)
	`, projectID).Scan(&taskCount); err != nil {
		t.Fatalf("count chat tasks: %v", err)
	}
	return commentIDs, taskCount
}

// TestMergeForwardDiscussion_MergesIntoSingleCommentAndTask pins DD-7/FR-5:
// one confirmation = exactly one merged comment + one Team Agent task. The
// merged content carries the three-part structure (trigger quote, history
// list with count), renders in created_at order regardless of the request
// order, and de-duplicates repeated ids. register_cr=false leaves the
// instruction block out.
func TestMergeForwardDiscussion_MergesIntoSingleCommentAndTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "merge", nil)
	ctx := context.Background()

	c1 := fx.insertDiscussionComment(t, "first: the API times out under load")
	c2 := fx.insertDiscussionComment(t, "second: we should add a retry with backoff")
	c3 := fx.insertDiscussionComment(t, "third: agreed, plus a metric for it")

	// Shuffled order + a duplicate id: the merged message must still render
	// chronologically, once per message.
	rr := callMergeForward(t, fx.ProjectID, map[string]any{
		"comment_ids": []string{c3, c1, c2, c1},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp SendProjectChatMessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.CommentID == "" || resp.TaskID == "" {
		t.Fatalf("expected comment_id and task_id, got %+v", resp)
	}

	// Exactly one comment on the chat pane, carrying the three-part structure.
	commentIDs, taskCount := chatPaneState(t, fx.ProjectID)
	if len(commentIDs) != 1 {
		t.Fatalf("expected exactly 1 merged comment on the chat pane, got %d", len(commentIDs))
	}
	if commentIDs[0] != resp.CommentID {
		t.Fatalf("chat comment = %s, want response comment %s", commentIDs[0], resp.CommentID)
	}
	if taskCount != 1 {
		t.Fatalf("expected exactly 1 Team Agent task, got %d", taskCount)
	}

	var content string
	if err := testPool.QueryRow(ctx, `SELECT content FROM comment WHERE id = $1`, resp.CommentID).Scan(&content); err != nil {
		t.Fatalf("load merged comment: %v", err)
	}
	for _, want := range []string{
		"## Trigger message",
		"> first: the API times out under load", // earliest message quoted in full
		"## Conversation history (3 messages)",
		"first: the API times out under load",
		"second: we should add a retry with backoff",
		"third: agreed, plus a metric for it",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("merged content missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "升级为 CR") {
		t.Fatalf("register_cr=false must not append the instruction block:\n%s", content)
	}
	if i1, i2 := strings.Index(content, "first:"), strings.Index(content, "third:"); i1 > i2 {
		t.Fatalf("history must render created_at ascending:\n%s", content)
	}

	// The task hangs on the chat container, triggered by the merged comment.
	var taskAgentID, triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		SELECT agent_id, trigger_comment_id FROM agent_task_queue
		WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1 AND origin_type = 'project_chat')
	`, fx.ProjectID).Scan(&taskAgentID, &triggerCommentID); err != nil {
		t.Fatalf("load chat task: %v", err)
	}
	if taskAgentID != fx.TeamAgentID {
		t.Fatalf("task agent = %s, want team agent %s", taskAgentID, fx.TeamAgentID)
	}
	if triggerCommentID != resp.CommentID {
		t.Fatalf("task trigger = %s, want merged comment %s", triggerCommentID, resp.CommentID)
	}
}

// TestMergeForwardDiscussion_RegisterCRAppendsInstructionBlock pins DD-8:
// register_cr=true appends the requirement-register instruction block as
// visible comment text (zero server-side CR writes).
func TestMergeForwardDiscussion_RegisterCRAppendsInstructionBlock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "register-cr", nil)
	ctx := context.Background()

	c1 := fx.insertDiscussionComment(t, "we need a durable export feature")

	rr := callMergeForward(t, fx.ProjectID, map[string]any{
		"comment_ids": []string{c1},
		"register_cr": true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp SendProjectChatMessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var content string
	if err := testPool.QueryRow(ctx, `SELECT content FROM comment WHERE id = $1`, resp.CommentID).Scan(&content); err != nil {
		t.Fatalf("load merged comment: %v", err)
	}
	if !strings.Contains(content, "升级为 CR") || !strings.Contains(content, "requirement-register") {
		t.Fatalf("register_cr=true must append the instruction block:\n%s", content)
	}
}

// TestMergeForwardDiscussion_InvalidSelections pins the authorization surface:
// empty selection, over-cap selection, malformed ids, an ordinary-issue
// comment, and another project's Discussion comment all get 400
// invalid_comment_selection.
func TestMergeForwardDiscussion_InvalidSelections(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "invalid", nil)

	valid := fx.insertDiscussionComment(t, "a legitimate discussion message")

	// An ordinary (non-container) issue comment in the same project.
	var ordinaryIssueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, project_id, creator_type, creator_id, title, status, priority, number, position)
		VALUES ($1, $2, 'member', $3, 'ordinary issue', 'todo', 'none',
			(SELECT COALESCE(MAX(number), 0) + 100 FROM issue WHERE workspace_id = $1), 0)
		RETURNING id
	`, testWorkspaceID, fx.ProjectID, testUserID).Scan(&ordinaryIssueID); err != nil {
		t.Fatalf("create ordinary issue: %v", err)
	}
	var ordinaryCommentID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'not a discussion message', 'comment')
		RETURNING id
	`, ordinaryIssueID, testWorkspaceID, testUserID).Scan(&ordinaryCommentID); err != nil {
		t.Fatalf("create ordinary comment: %v", err)
	}

	// Another project with its own Discussion container.
	other := newMergeForwardFixture(t, "invalid-foreign", nil)
	foreignComment := other.insertDiscussionComment(t, "another project's discussion")

	overCap := make([]string, 51)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1)
	}

	cases := []struct {
		name string
		ids  []string
	}{
		{"empty selection", []string{}},
		{"over the 50 cap", overCap},
		{"ordinary issue comment", []string{ordinaryCommentID}},
		{"foreign project discussion comment", []string{foreignComment}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := callMergeForward(t, fx.ProjectID, map[string]any{"comment_ids": c.ids})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "invalid_comment_selection") {
				t.Fatalf("expected invalid_comment_selection code, got %s", rr.Body.String())
			}
		})
	}

	// Malformed ids hit the shared parseUUIDSliceOrBadRequest writer: still a
	// 400 rejection, generic body.
	t.Run("malformed id", func(t *testing.T) {
		rr := callMergeForward(t, fx.ProjectID, map[string]any{"comment_ids": []string{"not-a-uuid"}})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	// Sanity: the untouched valid comment still forwards fine after the rejects.
	rr := callMergeForward(t, fx.ProjectID, map[string]any{"comment_ids": []string{valid}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("valid selection after rejects: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMergeForwardDiscussion_PresenterRequiredForPlainMember pins the 403
// branch: a plain member without a presenter grant cannot forward (the
// CR-2026-010 single-writer guard lives in the shared send kernel).
func TestMergeForwardDiscussion_PresenterRequiredForPlainMember(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "presenter-403", nil)
	c1 := fx.insertDiscussionComment(t, "a message a plain member wants to forward")

	memberID := createPlainMemberUser(t, "merge-forward-403")
	req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/merge-forward", map[string]any{
		"comment_ids": []string{c1},
	})
	req.Header.Set("X-User-ID", memberID)
	req = withURLParam(req, "id", fx.ProjectID)
	rr := httptest.NewRecorder()
	testHandler.MergeForwardDiscussion(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "presenter_required") {
		t.Fatalf("expected presenter_required code, got %s", rr.Body.String())
	}
	if comments, tasks := chatPaneState(t, fx.ProjectID); len(comments) != 0 || tasks != 0 {
		t.Fatalf("rejected forward must persist nothing: comments=%d tasks=%d", len(comments), tasks)
	}
}

// TestMergeForwardDiscussion_QueueFull429NoGhost pins the 429 branch: an
// active presenter (plain member) forwarding into a full project queue gets
// project_queue_full and NO ghost comment on the chat pane — the shared
// kernel's front-load guard rejects before anything is created.
func TestMergeForwardDiscussion_QueueFull429NoGhost(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "queue-full", map[string]any{"team_agent_queue_limit": 1})
	ctx := context.Background()

	// Fill the project's shared queue via an ordinary issue.
	var fillerIssueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, creator_type, creator_id, title, status, priority, number, position)
		VALUES ($1, $2, 'member', $3, 'queue filler', 'todo', 'none',
			(SELECT COALESCE(MAX(number), 0) + 200 FROM issue WHERE workspace_id = $1), 0)
		RETURNING id
	`, testWorkspaceID, fx.ProjectID, testUserID).Scan(&fillerIssueID); err != nil {
		t.Fatalf("create filler issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		SELECT $1, runtime_id, $2, 'queued', 0 FROM agent WHERE id = $1
	`, fx.TeamAgentID, fillerIssueID); err != nil {
		t.Fatalf("seed queued task: %v", err)
	}

	// The plain member holds the active presenter grant → passes the
	// single-writer guard but hits the capacity guard.
	presenterID := createPlainMemberUser(t, "merge-forward-429")
	if _, err := testPool.Exec(ctx, `
		INSERT INTO project_presenter_grant (workspace_id, project_id, user_id, status, granted_by)
		VALUES ($1, $2, $3, 'active', $4)
	`, testWorkspaceID, fx.ProjectID, presenterID, testUserID); err != nil {
		t.Fatalf("seed presenter grant: %v", err)
	}

	c1 := fx.insertDiscussionComment(t, "a message forwarded into a full queue")
	req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/merge-forward", map[string]any{
		"comment_ids": []string{c1},
	})
	req.Header.Set("X-User-ID", presenterID)
	req = withURLParam(req, "id", fx.ProjectID)
	rr := httptest.NewRecorder()
	testHandler.MergeForwardDiscussion(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "project_queue_full") {
		t.Fatalf("expected project_queue_full code, got %s", rr.Body.String())
	}
	if comments, tasks := chatPaneState(t, fx.ProjectID); len(comments) != 0 || tasks != 0 {
		t.Fatalf("429 must leave no ghost comment and no chat-container task: comments=%d tasks=%d", len(comments), tasks)
	}
}

// TestSendProjectChatMessage_AttachmentIDs pins the messages endpoint's
// session_id + draft-attachment contract (TASK-07 / §4.5): a draft
// attachment (all five bind targets empty) is bound to the created comment
// INSIDE the send transaction; omitting attachment_ids leaves behavior
// unchanged; a missing session_id rejects; malformed ids get the generic 400.
func TestSendProjectChatMessage_AttachmentIDs(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newMergeForwardFixture(t, "attachments", nil)
	ctx := context.Background()

	// Ensure the active session (what GET does) — messages requires its id.
	view, err := testHandler.IssueService.EnsureProjectChatSession(ctx, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(fx.ProjectID), util.MustParseUUID(testUserID))
	if err != nil {
		t.Fatalf("ensure chat session: %v", err)
	}

	// A composer-uploaded DRAFT attachment: all five bind targets empty.
	var attachmentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO attachment (workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
		VALUES ($1, 'member', $2, 'spec.pdf', 'https://cdn.test/spec.pdf', 'application/pdf', 1024)
		RETURNING id::text
	`, testWorkspaceID, testUserID).Scan(&attachmentID); err != nil {
		t.Fatalf("seed draft attachment: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, attachmentID)
	})

	send := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/messages", body)
		req = withURLParam(req, "id", fx.ProjectID)
		rr := httptest.NewRecorder()
		testHandler.SendProjectChatMessage(rr, req)
		return rr
	}

	// With session_id + attachment_ids: 201 with the full id set; the draft
	// binds to the created comment inside the send transaction.
	rr := send(map[string]any{
		"session_id":     view.SessionID,
		"content":        "see the attached spec",
		"attachment_ids": []string{attachmentID},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp SendProjectChatMessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.SessionID != view.SessionID {
		t.Fatalf("session_id = %q, want request value %q", resp.SessionID, view.SessionID)
	}
	if resp.IssueID == "" || resp.CommentID == "" || resp.TaskID == "" {
		t.Fatalf("send response missing ids: %+v", resp)
	}
	var linkedComment, linkedIssue, linkedTask *string
	if err := testPool.QueryRow(ctx, `SELECT comment_id::text, issue_id::text, task_id::text FROM attachment WHERE id = $1`, attachmentID).
		Scan(&linkedComment, &linkedIssue, &linkedTask); err != nil {
		t.Fatalf("query attachment link: %v", err)
	}
	if linkedComment == nil || *linkedComment != resp.CommentID {
		t.Fatalf("attachment comment_id = %v, want %s", linkedComment, resp.CommentID)
	}
	if linkedIssue == nil || *linkedIssue != resp.IssueID {
		t.Fatalf("attachment issue_id = %v, want %s", linkedIssue, resp.IssueID)
	}
	if linkedTask == nil || *linkedTask != resp.TaskID {
		t.Fatalf("attachment task_id = %v, want %s", linkedTask, resp.TaskID)
	}

	// Simulate the daemon claiming+completing the first task: the one-pending-
	// task-per-(issue,agent) index would otherwise reject the follow-up send
	// (a real daemon claims within milliseconds).
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, resp.TaskID); err != nil {
		t.Fatalf("complete first task: %v", err)
	}

	// Without the field: unchanged behavior, the attachment stays untouched.
	rr = send(map[string]any{"session_id": view.SessionID, "content": "a plain follow-up"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := testPool.QueryRow(ctx, `SELECT comment_id::text FROM attachment WHERE id = $1`, attachmentID).Scan(&linkedComment); err != nil {
		t.Fatalf("query attachment link: %v", err)
	}
	if linkedComment == nil || *linkedComment != resp.CommentID {
		t.Fatalf("attachment must stay linked to the FIRST comment, got %v", linkedComment)
	}

	// Missing session_id → 400.
	rr = send(map[string]any{"content": "no session id"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without session_id, got %d: %s", rr.Code, rr.Body.String())
	}

	// Malformed attachment_ids → 400 and nothing persisted.
	before, _ := chatPaneState(t, fx.ProjectID)
	rr = send(map[string]any{"session_id": view.SessionID, "content": "bad attachments", "attachment_ids": []string{"not-a-uuid"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed attachment_ids, got %d: %s", rr.Code, rr.Body.String())
	}
	after, _ := chatPaneState(t, fx.ProjectID)
	if len(after) != len(before) {
		t.Fatalf("malformed attachment_ids must not persist a comment: before=%d after=%d", len(before), len(after))
	}
}
