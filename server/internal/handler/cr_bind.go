package handler

// AIFIRST: CR-2026-053 TASK-05 (SDD §3.1/§4.1/§7.1) — task-scoped CR→Issue
// binding endpoint. The review Skill calls this before crctl review-record so
// the CR becomes visible to the project gates query (cr.shell_issue_id →
// issue.project_id), which the frontend needs to render the ApprovalCard.
//
// Identity is not client-settable: the mat_ task-token middleware stamps
// authoritative X-Task-ID / X-Agent-ID / X-Workspace-ID (and X-Actor-Source:
// task_token) headers, overriding whatever the client sent (auth.go). The
// request body carries nothing but the CR-ID in the URL path — task/agent/
// workspace/issue/project are all derived server-side (FR-B1/NFR-4). The
// service re-validates token workspace against the DB agent row and refuses
// on any mismatch (fail closed).

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// HandleBindCurrentTask is POST /api/crs/{crID}/bind-current-task.
// Accepts ONLY task tokens (mat_): any other actor gets TASK_CONTEXT_REQUIRED
// (401). Error semantics per FR-B3 — all seven error codes leave zero binding
// writes behind.
func (h *Handler) HandleBindCurrentTask(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "TASK_CONTEXT_REQUIRED"})
		return
	}
	taskID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Task-ID"), "task id")
	if !ok {
		return
	}
	agentID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Agent-ID"), "agent id")
	if !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Workspace-ID"), "workspace id")
	if !ok {
		return
	}
	crID := chi.URLParam(r, "crID")
	if crID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cr_id required"})
		return
	}

	result, err := h.TaskService.BindCurrentTaskToCR(r.Context(), taskID, agentID, workspaceID, crID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrCRBindTaskContext):
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "TASK_CONTEXT_REQUIRED"})
		case errors.Is(err, service.ErrCRBindTaskIssueRequired):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "TASK_ISSUE_REQUIRED"})
		case errors.Is(err, service.ErrCRBindCRNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "CR_NOT_FOUND"})
		case errors.Is(err, service.ErrCRBindTaskProjectMismatch):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "TASK_PROJECT_MISMATCH"})
		case errors.Is(err, service.ErrCRBindTaskCRConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "TASK_CR_CONFLICT"})
		case errors.Is(err, service.ErrCRBindCRIssueConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "CR_ISSUE_CONFLICT"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "CR_BIND_FAILED"})
		}
		return
	}

	// Transaction committed. Publish the existing cr:updated event so the
	// project gates query re-renders (NFR-5); a failed publish is logged,
	// never rolled back — the frontend re-queries on its own cadence and the
	// realtime layer invalidates gates queries on this event (SDD §7.3).
	h.publishCRUpdated(r, util.UUIDToString(workspaceID), crID)

	writeJSON(w, http.StatusOK, map[string]any{
		"cr_id":      result.CRID,
		"task_id":    util.UUIDToString(result.TaskID),
		"issue_id":   util.UUIDToString(result.IssueID),
		"project_id": util.UUIDToString(result.ProjectID),
		"changed":    result.Changed,
	})
}

// publishCRUpdated emits the existing cr:updated workspace event — the same
// event crsync publishes after projection writes. The realtime layer
// invalidates every project gates query on it, which is what makes the
// ApprovalCard appear (use-realtime-sync.ts). payload carries cr_id/status/
// needs_reconcile (same shape as crsync.publish).
func (h *Handler) publishCRUpdated(r *http.Request, workspaceID string, crID string) {
	if h.Bus == nil {
		return
	}
	var status string
	var needsReconcile bool
	if wsUUID, err := util.ParseUUID(workspaceID); err == nil {
		cr, qerr := h.Queries.GetCrShellIssueInWorkspaceForShare(r.Context(), db.GetCrShellIssueInWorkspaceForShareParams{
			WorkspaceID: wsUUID,
			CrID:        crID,
		})
		if qerr == nil {
			status = cr.Status
			needsReconcile = cr.NeedsReconcile
		}
	}
	h.Bus.Publish(events.Event{
		Type:        protocol.EventCRUpdated,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"cr_id":           crID,
			"status":          status,
			"needs_reconcile": needsReconcile,
		},
	})
	slog.Debug("cr bind: published cr:updated", "cr_id", crID, "workspace_id", workspaceID)
}
