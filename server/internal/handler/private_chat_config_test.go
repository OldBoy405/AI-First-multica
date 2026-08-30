package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// chatConfigTestPort is a handler-side ChatCatalogPort for the Private Ask
// PATCH tests: claude-opus-5 with thinking high/low, always cacheable.
type chatConfigTestPort struct{}

func (chatConfigTestPort) CacheLoad(context.Context, string) (agent.Catalog, bool, error) {
	return agent.Catalog{Models: []agent.Model{
		{ID: "claude-opus-5", Default: true, Thinking: &agent.ModelThinking{SupportedLevels: []agent.ThinkingLevel{{Value: "high"}, {Value: "low"}}}},
	}}, true, nil
}

func (chatConfigTestPort) LiveLoad(context.Context, string) (agent.Catalog, error) {
	return agent.Catalog{}, nil
}

// wireChatConfigTestPort swaps the handler's catalog ports for the duration
// of the test (the handler test fixture never wires a real catalog).
func wireChatConfigTestPort(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	origIssue, origTask := testHandler.IssueService.ChatCatalog, testHandler.TaskService.ChatCatalog
	testHandler.IssueService.ChatCatalog = chatConfigTestPort{}
	testHandler.TaskService.ChatCatalog = chatConfigTestPort{}
	t.Cleanup(func() {
		testHandler.IssueService.ChatCatalog = origIssue
		testHandler.TaskService.ChatCatalog = origTask
	})
}

// seedPrivateChatConfigAgent creates an agent with model claude-opus-5 /
// thinking high on a dedicated claude runtime. The runtime cleanup registers
// BEFORE createHandlerTestAgent's agent cleanup so the RESTRICT fkey is
// satisfied when the agent row goes first (t.Cleanup is LIFO).
func seedPrivateChatConfigAgent(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
		VALUES ($1, 'private-chat-config-runtime', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2, now()) RETURNING id`,
		testWorkspaceID, testUserID).Scan(&runtimeID); err != nil {
		t.Fatalf("create config runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	agentID := createHandlerTestAgent(t, "private-chat-config-agent", nil)
	if _, err := testPool.Exec(ctx, `UPDATE agent SET model = 'claude-opus-5', thinking_level = 'high', runtime_id = $1 WHERE id = $2`, runtimeID, agentID); err != nil {
		t.Fatalf("set agent defaults: %v", err)
	}
	return agentID
}

// seedPrivateAskRow inserts a chat_session row directly: legacy shape
// (base_* NULL) by default; override columns settable.
func seedPrivateAskRow(t *testing.T, agentID, projectID string) string {
	t.Helper()
	var sessionID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, project_id, explicitly_created_at)
		VALUES ($1, $2, $3, 'Private Ask', 'active', $4, now()) RETURNING id
	`, testWorkspaceID, agentID, testUserID, projectID).Scan(&sessionID); err != nil {
		t.Fatalf("seed private ask row: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	return sessionID
}

// privateChatConfigShape is the Private Ask GET/PATCH payload.
type privateChatConfigShape struct {
	ChatSessionResponse
	SessionID           string `json:"session_id"`
	Model               string `json:"model"`
	ThinkingLevel       string `json:"thinking_level"`
	ModelSource         string `json:"model_source"`
	ThinkingLevelSource string `json:"thinking_level_source"`
}

func decodePrivateChatConfig(t *testing.T, raw string) privateChatConfigShape {
	t.Helper()
	var resp privateChatConfigShape
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("decode private chat response: %v; body: %s", err, raw)
	}
	return resp
}

