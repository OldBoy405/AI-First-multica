package handler

import (
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// OrgAdminResponse is the wire shape of the Org Admin bootstrap endpoint.
type OrgAdminResponse struct {
	ProjectID   string `json:"project_id"`
	AgentID     string `json:"agent_id"`
	AutopilotID string `json:"autopilot_id"`
	TriggerID   string `json:"trigger_id"`
}

// CreateOrgAdminAgent handles POST /api/agents/org-admin. Owner/Admin only;
// the runtime must belong to the same workspace. Repeated calls return the
// same stable ids (idempotent bootstrap, SDD §4.6).
func (h *Handler) CreateOrgAdminAgent(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeErrorCode(w, http.StatusForbidden, "owner_admin_required", "only owners and admins may bootstrap the Org Admin workspace")
		return
	}
	var body struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RuntimeID == "" {
		writeErrorCode(w, http.StatusBadRequest, "runtime_required", "runtime_id is required")
		return
	}
	runtimeID := parseUUID(body.RuntimeID)
	if !runtimeID.Valid {
		writeErrorCode(w, http.StatusBadRequest, "runtime_required", "runtime_id must be a valid UUID")
		return
	}
	// Cross-workspace runtime guard: the runtime must live in this workspace.
	if _, err := h.Queries.GetAgentRuntimeForWorkspace(r.Context(), db.GetAgentRuntimeForWorkspaceParams{
		ID:          runtimeID,
		WorkspaceID: parseUUID(workspaceID),
	}); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "runtime_not_in_workspace", "runtime_id does not belong to this workspace")
		return
	}

	ref, err := service.EnsureOrgAdminWorkspace(r.Context(), h.Queries, h.TxStarter,
		parseUUID(workspaceID), member.UserID, runtimeID)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", "failed to bootstrap Org Admin workspace")
		return
	}
	writeJSON(w, http.StatusOK, OrgAdminResponse{
		ProjectID:   uuidToString(ref.ProjectID),
		AgentID:     uuidToString(ref.AgentID),
		AutopilotID: uuidToString(ref.AutopilotID),
		TriggerID:   uuidToString(ref.TriggerID),
	})
}
