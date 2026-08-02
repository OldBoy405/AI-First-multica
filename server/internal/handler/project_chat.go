package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProjectChatResponse is the entry payload for a project's Team Agent group
// chat (CR-2026-006): the hidden container issue that anchors the message
// stream, plus the agent the project has bound as its Team Agent (may be empty
// when unconfigured — the frontend then renders the owner/admin setup CTA).
type ProjectChatResponse struct {
	IssueID     string `json:"issue_id"`
	TeamAgentID string `json:"team_agent_id,omitempty"`
}

// GetProjectChat resolves (lazily creating on first use) the project's hidden
// Team Agent chat container issue and returns it together with the configured
// Team Agent id. GET /api/projects/{id}/chat.
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
	issue, err := h.IssueService.EnsureProjectChatIssue(r.Context(), project.WorkspaceID, project.ID, callerUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project chat")
		return
	}

	writeJSON(w, http.StatusOK, ProjectChatResponse{
		IssueID:     uuidToString(issue.ID),
		TeamAgentID: projectTeamAgentID(project.Settings),
	})
}

// ProjectDiscussionResponse is the entry payload for a project's Discussion
// tab (CR-2026-009): the hidden container issue that anchors the pure-human,
// agent-free message stream. Unlike ProjectChatResponse there is no agent
// binding to report — Discussion never drives an agent.
type ProjectDiscussionResponse struct {
	IssueID string `json:"issue_id"`
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
		IssueID: uuidToString(issue.ID),
	})
}

// projectTeamAgentID pulls the bound Team Agent id out of the project.settings
// JSONB bag. Returns "" when unset or malformed — an unconfigured Team Agent is
// a normal state the frontend handles, not an error.
func projectTeamAgentID(settings []byte) string {
	if len(settings) == 0 {
		return ""
	}
	var bag map[string]any
	if err := json.Unmarshal(settings, &bag); err != nil {
		return ""
	}
	if v, ok := bag[service.ProjectSettingTeamAgentID].(string); ok {
		return v
	}
	return ""
}

// SendProjectChatMessageRequest is the body of POST /api/projects/{id}/chat/messages.
type SendProjectChatMessageRequest struct {
	Content string `json:"content"`
}

// SendProjectChatMessageResponse is returned on a successful send.
type SendProjectChatMessageResponse struct {
	CommentID string `json:"comment_id"`
	TaskID    string `json:"task_id"`
}

// SendProjectChatMessage posts a member's message to the project's Team Agent
// group chat and enqueues a run for the bound agent. POST /api/projects/{id}/chat/messages.
//
// Errors: 409 when no Team Agent is configured; 429 project_queue_full when the
// shared queue is at capacity (front-load reject or the inner-guard race —
// TSUG-001); 502 for any other enqueue failure (retryable, the comment is
// rolled back before returning).
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

	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	teamAgentID := projectTeamAgentID(project.Settings)
	if teamAgentID == "" {
		writeErrorCode(w, http.StatusConflict, "team_agent_not_configured", "project has no Team Agent configured")
		return
	}
	agentUUID, err := util.ParseUUID(teamAgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored team_agent_id is invalid")
		return
	}
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	issue, err := h.IssueService.EnsureProjectChatIssue(r.Context(), project.WorkspaceID, project.ID, callerUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project chat")
		return
	}

	comment, task, err := h.TaskService.SendProjectChatMessage(r.Context(), issue, agentUUID, callerUUID, req.Content)
	if err != nil {
		var full *service.ErrProjectQueueFull
		if errors.As(err, &full) {
			writeProjectQueueFull(w, full)
			return
		}
		// Any other failure: the comment was already rolled back in the
		// service. Signal a retryable send failure.
		writeErrorCode(w, http.StatusBadGateway, "enqueue_failed", "failed to dispatch message to Team Agent")
		return
	}

	writeJSON(w, http.StatusCreated, SendProjectChatMessageResponse{
		CommentID: uuidToString(comment.ID),
		TaskID:    uuidToString(task.ID),
	})
}
