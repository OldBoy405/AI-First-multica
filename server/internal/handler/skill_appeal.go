package handler

// AIFIRST: CR-2026-048 TASK-07: false-positive appeal endpoints for the
// publish gate. The ledger is activity_log (append-only audit semantics, no
// new table); appeal ids bind content hashes so an approval can never outlive
// the content it was granted for.

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Appeal ledger actions (closed set, activity_log#action).
const (
	appealActionSubmitted = "skill_appeal_submitted"
	appealActionApproved  = "skill_appeal_approved"
	appealActionRejected  = "skill_appeal_rejected"
)

type submitSkillAppealRequest struct {
	// AppealID is the id the gate returned with the finding. It already binds
	// skill + content hash + file + line + pattern, so the server does not
	// recompute it: the blocked content may be an unsaved request body the
	// server cannot reconstruct, and only an owner decision on an id the gate
	// itself produces can release anything.
	AppealID  string `json:"appeal_id"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	PatternID string `json:"pattern_id"`
}

// SubmitSkillAppeal records a false-positive appeal. Idempotent per appeal id:
// a second submission with identical content is a no-op.
func (h *Handler) SubmitSkillAppeal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	skillRow, ok := h.loadSkillForUser(w, r, id)
	if !ok {
		return
	}
	if !h.canManageSkill(w, r, skillRow) {
		return
	}
	var req submitSkillAppealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.AppealID) != 64 || req.File == "" || req.PatternID == "" || req.Line < 1 {
		writeError(w, http.StatusBadRequest, "appeal_id, file, line and pattern_id are required")
		return
	}

	wsUUID := parseUUID(h.resolveWorkspaceID(r))
	already, err := h.Queries.HasAppealSubmitted(r.Context(), db.HasAppealSubmittedParams{WorkspaceID: wsUUID, AppealID: req.AppealID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check appeal: "+err.Error())
		return
	}
	if already {
		writeJSON(w, http.StatusOK, map[string]any{"appeal_id": req.AppealID, "duplicate": true})
		return
	}

	actorID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	details, err := json.Marshal(map[string]any{
		"appeal_id":  req.AppealID,
		"skill_id":   id,
		"file":       req.File,
		"line":       req.Line,
		"pattern_id": req.PatternID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode appeal: "+err.Error())
		return
	}
	if _, err := h.Queries.InsertSkillAppealEvent(r.Context(), db.InsertSkillAppealEventParams{
		WorkspaceID: wsUUID,
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     parseUUID(actorID),
		Action:      appealActionSubmitted,
		Details:     details,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record appeal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"appeal_id": req.AppealID})
}

type decideSkillAppealRequest struct {
	AppealID string `json:"appeal_id"`
	Approve  bool   `json:"approve"`
}

// DecideSkillAppeal records an owner's per-item decision. Owners/admins only:
// the appeal surface is an exception gate, and granting it must be rare and
// audited (PRD FR-18).
func (h *Handler) DecideSkillAppeal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.loadSkillForUser(w, r, id); !ok {
		return
	}
	wsID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceRole(w, r, wsID, "skill not found", "owner", "admin"); !ok {
		return
	}
	var req decideSkillAppealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AppealID == "" {
		writeError(w, http.StatusBadRequest, "appeal_id is required")
		return
	}

	actorID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	action := appealActionRejected
	if req.Approve {
		action = appealActionApproved
	}
	details, err := json.Marshal(map[string]any{
		"appeal_id":  req.AppealID,
		"skill_id":   id,
		"decided_by": actorID,
		"approve":    req.Approve,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode decision: "+err.Error())
		return
	}
	if _, err := h.Queries.InsertSkillAppealEvent(r.Context(), db.InsertSkillAppealEventParams{
		WorkspaceID: parseUUID(wsID),
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     parseUUID(actorID),
		Action:      action,
		Details:     details,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record decision: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"appeal_id": req.AppealID, "action": action})
}
