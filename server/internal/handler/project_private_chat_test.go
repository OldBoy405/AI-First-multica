package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// insertPrivateChatProject seeds a project, optionally binding a Team Agent
// through the settings bag (the same key SendProjectChatMessage resolves).
func insertPrivateChatProject(t *testing.T, teamAgentID string) string {
	t.Helper()
	settings := "{}"
	if teamAgentID != "" {
		b, _ := json.Marshal(map[string]string{service.ProjectSettingTeamAgentID: teamAgentID})
		settings = string(b)
	}
	var projectID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, 'private-ask-test', $2)
		RETURNING id
	`, testWorkspaceID, settings).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})
	return projectID
}

func callGetProjectPrivateChat(t *testing.T, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("GET", "/api/projects/"+projectID+"/private-chat", nil)
	req = withURLParam(req, "id", projectID)
	testHandler.GetProjectPrivateChat(w, req)
	return w
}

// TestGetProjectPrivateChat_GetOrCreate covers the pane's entry contract:
// first call lazily creates the (project, creator) session — bound to the
// project, without a work_dir, targeting the Team Agent — and every
// subsequent call returns the same session.
func TestGetProjectPrivateChat_GetOrCreate(t *testing.T) {
	agentID := createHandlerTestAgent(t, "private-ask-agent", nil)
	projectID := insertPrivateChatProject(t, agentID)

	w := callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusOK {
		t.Fatalf("first call: got %d, body %s", w.Code, w.Body.String())
	}
	var first ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if first.AgentID != agentID {
		t.Fatalf("session agent = %s, want %s", first.AgentID, agentID)
	}

	// The created row is project-bound, work_dir-less and titled for the pane.
	var gotProjectID, title string
	var workDir *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT project_id, title, work_dir FROM chat_session WHERE id = $1`,
		first.ID).Scan(&gotProjectID, &title, &workDir); err != nil {
		t.Fatalf("load created session: %v", err)
	}
	if gotProjectID != projectID {
		t.Fatalf("session project_id = %s, want %s", gotProjectID, projectID)
	}
	if title != "Private Ask" {
		t.Fatalf("session title = %q, want %q", title, "Private Ask")
	}
	if workDir != nil {
		t.Fatalf("session work_dir = %v, want NULL (read-only sandbox)", *workDir)
	}

	w = callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusOK {
		t.Fatalf("second call: got %d, body %s", w.Code, w.Body.String())
	}
	var second ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("get-or-create returned a new session: %s != %s", second.ID, first.ID)
	}
}

// TestGetProjectPrivateChat_NoTeamAgent: an unbound project yields the same
// structured 409 the Team Agent pane already handles with its setup CTA.
func TestGetProjectPrivateChat_NoTeamAgent(t *testing.T) {
	projectID := insertPrivateChatProject(t, "")

	w := callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409; body %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["code"] != "team_agent_not_configured" {
		t.Fatalf("error code = %q, want team_agent_not_configured", body["code"])
	}
}

// TestGetProjectPrivateChat_ArchivedStartsFresh: archiving drops the session
// out of the partial-unique predicate, so re-entering the pane starts a new
// session instead of resurrecting the archived one.
func TestGetProjectPrivateChat_ArchivedStartsFresh(t *testing.T) {
	agentID := createHandlerTestAgent(t, "private-ask-archive-agent", nil)
	projectID := insertPrivateChatProject(t, agentID)

	w := callGetProjectPrivateChat(t, projectID)
	var first ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`UPDATE chat_session SET status = 'archived' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("archive session: %v", err)
	}

	w = callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body %s", w.Code, w.Body.String())
	}
	var second ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("archived session was resurrected: %s", second.ID)
	}
}

// TestPrivateAskSessionExcludedFromGlobalChat: project-bound sessions must
// never leak into the global chat surfaces — the session lists and the FAB's
// pending-task aggregate (they would deep-link to a session the global list
// cannot show).
func TestPrivateAskSessionExcludedFromGlobalChat(t *testing.T) {
	agentID := createHandlerTestAgent(t, "private-ask-exclusion-agent", nil)
	projectID := insertPrivateChatProject(t, agentID)

	w := callGetProjectPrivateChat(t, projectID)
	var session ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	insertPendingChatTask(t, agentID, session.ID, "running")

	ctx := context.Background()
	wsUUID := util.MustParseUUID(testWorkspaceID)
	creatorUUID := util.MustParseUUID(testUserID)

	rows, err := testHandler.Queries.ListChatSessionsByCreator(ctx, db.ListChatSessionsByCreatorParams{
		WorkspaceID: wsUUID, CreatorID: creatorUUID,
	})
	if err != nil {
		t.Fatalf("ListChatSessionsByCreator: %v", err)
	}
	for _, row := range rows {
		if uuidToString(row.ID) == session.ID {
			t.Fatalf("private ask session leaked into global session list")
		}
	}

	allRows, err := testHandler.Queries.ListAllChatSessionsByCreator(ctx, db.ListAllChatSessionsByCreatorParams{
		WorkspaceID: wsUUID, CreatorID: creatorUUID,
	})
	if err != nil {
		t.Fatalf("ListAllChatSessionsByCreator: %v", err)
	}
	for _, row := range allRows {
		if uuidToString(row.ID) == session.ID {
			t.Fatalf("private ask session leaked into all-sessions list")
		}
	}

	pending, err := testHandler.Queries.ListPendingChatTasksByCreator(ctx, db.ListPendingChatTasksByCreatorParams{
		WorkspaceID: wsUUID, CreatorID: creatorUUID,
	})
	if err != nil {
		t.Fatalf("ListPendingChatTasksByCreator: %v", err)
	}
	for _, row := range pending {
		if uuidToString(row.ChatSessionID) == session.ID {
			t.Fatalf("private ask pending task leaked into global FAB aggregate")
		}
	}

	hasPending, err := testHandler.Queries.HasPendingChatTasksByCreator(ctx, db.HasPendingChatTasksByCreatorParams{
		WorkspaceID: wsUUID, CreatorID: creatorUUID,
		AgentIds: []pgtype.UUID{util.MustParseUUID(agentID)},
	})
	if err != nil {
		t.Fatalf("HasPendingChatTasksByCreator: %v", err)
	}
	if hasPending {
		t.Fatalf("has-pending fast path leaked a private ask task")
	}
}
