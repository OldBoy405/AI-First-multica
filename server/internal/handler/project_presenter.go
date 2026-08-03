package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PresenterGrantResponse is a single project_presenter_grant row (CR-2026-010).
type PresenterGrantResponse struct {
	UserID    string `json:"user_id"`
	Status    string `json:"status"`
	GrantedBy string `json:"granted_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

func presenterGrantToResponse(g db.ProjectPresenterGrant) PresenterGrantResponse {
	resp := PresenterGrantResponse{
		UserID:    uuidToString(g.UserID),
		Status:    g.Status,
		CreatedAt: g.CreatedAt.Time.Format(time.RFC3339Nano),
	}
	if g.GrantedBy.Valid {
		resp.GrantedBy = uuidToString(g.GrantedBy)
	}
	return resp
}

// PresenterStateResponse is the body of GET /api/projects/{id}/presenter.
// PendingRequests is only populated for owner/admin callers; MyRequest is
// populated for any caller with a pending request of their own (TSUG-003).
type PresenterStateResponse struct {
	Presenter       *PresenterGrantResponse  `json:"presenter"`
	PendingRequests []PresenterGrantResponse `json:"pending_requests"`
	MyRequest       *PresenterGrantResponse  `json:"my_request"`
}

// PresenterTargetRequest is the body of the approve/reject/transfer
// endpoints, all of which act on a specific target user.
type PresenterTargetRequest struct {
	UserID string `json:"user_id"`
}

// loadPresenterProjectAndCaller resolves the project (scoped to the request's
// workspace) and the caller's user id — the common prelude every presenter
// endpoint needs before calling into the service layer.
func (h *Handler) loadPresenterProjectAndCaller(w http.ResponseWriter, r *http.Request) (db.Project, string, bool) {
	projectUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return db.Project{}, "", false
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return db.Project{}, "", false
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.Project{}, "", false
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID: projectUUID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return db.Project{}, "", false
	}
	return project, userID, true
}

// writePresenterTransitionError maps a rejected transition to its HTTP
// status. Any other error (infrastructure failure) is the caller's
// responsibility to handle as a 500 — this never sees those since
// ErrPresenterTransition is only ever returned for rejected transitions.
func writePresenterTransitionError(w http.ResponseWriter, e *service.ErrPresenterTransition) {
	status := http.StatusBadRequest
	switch e.Code {
	case service.PresenterErrNoPendingRequest, service.PresenterErrNoActivePresenter:
		status = http.StatusNotFound
	case service.PresenterErrRequestAlreadyPending, service.PresenterErrPresenterAlreadyActive:
		status = http.StatusConflict
	case service.PresenterErrNotPresenter, service.PresenterErrInsufficientPermissions:
		status = http.StatusForbidden
	case service.PresenterErrRoleCannotRequest, service.PresenterErrTargetNotMember:
		status = http.StatusBadRequest
	}
	writeErrorCode(w, status, e.Code, e.Message)
}

// GetPresenterState returns the project's current presenter, the pending
// request list (owner/admin only), and the caller's own pending request (any
// role). GET /api/projects/{id}/presenter.
func (h *Handler) GetPresenterState(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	state, err := h.TaskService.GetPresenterState(r.Context(), project, parseUUID(userID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load presenter state")
		return
	}

	resp := PresenterStateResponse{PendingRequests: []PresenterGrantResponse{}}
	if state.Presenter != nil {
		g := presenterGrantToResponse(*state.Presenter)
		resp.Presenter = &g
	}
	for _, p := range state.PendingRequests {
		resp.PendingRequests = append(resp.PendingRequests, presenterGrantToResponse(p))
	}
	if state.MyRequest != nil {
		g := presenterGrantToResponse(*state.MyRequest)
		resp.MyRequest = &g
	}
	writeJSON(w, http.StatusOK, resp)
}

// RequestPresenter lets a plain member ask to become presenter.
// POST /api/projects/{id}/presenter/request.
func (h *Handler) RequestPresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.RequestPresenter(r.Context(), project, parseUUID(userID))
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to request presenter access")
		return
	}
	writeJSON(w, http.StatusCreated, presenterGrantToResponse(grant))
}

func decodePresenterTarget(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var req PresenterTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return pgtype.UUID{}, false
	}
	targetUUID, ok := parseUUIDOrBadRequest(w, req.UserID, "user_id")
	if !ok {
		return pgtype.UUID{}, false
	}
	return targetUUID, true
}

// ApprovePresenter grants the target user's pending request (Owner only).
// POST /api/projects/{id}/presenter/approve.
func (h *Handler) ApprovePresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	targetUUID, ok := decodePresenterTarget(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.ApprovePresenter(r.Context(), project, parseUUID(userID), targetUUID)
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to approve presenter request")
		return
	}
	writeJSON(w, http.StatusOK, presenterGrantToResponse(grant))
}

// RejectPresenter denies the target user's pending request (Owner only).
// POST /api/projects/{id}/presenter/reject.
func (h *Handler) RejectPresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	targetUUID, ok := decodePresenterTarget(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.RejectPresenter(r.Context(), project, parseUUID(userID), targetUUID)
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to reject presenter request")
		return
	}
	writeJSON(w, http.StatusOK, presenterGrantToResponse(grant))
}

// TransferPresenter hands control from the caller (must be the current
// presenter) to the target user. POST /api/projects/{id}/presenter/transfer.
func (h *Handler) TransferPresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	targetUUID, ok := decodePresenterTarget(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.TransferPresenter(r.Context(), project, parseUUID(userID), targetUUID)
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to transfer presenter access")
		return
	}
	writeJSON(w, http.StatusOK, presenterGrantToResponse(grant))
}

// RevokePresenter forcibly ends the active presenter's control (Owner only).
// POST /api/projects/{id}/presenter/revoke.
func (h *Handler) RevokePresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.RevokePresenter(r.Context(), project, parseUUID(userID))
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to revoke presenter access")
		return
	}
	writeJSON(w, http.StatusOK, presenterGrantToResponse(grant))
}

// ReleasePresenter lets the current presenter voluntarily give up control.
// POST /api/projects/{id}/presenter/release.
func (h *Handler) ReleasePresenter(w http.ResponseWriter, r *http.Request) {
	project, userID, ok := h.loadPresenterProjectAndCaller(w, r)
	if !ok {
		return
	}
	grant, err := h.TaskService.ReleasePresenter(r.Context(), project, parseUUID(userID))
	if err != nil {
		var te *service.ErrPresenterTransition
		if errors.As(err, &te) {
			writePresenterTransitionError(w, te)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to release presenter access")
		return
	}
	writeJSON(w, http.StatusOK, presenterGrantToResponse(grant))
}