// TestGetProjectPrivateChatSnapshotAndShape is BLOCK-004 + BLOCK-007: the
// get-or-create writes base_* in the INSERT itself (session_default on the
// response), session_id is the row id char-for-char, and a later agent-default
// change never moves the snapshot.
func TestGetProjectPrivateChatSnapshotAndShape(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()
	agentID := seedPrivateChatConfigAgent(t)
	projectID := insertPrivateChatProject(t, agentID)

	w := callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusOK {
		t.Fatalf("first GET: %d: %s", w.Code, w.Body.String())
	}
	first := decodePrivateChatConfig(t, w.Body.String())
	if first.SessionID == "" || first.SessionID != first.ID {
		t.Fatalf("session_id = %q must equal id %q (same UUID, char-for-char)", first.SessionID, first.ID)
	}
	if first.Model != "claude-opus-5" || first.ModelSource != "session_default" {
		t.Fatalf("model = %q (%s), want claude-opus-5 session_default", first.Model, first.ModelSource)
	}
	if first.ThinkingLevel != "high" || first.ThinkingLevelSource != "session_default" {
		t.Fatalf("thinking = %q (%s), want high session_default", first.ThinkingLevel, first.ThinkingLevelSource)
	}
	var baseModel, baseThinking string
	if err := testPool.QueryRow(ctx, `SELECT base_model, base_thinking_level FROM chat_session WHERE id = $1`, first.ID).Scan(&baseModel, &baseThinking); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" {
		t.Fatalf("base_* = (%q, %q), want the INSERT-time agent defaults", baseModel, baseThinking)
	}

	// The snapshot is immutable: changing the agent's defaults must not move
	// the session's effective values (BLOCK-004, AC-19).
	if _, err := testPool.Exec(ctx, `UPDATE agent SET model = 'claude-opus-5', thinking_level = 'low' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("change agent defaults: %v", err)
	}
	w = callGetProjectPrivateChat(t, projectID)
	again := decodePrivateChatConfig(t, w.Body.String())
	if again.Model != "claude-opus-5" || again.ThinkingLevel != "high" ||
		again.ModelSource != "session_default" || again.ThinkingLevelSource != "session_default" {
		t.Fatalf("after agent change: %+v, want the unchanged session snapshot", again)
	}
	if err := testPool.QueryRow(ctx, `SELECT base_model, base_thinking_level FROM chat_session WHERE id = $1`, first.ID).Scan(&baseModel, &baseThinking); err != nil {
		t.Fatalf("re-read snapshot: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" {
		t.Fatalf("base_* moved after agent change: (%q, %q)", baseModel, baseThinking)
	}
}

// TestGetProjectPrivateChatLegacyRowAgentDefaultNoWrite is AC-19/FR-11: a
// legacy row (base_* NULL) renders agent_default without writing the DB.
func TestGetProjectPrivateChatLegacyRowAgentDefaultNoWrite(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()
	agentID := seedPrivateChatConfigAgent(t)
	projectID := insertPrivateChatProject(t, agentID)
	sessionID := seedPrivateAskRow(t, agentID, projectID)

	w := callGetProjectPrivateChat(t, projectID)
	if w.Code != http.StatusOK {
		t.Fatalf("GET legacy: %d: %s", w.Code, w.Body.String())
	}
	resp := decodePrivateChatConfig(t, w.Body.String())
	if resp.ID != sessionID || resp.SessionID != sessionID {
		t.Fatalf("legacy row id mismatch: id=%s session_id=%s want %s", resp.ID, resp.SessionID, sessionID)
	}
	if resp.Model != "claude-opus-5" || resp.ModelSource != "agent_default" {
		t.Fatalf("legacy model = %q (%s), want claude-opus-5 agent_default", resp.Model, resp.ModelSource)
	}
	var baseNull bool
	if err := testPool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&baseNull); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if !baseNull {
		t.Fatal("GET must not write the legacy row (FR-11/AC-19)")
	}
}

// TestCreateChatSessionLeavesBaseSnapshotNull is the BLOCK-004 regression
// half for existing callers: the ordinary POST /api/chat/sessions path passes
// no base_* params, so those rows stay NULL byte-for-byte.
func TestCreateChatSessionLeavesBaseSnapshotNull(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()
	agentID := seedPrivateChatConfigAgent(t)

	req := newRequest("POST", "/api/chat/sessions", map[string]any{
		"agent_id": agentID,
		"title":    "ordinary 1:1 chat",
	})
	req = withChatTestWorkspaceCtx(t, req)
	w := httptest.NewRecorder()
	testHandler.CreateChatSession(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateChatSession: %d: %s", w.Code, w.Body.String())
	}
	var created ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	var baseNull bool
	if err := testPool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL FROM chat_session WHERE id = $1`, created.ID).Scan(&baseNull); err != nil {
		t.Fatalf("read created row: %v", err)
	}
	if !baseNull {
		t.Fatal("existing CreateChatSession callers must keep base_* NULL (no snapshot)")
	}
}

