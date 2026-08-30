package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// projectChatSendFixture wires a Team Agent project plus an ensured active
// session, the state the frontend reaches by opening the chat pane (GET)
// before POSTing to container/messages.
type projectChatSendFixture struct {
	mergeForwardFixture
	SessionID string
}

func newProjectChatSendFixture(t *testing.T, label string) projectChatSendFixture {
	t.Helper()
	fx := newMergeForwardFixture(t, label, nil)
	view, err := testHandler.IssueService.EnsureProjectChatSession(context.Background(),
		util.MustParseUUID(testWorkspaceID), util.MustParseUUID(fx.ProjectID), util.MustParseUUID(testUserID))
	if err != nil {
		t.Fatalf("ensure chat session: %v", err)
	}
	if view.SessionID == "" {
		t.Fatalf("expected an ensured session")
	}
	return projectChatSendFixture{mergeForwardFixture: fx, SessionID: view.SessionID}
}

func (fx projectChatSendFixture) postContainer(t *testing.T, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/container", map[string]any{"session_id": sessionID})
	req = withURLParam(req, "id", fx.ProjectID)
	rr := httptest.NewRecorder()
	testHandler.PostProjectChatContainer(rr, req)
	return rr
}

func (fx projectChatSendFixture) postMessage(t *testing.T, sessionID, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/projects/"+fx.ProjectID+"/chat/messages", map[string]any{
		"session_id": sessionID,
		"content":    content,
	})
	req = withURLParam(req, "id", fx.ProjectID)
	rr := httptest.NewRecorder()
	testHandler.SendProjectChatMessage(rr, req)
	return rr
}

// forceSessionModel sets the session's base snapshot to a model outside the
// static test catalog so §4.3 rejects with 400.
func (fx projectChatSendFixture) forceSessionModel(t *testing.T, model string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE project_chat_session SET base_model = $1 WHERE id = $2`, model, fx.SessionID); err != nil {
		t.Fatalf("set session base_model: %v", err)
	}
}

func (fx projectChatSendFixture) containerIssueCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'`, fx.ProjectID).Scan(&n); err != nil {
		t.Fatalf("count container issues: %v", err)
	}
	return n
}

// TestPostProjectChatContainerIdempotent pins FR-10/FR-4: POST container binds
// once; a repeat call returns the same issue_id (idempotency key =
// session_id) and the container count stays at one.
func TestPostProjectChatContainerIdempotent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newProjectChatSendFixture(t, "container-idem")

	rr := fx.postContainer(t, fx.SessionID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var first ProjectChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if first.SessionID != fx.SessionID {
		t.Fatalf("session_id = %q, want %q", first.SessionID, fx.SessionID)
	}
	if first.IssueID == nil || *first.IssueID == "" {
		t.Fatalf("bound container must carry a non-null issue_id: %+v", first)
	}

	rr = fx.postContainer(t, fx.SessionID)
	if rr.Code != http.StatusOK {
		t.Fatalf("repeat container POST: %d: %s", rr.Code, rr.Body.String())
	}
	var again ProjectChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode repeat response: %v", err)
	}
	if again.IssueID == nil || *again.IssueID != *first.IssueID {
		t.Fatalf("repeat POST drifted: %+v vs %+v", again, first)
	}
	if n := fx.containerIssueCount(t); n != 1 {
		t.Fatalf("container count = %d, want 1", n)
	}
}

// TestPostProjectChatContainerInvalidModelNoIssue is the container half of
// AC-23/AC-24: a §4.3 validation failure binds nothing.
func TestPostProjectChatContainerInvalidModelNoIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newProjectChatSendFixture(t, "container-invalid")
	fx.forceSessionModel(t, "madeup-model-9")

	rr := fx.postContainer(t, fx.SessionID)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_model_or_thinking_level") {
		t.Fatalf("expected invalid_model_or_thinking_level code, got %s", rr.Body.String())
	}
	if n := fx.containerIssueCount(t); n != 0 {
		t.Fatalf("validation failure created %d container issues", n)
	}
}

// TestSendProjectChatMessageSameIssueAsContainer pins AC-13's shared-issue
// guarantee: the container POST and the first send bind the SAME container
// issue, and the send response echoes the request's session_id.
func TestSendProjectChatMessageSameIssueAsContainer(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newProjectChatSendFixture(t, "ac13")

	rr := fx.postContainer(t, fx.SessionID)
	if rr.Code != http.StatusOK {
		t.Fatalf("container POST: %d: %s", rr.Code, rr.Body.String())
	}
	var bound ProjectChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &bound); err != nil {
		t.Fatalf("decode container response: %v", err)
	}

	rr = fx.postMessage(t, fx.SessionID, "first message after container")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var sent SendProjectChatMessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &sent); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	if sent.SessionID != fx.SessionID {
		t.Fatalf("send session_id = %q, want %q", sent.SessionID, fx.SessionID)
	}
	if sent.IssueID != *bound.IssueID {
		t.Fatalf("send issue_id = %s, want container issue %s (AC-13)", sent.IssueID, *bound.IssueID)
	}
	if sent.CommentID == "" || sent.TaskID == "" {
		t.Fatalf("send response missing ids: %+v", sent)
	}
	if n := fx.containerIssueCount(t); n != 1 {
		t.Fatalf("container count = %d, want 1", n)
	}
}

// TestSendProjectChatMessageInvalidModelPreTx pins the messages pre-transaction
// §4.3 contract: 400 with zero persisted rows.
func TestSendProjectChatMessageInvalidModelPreTx(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newProjectChatSendFixture(t, "send-invalid")
	fx.forceSessionModel(t, "madeup-model-9")

	rr := fx.postMessage(t, fx.SessionID, "invalid config message")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_model_or_thinking_level") {
		t.Fatalf("expected invalid_model_or_thinking_level code, got %s", rr.Body.String())
	}
	if n := fx.containerIssueCount(t); n != 0 {
		t.Fatalf("pre-tx validation failure created %d container issues", n)
	}
	if comments, tasks := chatPaneState(t, fx.ProjectID); len(comments) != 0 || tasks != 0 {
		t.Fatalf("pre-tx validation failure persisted comments=%d tasks=%d", len(comments), tasks)
	}
}

// TestPostProjectChatContainerMissingSession pins the request contract: the
// container body requires a session_id.
func TestPostProjectChatContainerMissingSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newProjectChatSendFixture(t, "container-missing-session")
	rr := fx.postContainer(t, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}
