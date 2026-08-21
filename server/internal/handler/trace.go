package handler

// AIFIRST: CR-2026-049 TASK-07 — trace read API endpoints (SDD §3.5/§3.7).
// GET /api/cr/specs/{spec_id}/trace and GET /api/cr/spec-search; workspace
// comes from the X-Workspace-ID header + membership check, never from the
// body/query. Error envelope is the shared maturity ApiError shape
// {"error","message","request_id"}.

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/governance"
)

func (h *Handler) traceService() *governance.TraceService {
	return governance.NewTraceService(h.DB)
}

// HandleSpecTrace handles GET /api/cr/specs/{spec_id}/trace.
func (h *Handler) HandleSpecTrace(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	specID := chi.URLParam(r, "specID")
	if specID == "" {
		writeApiError(w, http.StatusBadRequest, "invalid_query", "spec id is required")
		return
	}
	timeline, err := h.traceService().SpecTimeline(r.Context(), workspaceID, specID)
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load spec trace")
		return
	}
	writeJSON(w, http.StatusOK, timeline)
}

// HandleSpecSearch handles GET /api/cr/spec-search?q=&owner=&limit=&cursor=.
func (h *Handler) HandleSpecSearch(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	q := r.URL.Query().Get("q")
	owner := r.URL.Query().Get("owner")
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
	page, err := h.traceService().SpecSearch(r.Context(), workspaceID, q, owner, limit, cursor)
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "spec search failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
