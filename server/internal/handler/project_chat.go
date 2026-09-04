package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProjectChatResponse is the entry payload for a project's Team Agent group
// chat (CR-2026-056): the active session and its resolved config. IssueID is
// null until a container is bound (first send or explicit POST container). A
// project without a Team Agent returns an empty session_id / team_agent_id so
// the frontend keeps rendering its setup CTA.
type ProjectChatResponse struct {
	SessionID           string  `json:"session_id"`
	IssueID             *string `json:"issue_id"`
	TeamAgentID         string  `json:"team_agent_id"`
	Model               string  `json:"model"`
	ThinkingLevel       string  `json:"thinking_level"`
	ModelSource         string  `json:"model_source"`
	ThinkingLevelSource string  `json:"thinking_level_source"`
}

// projectChatViewResponse adapts the service view to the wire shape.
func projectChatViewResponse(v *service.ProjectChatSessionView) ProjectChatResponse {
	return ProjectChatResponse{
		SessionID:           v.SessionID,
		IssueID:             v.IssueID,
		TeamAgentID:         v.TeamAgentID,
		Model:               v.Model,
		ThinkingLevel:       v.ThinkingLevel,
		ModelSource:         string(v.ModelSource),
		ThinkingLevelSource: string(v.ThinkingLevelSource),
	}
}

// GetProjectChat resolves (lazily creating on first use) the project's active
// Team Agent chat session (CR-2026-056, SDD §4.1) and returns it together
// with the configured Team Agent id and resolved config. GET
// /api/projects/{id}/chat. It never creates the container issue (AC-11).
func (h *Handler) GetProjectChat(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	view, err := h.IssueService.EnsureProjectChatSession(r.Context(), project.WorkspaceID, project.ID, callerUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project chat")
		return
	}

	writeJSON(w, http.StatusOK, projectChatViewResponse(view))
}

// PatchProjectChatConfigRequest is the body of PATCH
// /api/projects/{id}/chat/config (SDD §3.1). The model / thinking_level fields
// are three-state: omitted = keep, JSON null or empty string = clear the
// override, non-empty string = set.
type PatchProjectChatConfigRequest struct {
	SessionID     string          `json:"session_id"`
	Model         json.RawMessage `json:"model"`
	ThinkingLevel json.RawMessage `json:"thinking_level"`
}

// PatchProjectChatConfig applies a three-state config patch to the active
// Team Agent chat session (owner/admin only, AC-6).
func (h *Handler) PatchProjectChatConfig(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	var body PatchProjectChatConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sessionUUID, err := util.ParseUUID(body.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	modelPatch, err := parseChatConfigFieldPatch(body.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "model must be a string or null")
		return
	}
	thinkingPatch, err := parseChatConfigFieldPatch(body.ThinkingLevel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "thinking_level must be a string or null")
		return
	}

	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Resolve the provider string for the bound agent's runtime ahead of the
	// service call (validation input only; the service re-checks the binding
	// under the advisory).
	session, err := h.Queries.GetProjectChatSessionByID(r.Context(), db.GetProjectChatSessionByIDParams{
		ID: sessionUUID, WorkspaceID: project.WorkspaceID, ProjectID: project.ID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID: session.AgentID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "team agent not found")
		return
	}
	provider, pok := h.resolveAgentProvider(r, project.WorkspaceID, agent.RuntimeID)
	if !pok {
		writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "cannot resolve the team agent's runtime provider")
		return
	}

	view, err := h.IssueService.UpdateProjectChatSessionConfig(r.Context(),
		project.WorkspaceID, project.ID, sessionUUID, callerUUID, provider, modelPatch, thinkingPatch)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrTeamAgentNotConfigured):
			writeErrorCode(w, http.StatusConflict, "team_agent_not_configured", "project has no Team Agent configured")
		case errors.Is(err, service.ErrForbiddenChatConfig):
			writeErrorCode(w, http.StatusForbidden, "forbidden_chat_config", "chat config requires owner or admin")
		case errors.Is(err, service.ErrChatSessionNotFound):
			writeError(w, http.StatusNotFound, "chat session not found")
		case errors.Is(err, service.ErrChatSessionClosedOrChanged):
			writeErrorCode(w, http.StatusConflict, "chat_session_closed_or_changed", "session closed or the project's Team Agent changed")
		case errors.Is(err, service.ErrInvalidModelOrThinkingLevel):
			writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "invalid model or thinking level")
		default:
			writeError(w, http.StatusInternalServerError, "failed to update chat config")
		}
		return
	}
	writeJSON(w, http.StatusOK, projectChatViewResponse(view))
}