// patchPrivateChatConfig issues the PATCH handler call with the given actor
// and workspace headers.
func patchPrivateChatConfig(t *testing.T, sessionID, userID, workspaceID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("PATCH", "/api/chat/sessions/"+sessionID+"/config", body)
	req = withURLParam(req, "sessionId", sessionID)
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}
	w := httptest.NewRecorder()
	testHandler.PatchChatSessionConfig(w, req)
	return w
}

// TestPatchChatSessionConfigHTTP covers AC-25 and the three-state contract at
// the HTTP boundary: creator-only 403, ordinary-session 404, wrong-workspace
// 404, set/clear/empty-string semantics, first-PATCH backfill, and the
// session_id response field.
func TestPatchChatSessionConfigHTTP(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()
	wireChatConfigTestPort(t)
	agentID := seedPrivateChatConfigAgent(t)
	projectID := insertPrivateChatProject(t, agentID)
	sessionID := seedPrivateAskRow(t, agentID, projectID)
	otherUserID := seedSecondMember(t)

	// AC-25: non-creator -> 403 forbidden_chat_config.
	w := patchPrivateChatConfig(t, sessionID, otherUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-creator patch: %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "forbidden_chat_config" {
		t.Fatalf("non-creator error body = %s (err %v)", w.Body.String(), err)
	}

	// Wrong workspace -> 404 chat_session_not_found.
	w = patchPrivateChatConfig(t, sessionID, testUserID, "00000000-0000-0000-0000-000000000099", map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong-workspace patch: %d: %s", w.Code, w.Body.String())
	}

	// Creator set: 200 with session_id + override + first-PATCH backfill.
	w = patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusOK {
		t.Fatalf("creator patch: %d: %s", w.Code, w.Body.String())
	}
	resp := decodePrivateChatConfig(t, w.Body.String())
	if resp.SessionID != sessionID || resp.SessionID != resp.ID {
		t.Fatalf("session_id = %q (id %q), want %q", resp.SessionID, resp.ID, sessionID)
	}
	if resp.Model != "claude-opus-5" || resp.ModelSource != "override" {
		t.Fatalf("patched model = %q (%s), want claude-opus-5 override", resp.Model, resp.ModelSource)
	}
	if resp.ThinkingLevel != "high" || resp.ThinkingLevelSource != "session_default" {
		t.Fatalf("thinking = %q (%s), want high session_default (backfilled)", resp.ThinkingLevel, resp.ThinkingLevelSource)
	}
	var baseModel, overrideModel string
	if err := testPool.QueryRow(ctx, `SELECT base_model, model_override FROM chat_session WHERE id = $1`, sessionID).Scan(&baseModel, &overrideModel); err != nil {
		t.Fatalf("read patched row: %v", err)
	}
	if baseModel != "claude-opus-5" || overrideModel != "claude-opus-5" {
		t.Fatalf("row = base(%q) override(%q)", baseModel, overrideModel)
	}

	// Empty string clears the override; it never lands in the column.
	w = patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"model": ""})
	if w.Code != http.StatusOK {
		t.Fatalf("empty-string patch: %d: %s", w.Code, w.Body.String())
	}
	resp = decodePrivateChatConfig(t, w.Body.String())
	if resp.ModelSource != "session_default" || resp.Model != "claude-opus-5" {
		t.Fatalf("cleared model = %q (%s)", resp.Model, resp.ModelSource)
	}
	var overrideNull bool
	if err := testPool.QueryRow(ctx, `SELECT model_override IS NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&overrideNull); err != nil || !overrideNull {
		t.Fatalf("empty string must clear the override column: %v %v", err, overrideNull)
	}

	// null clears too.
	w = patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"thinking_level": "low"})
	if w.Code != http.StatusOK {
		t.Fatalf("thinking patch: %d: %s", w.Code, w.Body.String())
	}
	w = patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"thinking_level": nil})
	if w.Code != http.StatusOK {
		t.Fatalf("null patch: %d: %s", w.Code, w.Body.String())
	}
	resp = decodePrivateChatConfig(t, w.Body.String())
	if resp.ThinkingLevelSource != "session_default" {
		t.Fatalf("null-cleared thinking source = %s, want session_default", resp.ThinkingLevelSource)
	}

	// Ordinary 1:1 session (project_id NULL) -> 404.
	var ordinarySession string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, 'Ordinary', 'active', now()) RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&ordinarySession); err != nil {
		t.Fatalf("seed ordinary session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, ordinarySession)
	})
	w = patchPrivateChatConfig(t, ordinarySession, testUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("ordinary session patch: %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "chat_session_not_found" {
		t.Fatalf("ordinary session error body = %s (err %v)", w.Body.String(), err)
	}
}

// TestPatchChatSessionConfigErrorBoundariesWithoutRuntime pins the SDD §3.2
// error ORDER (review BLOCK-003): the creator / project-bound gates must be
// answered BEFORE agent/provider resolution. A Private Ask session whose
// agent's runtime lives in a foreign workspace (so the handler's
// workspace-scoped provider lookup resolves nothing) must still get 403 for
// a non-creator and 404 for an ordinary (project_id IS NULL) session — never
// a premature 400 invalid_model_or_thinking_level. The authorized
// project-bound control case keeps the 400.
func TestPatchChatSessionConfigErrorBoundariesWithoutRuntime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()

	// A runtime in a foreign workspace satisfies the agent.runtime_id FK but
	// never resolves through GetAgentRuntimeForWorkspace(id, testWorkspaceID)
	// — the exact "agent has no resolvable runtime/provider" shape.
	foreignWorkspaceID := dbfx.Insert(t, "workspace", testutil.Cols{
		"name":         "Private ask config foreign ws",
		"slug":         "pcc-foreign-ws",
		"description":  "Foreign workspace",
		"issue_prefix": "PCC",
	})
	var foreignRuntimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
		VALUES ($1, 'pcc-foreign-runtime', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2, now()) RETURNING id`,
		foreignWorkspaceID, testUserID).Scan(&foreignRuntimeID); err != nil {
		t.Fatalf("create foreign runtime: %v", err)
	}
	agentID := createHandlerTestAgent(t, "private-chat-config-foreign-runtime", nil)
	if _, err := testPool.Exec(ctx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, foreignRuntimeID, agentID); err != nil {
		t.Fatalf("repoint agent runtime: %v", err)
	}
	// RESTRICT fkey order: agent first, then runtime, then workspace.
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, foreignRuntimeID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, foreignWorkspaceID)
	})

	projectID := insertPrivateChatProject(t, agentID)
	sessionID := seedPrivateAskRow(t, agentID, projectID)
	otherUserID := seedSecondMember(t)

	// Non-creator on a project-bound session -> 403 even without a runtime.
	w := patchPrivateChatConfig(t, sessionID, otherUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-creator patch without runtime: %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "forbidden_chat_config" {
		t.Fatalf("non-creator error body = %s (err %v)", w.Body.String(), err)
	}

	// Ordinary 1:1 session (project_id NULL) -> 404 even without a runtime.
	var ordinarySession string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, 'Ordinary', 'active', now()) RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&ordinarySession); err != nil {
		t.Fatalf("seed ordinary session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, ordinarySession)
	})
	w = patchPrivateChatConfig(t, ordinarySession, testUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("ordinary session patch without runtime: %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "chat_session_not_found" {
		t.Fatalf("ordinary session error body = %s (err %v)", w.Body.String(), err)
	}

	// Authorized project-bound control: the foreign runtime surfaces as the
	// §4.3 validation 400, exactly as before the reorder.
	w = patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"model": "claude-opus-5"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("creator patch without runtime: %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "invalid_model_or_thinking_level" {
		t.Fatalf("creator error body = %s (err %v)", w.Body.String(), err)
	}
}

// TestPatchChatSessionConfigInvalidModelHTTP: a §4.3 failure returns the
// structured 400 and leaves zero residue (no override, no backfill).
func TestPatchChatSessionConfigInvalidModelHTTP(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()
	wireChatConfigTestPort(t)
	agentID := seedPrivateChatConfigAgent(t)
	projectID := insertPrivateChatProject(t, agentID)
	sessionID := seedPrivateAskRow(t, agentID, projectID)

	w := patchPrivateChatConfig(t, sessionID, testUserID, testWorkspaceID, map[string]any{"model": "claude-madeup-9"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid model patch: %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "invalid_model_or_thinking_level" {
		t.Fatalf("error body = %s (err %v)", w.Body.String(), err)
	}
	var baseNull, overrideNull bool
	if err := testPool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL, model_override IS NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&baseNull, &overrideNull); err != nil {
		t.Fatalf("read residue: %v", err)
	}
	if !baseNull || !overrideNull {
		t.Fatal("failed PATCH must leave zero residue (no backfill, no override)")
	}
}
