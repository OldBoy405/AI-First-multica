package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CR-2026-059 TASK-02/03 DB-backed acceptance vectors for the shared
// Discussion session endpoints. Every test seeds its own project/session and
// cleans up; the kind dispatch paths run against the real test database
// (migrations 481-490 must be applied, same as every other DB-backed suite).

// discussionSharedFixture wires a project and its shared Discussion session.
type discussionSharedFixture struct {
	ProjectID   string
	SessionID   string
	Coordinator string // bound coordinator agent id, "" when unconfigured
}

func newDiscussionSharedFixture(t *testing.T, label string, coordinatorID string) discussionSharedFixture {
	t.Helper()
	wireChatCatalogPort()

	settings := map[string]any{}
	if coordinatorID != "" {
		settings[service.ProjectSettingDiscussionCoordinatorID] = coordinatorID
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var projectID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, $2, $3) RETURNING id
	`, testWorkspaceID, "shared discussion fixture "+label, raw).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_idempotency WHERE scope_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE chat_session_id IN (SELECT id FROM chat_session WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id IN (SELECT id FROM chat_session WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	resp := callGetProjectDiscussion(t, projectID)
	return discussionSharedFixture{
		ProjectID:   projectID,
		SessionID:   resp.SessionID,
		Coordinator: coordinatorID,
	}
}

// withSharedWorkspaceCtx injects the workspace context (plus the member row
// when one exists) — the chi middleware normally does this; the test harness
// calls handlers directly, and ctxWorkspaceID otherwise returns empty.
func withSharedWorkspaceCtx(t *testing.T, req *http.Request, userID string) *http.Request {
	t.Helper()
	memberRow, _ := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      util.MustParseUUID(userID),
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
	})
	return req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, memberRow))
}

func callGetProjectDiscussion(t *testing.T, projectID string) ProjectDiscussionResponse {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/projects/"+projectID+"/discussion", nil), "id", projectID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr := httptest.NewRecorder()
	testHandler.GetProjectDiscussion(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET discussion status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp ProjectDiscussionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode discussion response: %v", err)
	}
	return resp
}

func callSharedSend(t *testing.T, sessionID string, body map[string]any, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/chat/sessions/"+sessionID+"/messages", body)
	req = withURLParam(req, "sessionId", sessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rr := httptest.NewRecorder()
	testHandler.SendChatMessage(rr, req)
	return rr
}

func callSharedSendAsUser(t *testing.T, sessionID, userID string, body map[string]any, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/chat/sessions/"+sessionID+"/messages", body)
	req = withURLParam(req, "sessionId", sessionID)
	req.Header.Set("X-User-ID", userID)
	req = withSharedWorkspaceCtx(t, req, userID)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rr := httptest.NewRecorder()
	testHandler.SendChatMessage(rr, req)
	return rr
}

func callSharedList(t *testing.T, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/chat/sessions/"+sessionID+"/messages", nil), "sessionId", sessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr := httptest.NewRecorder()
	testHandler.ListChatMessages(rr, req)
	return rr
}

func decodeDiscussionError(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	return body
}

// seedSharedUser creates a bare user row (no membership) for outsider
// vectors, cleaned up with the test.
func seedSharedUser(t *testing.T, label string) string {
	t.Helper()
	var uid string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		label, fmt.Sprintf("%s@multica.test", label)).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, uid) })
	return uid
}

// seedSharedMember creates a user + workspace member pair for the given role.
func seedSharedMember(t *testing.T, label, role string) string {
	t.Helper()
	uid := seedSharedUser(t, label)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		testWorkspaceID, uid, role); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, uid)
	})
	return uid
}

func TestSharedDiscussionSend_OrdinaryMessageNoTaskAndAuthorColumns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionSharedFixture(t, "ordinary", "")
	ctx := context.Background()

	// Missing Idempotency-Key -> 400 idempotency_key_required, zero writes.
	rr := callSharedSend(t, fx.SessionID, map[string]any{"content": "hello"}, "")
	if rr.Code != 400 || decodeDiscussionError(t, rr)["code"] != "idempotency_key_required" {
		t.Fatalf("missing key: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Empty content and no attachments -> 400 invalid_discussion_message.
	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "   "}, "key-empty")
	if rr.Code != 400 || decodeDiscussionError(t, rr)["code"] != "invalid_discussion_message" {
		t.Fatalf("empty content: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Ordinary message (AC-3): 201, task_id null, author columns written.
	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "hello world"}, "key-1")
	if rr.Code != 201 {
		t.Fatalf("send status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp DiscussionSendResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	if resp.SessionID != fx.SessionID || resp.MessageID == "" {
		t.Fatalf("unexpected send response: %+v", resp)
	}
	if resp.IssueID != nil {
		t.Fatalf("issue_id must stay null, got %q", *resp.IssueID)
	}
	if resp.TaskID != nil {
		t.Fatalf("ordinary message must not enqueue a task, got %q", *resp.TaskID)
	}
	var authorType, authorID string
	if err := testPool.QueryRow(ctx, `
		SELECT author_type, author_id::text FROM chat_message WHERE id = $1
	`, resp.MessageID).Scan(&authorType, &authorID); err != nil {
		t.Fatalf("read message author: %v", err)
	}
	if authorType != "member" || authorID != testUserID {
		t.Fatalf("author columns = (%s, %s), want (member, %s)", authorType, authorID, testUserID)
	}
	var taskCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, fx.SessionID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("ordinary message enqueued %d task(s), want 0 (AC-3)", taskCount)
	}

	// Idempotent replay (AC-26): same key + same fingerprint -> same message id,
	// exactly one stored row.
	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "hello world"}, "key-1")
	if rr.Code != 201 {
		t.Fatalf("replay status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var replay DiscussionSendResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replay.MessageID != resp.MessageID {
		t.Fatalf("replay message = %s, want %s", replay.MessageID, resp.MessageID)
	}
	var rowCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fx.SessionID).Scan(&rowCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("replay wrote %d message rows, want 1 (AC-26)", rowCount)
	}
	// Different fingerprint under the same key -> 409 idempotency_key_reused.
	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "different"}, "key-1")
	if rr.Code != 409 || decodeDiscussionError(t, rr)["code"] != "idempotency_key_reused" {
		t.Fatalf("fingerprint conflict: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSharedDiscussionSend_CoordinatorTaskWithoutIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// Unconfigured coordinator: analyze -> 409 not_configured, zero writes.
	fxNone := newDiscussionSharedFixture(t, "no-coordinator", "")
	rr := callSharedSend(t, fxNone.SessionID, map[string]any{"content": "please analyze this", "coordinator_request": "analyze"}, "key-nc")
	if rr.Code != 409 || decodeDiscussionError(t, rr)["code"] != "discussion_coordinator_not_configured" {
		t.Fatalf("unconfigured coordinator: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fxNone.SessionID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("409 not_configured left %d message row(s), want 0 (AC-11 zero writes)", n)
	}

	// Configured coordinator: the seeded workspace agent is the coordinator.
	var coordinatorID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&coordinatorID); err != nil {
		t.Fatalf("load seeded coordinator: %v", err)
	}
	fx := newDiscussionSharedFixture(t, "coordinator", coordinatorID)
	ctx := context.Background()

	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "please analyze this", "coordinator_request": "analyze"}, "key-coord")
	if rr.Code != 201 {
		t.Fatalf("coordinator send status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp DiscussionSendResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TaskID == nil {
		t.Fatal("coordinator turn must enqueue a task")
	}
	// AC-4: the task is issue-free, session-bound, and carries the chat_config
	// snapshot (the §4.2 preflight resolved/validated it).
	var issueID, sessionID, originatorSource *string
	var contextJSON []byte
	if err := testPool.QueryRow(ctx, `
		SELECT issue_id::text, chat_session_id::text, originator_source::text, context
		FROM agent_task_queue WHERE id = $1
	`, *resp.TaskID).Scan(&issueID, &sessionID, &originatorSource, &contextJSON); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if issueID != nil {
		t.Fatalf("coordinator task issue_id = %q, want NULL (AC-4)", *issueID)
	}
	if sessionID == nil || *sessionID != fx.SessionID {
		t.Fatalf("task chat_session_id = %v, want %q", sessionID, fx.SessionID)
	}
	var taskContext map[string]any
	if err := json.Unmarshal(contextJSON, &taskContext); err != nil || taskContext["chat_config"] == nil {
		t.Fatalf("task context missing chat_config: %s", contextJSON)
	}
	if originatorSource == nil || *originatorSource != "direct_human" {
		t.Fatalf("originator_source = %v, want direct_human", originatorSource)
	}
	// The user message is bound to the task's input batch.
	var messageTaskID string
	if err := testPool.QueryRow(ctx, `SELECT task_id::text FROM chat_message WHERE id = $1`, resp.MessageID).Scan(&messageTaskID); err != nil {
		t.Fatalf("read message task: %v", err)
	}
	if messageTaskID != *resp.TaskID {
		t.Fatalf("message task_id = %q, want %q", messageTaskID, *resp.TaskID)
	}
}

func TestSharedDiscussionSend_CrossUploaderDraftRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionSharedFixture(t, "cross-uploader", "")
	ctx := context.Background()

	// Member B uploads an unbound draft; member A (testUserID) tries to bind it.
	otherUser := seedSharedMember(t, "cross-uploader-b", "member")
	var draftID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO attachment (workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
		VALUES ($1, 'member', $2, 'b-draft.txt', 'http://store/b-draft', 'text/plain', 4)
		RETURNING id::text
	`, testWorkspaceID, otherUser).Scan(&draftID); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, draftID)
	})

	rr := callSharedSend(t, fx.SessionID, map[string]any{"content": "borrow B's file", "attachment_ids": []string{draftID}}, "key-cross")
	if rr.Code != 409 || decodeDiscussionError(t, rr)["code"] != "attachment_already_bound" {
		t.Fatalf("cross-uploader bind: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Zero residue (AC-13): no message, no bound attachment, no reservation.
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fx.SessionID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed bind left %d message row(s), want 0", count)
	}
	var chatSessionID *string
	if err := testPool.QueryRow(ctx, `SELECT chat_session_id::text FROM attachment WHERE id = $1`, draftID).Scan(&chatSessionID); err != nil {
		t.Fatalf("read draft: %v", err)
	}
	if chatSessionID != nil {
		t.Fatalf("failed bind left draft bound to %q, want NULL", *chatSessionID)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_idempotency WHERE scope_id = $1`, fx.SessionID).Scan(&count); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed bind left %d reservation row(s), want 0 (B-AUTH-2 zero residue)", count)
	}
}

func TestSharedDiscussionSession_GatesAndClosure(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionSharedFixture(t, "gates", "")
	ctx := context.Background()

	// Non-member: GET project path -> 403 forbidden_project_discussion.
	outsider := seedSharedUser(t, "shared-outsider")
	req := withURLParam(newRequest("GET", "/api/projects/"+fx.ProjectID+"/discussion", nil), "id", fx.ProjectID)
	req.Header.Set("X-User-ID", outsider)
	req = withSharedWorkspaceCtx(t, req, outsider)
	rr := httptest.NewRecorder()
	testHandler.GetProjectDiscussion(rr, req)
	if rr.Code != 403 || decodeDiscussionError(t, rr)["code"] != "forbidden_project_discussion" {
		t.Fatalf("non-member GET: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Non-member: session path -> 404 chat_session_not_found (AC-20).
	rr = callSharedSendAsUser(t, fx.SessionID, outsider, map[string]any{"content": "hi"}, "key-outsider")
	if rr.Code != 404 || decodeDiscussionError(t, rr)["code"] != "chat_session_not_found" {
		t.Fatalf("non-member send: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Ordinary member send works (member gate positive vector).
	memberID := seedSharedMember(t, "shared-member", "member")
	rr = callSharedSendAsUser(t, fx.SessionID, memberID, map[string]any{"content": "member hello"}, "key-member")
	if rr.Code != 201 {
		t.Fatalf("member send: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Shared config PATCH: plain member -> 403 forbidden_chat_config.
	req = withURLParam(newRequest("PATCH", "/api/chat/sessions/"+fx.SessionID+"/config", map[string]any{"model": "x"}), "sessionId", fx.SessionID)
	req.Header.Set("X-User-ID", memberID)
	req = withSharedWorkspaceCtx(t, req, memberID)
	rr = httptest.NewRecorder()
	testHandler.PatchChatSessionConfig(rr, req)
	if rr.Code != 403 || decodeDiscussionError(t, rr)["code"] != "forbidden_chat_config" {
		t.Fatalf("member PATCH: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Owner PATCH with an unsupported model -> 400 invalid_model_or_thinking_level
	// (L2 union authority: no coordinator, value must pass a ready runtime).
	req = withURLParam(newRequest("PATCH", "/api/chat/sessions/"+fx.SessionID+"/config", map[string]any{"model": "definitely-not-a-model"}), "sessionId", fx.SessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr = httptest.NewRecorder()
	testHandler.PatchChatSessionConfig(rr, req)
	if rr.Code != 400 || decodeDiscussionError(t, rr)["code"] != "invalid_model_or_thinking_level" {
		t.Fatalf("owner bad model PATCH: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Owner PATCH with the pure sentinel -> 200.
	req = withURLParam(newRequest("PATCH", "/api/chat/sessions/"+fx.SessionID+"/config", map[string]any{"model": "", "thinking_level": ""}), "sessionId", fx.SessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr = httptest.NewRecorder()
	testHandler.PatchChatSessionConfig(rr, req)
	if rr.Code != 200 {
		t.Fatalf("owner sentinel PATCH: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Archived shared session: reads stay 200 (AC-20), writes 409.
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fx.SessionID); err != nil {
		t.Fatalf("archive session: %v", err)
	}
	if rr := callSharedList(t, fx.SessionID); rr.Code != 200 {
		t.Fatalf("archived list: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = callSharedSend(t, fx.SessionID, map[string]any{"content": "after archive"}, "key-archived")
	if rr.Code != 409 || decodeDiscussionError(t, rr)["code"] != "chat_session_closed_or_changed" {
		t.Fatalf("archived send: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Shared DELETE is refused with 403 forbidden_chat_config (§3.6 closure).
	req = withURLParam(newRequest("DELETE", "/api/chat/sessions/"+fx.SessionID, nil), "sessionId", fx.SessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr = httptest.NewRecorder()
	testHandler.DeleteChatSession(rr, req)
	if rr.Code != 403 || decodeDiscussionError(t, rr)["code"] != "forbidden_chat_config" {
		t.Fatalf("shared DELETE: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSharedDiscussionList_PageObjectAndInvalidCursor(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionSharedFixture(t, "list", "")
	rr := callSharedSend(t, fx.SessionID, map[string]any{"content": "first"}, "key-list-1")
	if rr.Code != 201 {
		t.Fatalf("seed send: %d %s", rr.Code, rr.Body.String())
	}
	// /messages on a shared session returns the PAGE OBJECT, never a bare array
	// (SDD §3.3).
	rr = callSharedList(t, fx.SessionID)
	if rr.Code != 200 {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var page struct {
		Messages []ChatMessageResponse `json:"messages"`
		Limit    int                   `json:"limit"`
		HasMore  bool                  `json:"has_more"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("shared list must be a page object, got %q: %v", rr.Body.String(), err)
	}
	if len(page.Messages) != 1 || page.Limit != 50 {
		t.Fatalf("page = %+v", page)
	}
	if page.Messages[0].AuthorType == nil || *page.Messages[0].AuthorType != "member" {
		t.Fatalf("shared message must carry author_type member: %+v", page.Messages[0])
	}
	// Invalid limit -> 400 invalid_cursor.
	req := withURLParam(newRequest("GET", "/api/chat/sessions/"+fx.SessionID+"/messages/page?limit=9999", nil), "sessionId", fx.SessionID)
	req = withSharedWorkspaceCtx(t, req, testUserID)
	rr = httptest.NewRecorder()
	testHandler.ListChatMessagesPage(rr, req)
	if rr.Code != 400 || decodeDiscussionError(t, rr)["code"] != "invalid_cursor" {
		t.Fatalf("invalid limit: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSharedDiscussionMergeForward_MessageIDsIdempotent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	wireChatCatalogPort()
	ctx := context.Background()

	teamAgentID := createHandlerTestAgent(t, "shared merge team agent", []byte("{}"))
	settings, _ := json.Marshal(map[string]string{service.ProjectSettingTeamAgentID: teamAgentID})
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, 'shared merge fixture', $2) RETURNING id
	`, testWorkspaceID, settings).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_idempotency WHERE scope_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project_chat_session WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	fx := callGetProjectDiscussion(t, projectID)
	sessionID := fx.SessionID

	// Seed two shared messages.
	rr := callSharedSend(t, sessionID, map[string]any{"content": "msg one"}, "key-m1")
	if rr.Code != 201 {
		t.Fatalf("seed m1: %d %s", rr.Code, rr.Body.String())
	}
	var m1 DiscussionSendResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &m1)
	rr = callSharedSend(t, sessionID, map[string]any{"content": "msg two"}, "key-m2")
	if rr.Code != 201 {
		t.Fatalf("seed m2: %d %s", rr.Code, rr.Body.String())
	}
	var m2 DiscussionSendResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &m2)

	merge := func(key string) *httptest.ResponseRecorder {
		req := withURLParam(newRequest("POST", "/api/projects/"+projectID+"/chat/merge-forward", map[string]any{
			"message_ids": []string{m1.MessageID, m2.MessageID},
		}), "id", projectID)
		req = withSharedWorkspaceCtx(t, req, testUserID)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		out := httptest.NewRecorder()
		testHandler.MergeForwardDiscussion(out, req)
		return out
	}

	// Missing Idempotency-Key -> 400 idempotency_key_required.
	if out := merge(""); out.Code != 400 || decodeDiscussionError(t, out)["code"] != "idempotency_key_required" {
		t.Fatalf("merge without key: %d %s", out.Code, out.Body.String())
	}
	// Happy path -> 201 (Team Agent comment/task pair).
	out := merge("merge-key")
	if out.Code != 201 {
		t.Fatalf("merge status=%d body=%s", out.Code, out.Body.String())
	}
	var first SendProjectChatMessageResponse
	if err := json.Unmarshal(out.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode merge response: %v", err)
	}
	// Replay -> 201 with the same comment_id/task_id (AC-27), no new rows.
	out = merge("merge-key")
	if out.Code != 201 {
		t.Fatalf("merge replay status=%d body=%s", out.Code, out.Body.String())
	}
	var replay SendProjectChatMessageResponse
	if err := json.Unmarshal(out.Body.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replay.CommentID != first.CommentID || replay.TaskID != first.TaskID {
		t.Fatalf("replay = %s/%s, want %s/%s (AC-27)", replay.CommentID, replay.TaskID, first.CommentID, first.TaskID)
	}
	var commentCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, first.IssueID).Scan(&commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("merge replay wrote %d comments, want 1", commentCount)
	}
	// Source messages are untouched (AC-15: not moved/deleted/duplicated).
	var msgCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&msgCount); err != nil {
		t.Fatalf("count source messages: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("source messages = %d, want 2 (AC-15)", msgCount)
	}
}

var _ = fmt.Sprintf // keep fmt for future diagnostics