// parseChatConfigFieldPatch folds the raw JSON body value into the
// three-state patch (SDD FR-6). An absent key stays absent; null and the
// empty string both mean "clear".
func parseChatConfigFieldPatch(raw json.RawMessage) (service.ChatConfigFieldPatch, error) {
	if len(raw) == 0 {
		return service.ChatConfigFieldPatch{}, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return service.ChatConfigFieldPatch{Present: true, Clear: true}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return service.ChatConfigFieldPatch{}, err
	}
	if s == "" {
		return service.ChatConfigFieldPatch{Present: true, Clear: true}, nil
	}
	return service.ChatConfigFieldPatch{Present: true, Value: s}, nil
}

// ProjectDiscussionResponse is the entry payload for a project's Discussion
// tab (CR-2026-009): the hidden container issue that anchors the pure-human,
// agent-free message stream. CR-2026-012 adds the optional Discussion
// Coordinator binding: when set, @-mentioning that agent in Discussion
// activates it (the controlled opening of the CR-2026-009 red line).
type ProjectDiscussionResponse struct {
	IssueID string `json:"issue_id"`
	// CoordinatorAgentID is the bound Discussion Coordinator's agent id;
	// empty when unconfigured (Discussion stays agent-free — the red line).
	CoordinatorAgentID string `json:"coordinator_agent_id,omitempty"`
}

// GetProjectDiscussion resolves (lazily creating on first use) the project's
// hidden Discussion container issue. GET /api/projects/{id}/discussion.
func (h *Handler) GetProjectDiscussion(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	issue, err := h.IssueService.EnsureProjectDiscussionIssue(r.Context(), project.WorkspaceID, project.ID, callerUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project discussion")
		return
	}

	writeJSON(w, http.StatusOK, ProjectDiscussionResponse{
		IssueID:            uuidToString(issue.ID),
		CoordinatorAgentID: projectTeamAgentSetting(project.Settings, service.ProjectSettingDiscussionCoordinatorID),
	})
}

// projectTeamAgentID pulls the bound Team Agent id out of the project.settings
// JSONB bag. Returns "" when unset or malformed — an unconfigured Team Agent is
// a normal state the frontend handles, not an error.
func projectTeamAgentID(settings []byte) string {
	return projectTeamAgentSetting(settings, service.ProjectSettingTeamAgentID)
}

// projectTeamAgentSetting reads one agent-UUID-string setting out of the
// project.settings JSONB bag (shared by the Team Agent binding and the
// CR-2026-012 Discussion Coordinator binding). Returns "" when unset or
// malformed — an unconfigured binding is a normal state the frontend
// handles, not an error.
func projectTeamAgentSetting(settings []byte, key string) string {
	if len(settings) == 0 {
		return ""
	}
	var bag map[string]any
	if err := json.Unmarshal(settings, &bag); err != nil {
		return ""
	}
	if v, ok := bag[key].(string); ok {
		return v
	}
	return ""
}

