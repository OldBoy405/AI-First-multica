package handler

import (
	"encoding/json"
	"net/http"

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
