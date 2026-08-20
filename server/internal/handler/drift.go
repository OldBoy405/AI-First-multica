package handler

// AIFIRST: CR-2026-049 TASK-11 — drift API endpoints (SDD §3.6/§3.7).
// GET /api/drift/overview, GET /api/drift/findings (keyset), PATCH
// /api/drift/findings/{id} (CAS). Workspace comes from X-Workspace-ID +
// membership; every query is workspace-first, cross-workspace ids are 404.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/drift"
)

func (h *Handler) driftOverviewRepo() *drift.OverviewRepo {
	return drift.NewOverviewRepo(h.DB)
}

func (h *Handler) driftFindingRepo() *drift.FindingQueryRepo {
	return drift.NewFindingQueryRepo(h.DB)
}

func driftErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, drift.ErrFindingNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, drift.ErrInvalidTransition):
		return http.StatusConflict, "invalid_transition"
	case errors.Is(err, drift.ErrInvalidCursor):
		return http.StatusBadRequest, "invalid_cursor"
	case errors.Is(err, drift.ErrInvalidFilter):
		return http.StatusBadRequest, "invalid_query"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// HandleDriftOverview handles GET /api/drift/overview.
func (h *Handler) HandleDriftOverview(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	overview, err := h.driftOverviewRepo().Overview(r.Context(), workspaceID, time.Now())
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load drift overview")
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// HandleDriftFindings handles GET /api/drift/findings?status=&kind=&repository_id=&limit=&cursor=.
func (h *Handler) HandleDriftFindings(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeApiError(w, http.StatusBadRequest, "invalid_query", "limit must be 1..100")
			return
		}
		limit = n
	}
	var cursor *string
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if len(raw) > 512 {
			writeApiError(w, http.StatusBadRequest, "invalid_cursor", "cursor too long")
			return
		}
		c := raw
		cursor = &c
	}
	page, err := h.driftFindingRepo().ListFindings(r.Context(), workspaceID, drift.ListFindingsFilter{
		Status:       r.URL.Query().Get("status"),
		Kind:         r.URL.Query().Get("kind"),
		RepositoryID: r.URL.Query().Get("repository_id"),
	}, limit, cursor)
	if err != nil {
		status, code := driftErrorStatus(err)
		writeApiError(w, status, code, "finding query failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// HandleDriftPatchFinding handles PATCH /api/drift/findings/{id} with
// {from_status, to_status}.
func (h *Handler) HandleDriftPatchFinding(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	findingID := chi.URLParam(r, "findingID")
	var req struct {
		FromStatus string `json:"from_status"`
		ToStatus   string `json:"to_status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FromStatus == "" || req.ToStatus == "" {
		writeApiError(w, http.StatusBadRequest, "invalid_payload", "from_status/to_status are required")
		return
	}
	updated, err := h.driftFindingRepo().PatchStatus(r.Context(), workspaceID, findingID, req.FromStatus, req.ToStatus)
	if err != nil {
		status, code := driftErrorStatus(err)
		writeApiError(w, status, code, "finding status transition failed")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