// GetProjectPrivateChat resolves (lazily creating on first use) the caller's
// Private Ask session for this project (CR-2026-008): the latest active
// chat_session bound to (project_id, creator_id). GET /api/projects/{id}/private-chat.
//
// The session is a personal read-only sandbox: it is created without a
// work_dir, targets the project's bound Team Agent, and is only ever visible
// to its creator (all /api/chat/sessions/{id}/* endpoints enforce
// creator-only access; the realtime layer delivers its events per-user).
//
// CR-2026-056 (SDD §3.2, BLOCK-004): get-or-create writes the base_* snapshot
// in the SAME INSERT as the row (the Team Agent's defaults at creation time;
// never a post-hoc UPDATE). The response appends session_id (== the row id,
// the same UUID) plus the four resolved chat-config fields — display only,
// no §4.3 validation, and existing rows are never written (legacy rows with
// base_* NULL resolve as agent_default, FR-11/AC-19).
//
// Errors: 409 team_agent_not_configured when the project has no Team Agent
// bound (the frontend renders the same setup CTA as the Team Agent pane).
func (h *Handler) GetProjectPrivateChat(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	teamAgentID := projectTeamAgentID(project.Settings)
	var agentUUID pgtype.UUID
	if teamAgentID != "" {
		var perr error
		agentUUID, perr = util.ParseUUID(teamAgentID)
		if perr != nil {
			writeError(w, http.StatusInternalServerError, "stored team_agent_id is invalid")
			return
		}
	}

	session, err := h.Queries.GetProjectChatSessionForCreator(r.Context(), db.GetProjectChatSessionForCreatorParams{
		ProjectID: project.ID, CreatorID: callerUUID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		if !isNotFound(err) {
			writeError(w, http.StatusInternalServerError, "failed to resolve private chat session")
			return
		}
		// Existing sessions still resolve when the binding was removed; only
		// the get-or-create path needs a bound Team Agent (baseline shape).
		if teamAgentID == "" {
			writeErrorCode(w, http.StatusConflict, "team_agent_not_configured", "project has no Team Agent configured")
			return
		}
		agent, err := h.Queries.GetAgent(r.Context(), agentUUID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load private chat agent")
			return
		}
		// BLOCK-004: the snapshot is consumed by the INSERT itself.
		baseModel, baseThinking := service.SnapshotAgentDefaults(agent)
		session, err = h.Queries.CreateChatSession(r.Context(), db.CreateChatSessionParams{
			WorkspaceID:       project.WorkspaceID,
			AgentID:           agentUUID,
			CreatorID:         callerUUID,
			Title:             "Private Ask",
			ProjectID:         project.ID,
			BaseModel:         baseModel,
			BaseThinkingLevel: baseThinking,
		})
		if err != nil {
			// Concurrent get-or-create (two tabs opening the pane at once): the
			// partial unique index collapses the race; the loser reselects.
			// The winner row's own INSERT snapshot is authoritative then.
			if isUniqueViolation(err) {
				session, err = h.Queries.GetProjectChatSessionForCreator(r.Context(), db.GetProjectChatSessionForCreatorParams{
					ProjectID: project.ID, CreatorID: callerUUID, WorkspaceID: project.WorkspaceID,
				})
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to create private chat session")
				return
			}
		}
	}

	// Display resolution (SDD §4.2, no §4.3 validation): the session's own
	// agent provides the agent_default fallback for legacy rows.
	agent, err := h.Queries.GetAgent(r.Context(), session.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load private chat agent")
		return
	}
	resolved := service.ResolveChatConfig(
		session.BaseModel, session.ModelOverride, agent.Model,
		session.BaseThinkingLevel, session.ThinkingLevelOverride, agent.ThinkingLevel,
	)
	writeJSON(w, http.StatusOK, h.privateChatSessionResponse(session, resolved))
}

// SendProjectChatMessageRequest is the body of POST /api/projects/{id}/chat/messages.
type SendProjectChatMessageRequest struct {
	// SessionID is REQUIRED (SDD §3.1): the active session the message is
	// posted into and the anchor for the container bind.
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	// AttachmentIDs optionally binds newly uploaded draft files to the chat
	// comment inside the same send transaction (TASK-07 / §4.5). Older clients
	// omit the field; behavior is unchanged then.
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
}

// SendProjectChatMessageResponse is returned on a successful send.
type SendProjectChatMessageResponse struct {
	SessionID string `json:"session_id"`
	IssueID   string `json:"issue_id"`
	CommentID string `json:"comment_id"`
	TaskID    string `json:"task_id"`
}

// SendProjectChatMessage posts a member's message to the project's Team Agent
// group chat and enqueues a run for the bound agent. POST /api/projects/{id}/chat/messages.
//
// Errors: 400 invalid_model_or_thinking_level (§4.3, pre-transaction — nothing
// persisted); 403 presenter_required when an active presenter holds
// single-writer control and the caller is neither the presenter nor
// owner/admin (CR-2026-010); 404 chat_session_not_found; 409
// team_agent_not_configured / chat_session_closed_or_changed /
// attachment_already_bound; 429 project_queue_full; 502 enqueue_failed for
// any other failure (the whole send transaction was rolled back).
func (h *Handler) SendProjectChatMessage(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req SendProjectChatMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	sessionUUID, err := util.ParseUUID(req.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	attachmentIDs, ok := parseUUIDSliceOrBadRequest(w, req.AttachmentIDs, "attachment_ids")
	if !ok {
		return
	}

	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	result, err := h.IssueService.SendProjectChatMessage(r.Context(),
		project.WorkspaceID, project.ID, sessionUUID, callerUUID, req.Content, attachmentIDs)
	if err != nil {
		writeProjectChatSendError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, SendProjectChatMessageResponse{
		SessionID: result.SessionID,
		IssueID:   result.IssueID,
		CommentID: result.CommentID,
		TaskID:    result.TaskID,
	})
}

// PostProjectChatContainerRequest is the body of POST
// /api/projects/{id}/chat/container (SDD §3.1). The idempotency key IS the
// session_id — no Idempotency-Key header required.
type PostProjectChatContainerRequest struct {
	SessionID string `json:"session_id"`
}

// PostProjectChatContainer explicitly binds the container issue for an active
// session (FR-10/FR-4). Success = 200 with the GET shape and a non-null
// issue_id; a repeat call returns the same issue (idempotent). Validation
// failure (§4.3 or presenter) leaves the session unbound — no issue created.
func (h *Handler) PostProjectChatContainer(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var body PostProjectChatContainerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sessionUUID, err := util.ParseUUID(body.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Provider resolution mirrors PatchProjectChatConfig: validation input
	// only; the service re-checks the binding under the advisory.
	session, err := h.Queries.GetProjectChatSessionByID(r.Context(), db.GetProjectChatSessionByIDParams{
		ID: sessionUUID, WorkspaceID: project.WorkspaceID, ProjectID: project.ID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID: session.AgentID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "team agent not found")
		return
	}
	provider, pok := h.resolveAgentProvider(r, project.WorkspaceID, agent.RuntimeID)
	if !pok {
		writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "cannot resolve the team agent's runtime provider")
		return
	}

	_, view, err := h.IssueService.EnsureProjectChatContainer(r.Context(),
		project.WorkspaceID, project.ID, sessionUUID, callerUUID, provider)
	if err != nil {
		var presenterRequired *service.ErrPresenterRequired
		switch {
		case errors.As(err, &presenterRequired):
			writePresenterRequired(w, presenterRequired)
		case errors.Is(err, service.ErrTeamAgentNotConfigured):
			writeErrorCode(w, http.StatusConflict, "team_agent_not_configured", "project has no Team Agent configured")
		case errors.Is(err, service.ErrChatSessionNotFound):
			writeError(w, http.StatusNotFound, "chat session not found")
		case errors.Is(err, service.ErrChatSessionClosedOrChanged):
			writeErrorCode(w, http.StatusConflict, "chat_session_closed_or_changed", "session closed or the project's Team Agent changed")
		case errors.Is(err, service.ErrInvalidModelOrThinkingLevel):
			writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "invalid model or thinking level")
		default:
			writeError(w, http.StatusInternalServerError, "failed to bind chat container")
		}
		return
	}
	writeJSON(w, http.StatusOK, projectChatViewResponse(view))
}

// writeProjectChatSendError maps the shared send-kernel error contract to the
// wire (SDD §3.1/§3.3): presenter 403, queue full 429, config 400, session
// 404/409, unconfigured 409, attachment conflict 409, anything else 502
// enqueue_failed (the send transaction already rolled everything back).
func writeProjectChatSendError(w http.ResponseWriter, err error) {
	var presenterRequired *service.ErrPresenterRequired
	if errors.As(err, &presenterRequired) {
		writePresenterRequired(w, presenterRequired)
		return
	}
	var full *service.ErrProjectQueueFull
	if errors.As(err, &full) {
		writeProjectQueueFull(w, full)
		return
	}
	switch {
	case errors.Is(err, service.ErrInvalidModelOrThinkingLevel):
		writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "invalid model or thinking level")
	case errors.Is(err, service.ErrChatSessionNotFound):
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
	case errors.Is(err, service.ErrChatSessionClosedOrChanged):
		writeErrorCode(w, http.StatusConflict, "chat_session_closed_or_changed", "session closed or the project's Team Agent changed")
	case errors.Is(err, service.ErrTeamAgentNotConfigured):
		writeErrorCode(w, http.StatusConflict, "team_agent_not_configured", "project has no Team Agent configured")
	case errors.Is(err, service.ErrAttachmentAlreadyBound):
		writeErrorCode(w, http.StatusConflict, "attachment_already_bound", "a draft attachment is already bound")
	default:
		// Any other failure: the transaction already rolled the comment,
		// container bind, task and attachments back. Signal a retryable send
		// failure.
		writeErrorCode(w, http.StatusBadGateway, "enqueue_failed", "failed to dispatch message to Team Agent")
	}
}

// writePresenterRequired returns 403 with the active presenter's user id so
// the frontend can render "current presenter is X" alongside the rejection
// (CR-2026-010 SDD §4.3 AC-2).
func writePresenterRequired(w http.ResponseWriter, required *service.ErrPresenterRequired) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"code":              "presenter_required",
		"error":             required.Error(),
		"presenter_user_id": required.PresenterUserID,
	})
}

// MergeForwardDiscussionRequest is the body of POST /api/projects/{id}/chat/merge-forward.
type MergeForwardDiscussionRequest struct {
	CommentIDs []string `json:"comment_ids"`
	// RegisterCR appends the requirement-register instruction block to the
	// merged message (DD-8): pure comment text, zero server-side CR writes.
	RegisterCR bool `json:"register_cr"`
}

// mergeForwardMaxComments caps one merged forward (ponytail: genuinely longer
// discussions should be summarized via an upgrade path, not prompt-bombed
// into the Team Agent).
const mergeForwardMaxComments = 50

// MergeForwardDiscussion forwards a member's multi-select of Discussion
// messages to the project Team Agent as ONE merged message + ONE task
// (CR-2026-012 DD-7/DD-8 + CR-2026-056 §4.12). POST /api/projects/{id}/chat/merge-forward.
//
// The request carries no session_id: the active session is ensured (created
// on first use) inside the service, and the send kernel's lock-internal
// checks surface a concurrent rebind as 409. The success body carries the
// ensured session_id and the bound issue_id on top of the existing
// comment/task fields (SDD §3.1).
//
// Errors: 400 invalid_comment_selection (empty / over cap / any comment
// outside this project's Discussion container; malformed ids get the generic
// 400 from parseUUIDSliceOrBadRequest); the rest matches
// SendProjectChatMessage's shared kernel mapping.
func (h *Handler) MergeForwardDiscussion(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req MergeForwardDiscussionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	invalidSelection := func(msg string) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_comment_selection", msg)
	}
	if len(req.CommentIDs) == 0 {
		invalidSelection("comment_ids must not be empty")
		return
	}
	if len(req.CommentIDs) > mergeForwardMaxComments {
		invalidSelection("comment_ids exceeds the 50-message cap")
		return
	}
	commentUUIDs, ok := parseUUIDSliceOrBadRequest(w, req.CommentIDs, "comment_ids")
	if !ok {
		return
	}

	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Every selected comment must live in THIS project's Discussion container
	// — cross-container or cross-project selections are rejected wholesale.
	discussionIssue, err := h.Queries.GetProjectDiscussionIssue(r.Context(), db.GetProjectDiscussionIssueParams{
		ProjectID: project.ID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		invalidSelection("project has no Discussion container")
		return
	}
	seen := make(map[pgtype.UUID]struct{}, len(commentUUIDs))
	comments := make([]db.Comment, 0, len(commentUUIDs))
	for _, id := range commentUUIDs {
		if _, dup := seen[id]; dup {
			continue // duplicate ids would render the message twice
		}
		seen[id] = struct{}{}
		comment, cerr := h.Queries.GetCommentInWorkspace(r.Context(), db.GetCommentInWorkspaceParams{
			ID: id, WorkspaceID: project.WorkspaceID,
		})
		if cerr != nil || comment.IssueID != discussionIssue.ID {
			invalidSelection("every comment must belong to this project's Discussion")
			return
		}
		comments = append(comments, comment)
	}

	result, err := h.IssueService.MergeForwardDiscussion(r.Context(),
		project.WorkspaceID, project.ID, callerUUID, comments, nil, req.RegisterCR, "")
	if err != nil {
		writeProjectChatSendError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, SendProjectChatMessageResponse{
		SessionID: result.SessionID,
		IssueID:   result.IssueID,
		CommentID: result.CommentID,
		TaskID:    result.TaskID,
	})
}
