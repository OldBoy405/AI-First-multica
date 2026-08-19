package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/maturity"
	"github.com/multica-ai/multica/server/internal/service"
)

func (h *Handler) maturityService() *service.MaturityService {
	prices, _ := maturity.GeneratedPriceMap()
	return service.NewMaturityService(h.Queries, maturity.GeneratedConfig(), prices)
}

func writeApiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, maturity.ApiError{Error: code, Message: message, RequestID: uuid.NewString()})
}

func parseOptionalDate(r *http.Request) (*time.Time, bool) {
	raw := r.URL.Query().Get("date")
	if raw == "" {
		return nil, true
	}
	d, err := time.ParseInLocation("2006-01-02", raw, shanghaiLoc())
	if err != nil {
		return nil, false
	}
	return &d, true
}

func parseRange(r *http.Request) (from, to time.Time, ok bool) {
	loc := shanghaiLoc()
	to = time.Now().In(loc)
	from = to.AddDate(0, 0, -27)
	if raw := r.URL.Query().Get("from"); raw != "" {
		t, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return from, to, false
		}
		from = t
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return from, to, false
		}
		to = t
	}
	if to.Before(from) || to.Sub(from) > 366*24*time.Hour {
		return from, to, false
	}
	return from, to, true
}

func shanghaiLoc() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}

// GetMaturityOverall handles GET /api/maturity/overall.
func (h *Handler) GetMaturityOverall(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	date, ok := parseOptionalDate(r)
	if !ok {
		writeApiError(w, http.StatusBadRequest, "invalid_query", "date must be YYYY-MM-DD")
		return
	}
	resp, err := h.maturityService().Overall(r.Context(), parseUUID(workspaceID), date)
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load maturity overview")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetMaturityTokenTrend handles GET /api/maturity/token-trend.
func (h *Handler) GetMaturityTokenTrend(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	q := r.URL.Query()
	dimension := q.Get("dimension")
	if dimension != "project" && dimension != "user" && dimension != "model" {
		writeApiError(w, http.StatusBadRequest, "invalid_query", "dimension must be project|user|model")
		return
	}
	dimensionID := q.Get("dimension_id")
	if dimension == "user" {
		if dimensionID != "self" {
			writeApiError(w, http.StatusBadRequest, "unsupported_user_dimension", "only dimension_id=self is available")
			return
		}
		dimensionID = uuid.UUID(member.UserID.Bytes).String()
	}
	from, to, ok := parseRange(r)
	if !ok {
		writeApiError(w, http.StatusBadRequest, "invalid_query", "invalid or oversized date range")
		return
	}
	resp, err := h.maturityService().TokenTrend(r.Context(), parseUUID(workspaceID), maturity.TokenTrendQuery{
		Dimension: dimension, DimensionID: dimensionID,
		From: from, To: to, IncludeCost: q.Get("include_cost") == "true",
	})
	if err != nil {
		writeApiError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetMaturityRankings handles GET /api/maturity/rankings.
func (h *Handler) GetMaturityRankings(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	q := r.URL.Query()
	if scope := q.Get("scope"); scope != "" && scope != "project" {
		writeApiError(w, http.StatusBadRequest, "unsupported_scope", "only project rankings are available")
		return
	}
	date, ok := parseOptionalDate(r)
	if !ok {
		writeApiError(w, http.StatusBadRequest, "invalid_query", "date must be YYYY-MM-DD")
		return
	}
	metric := q.Get("metric")
	if metric == "" {
		metric = "total"
	}
	limit := 20
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeApiError(w, http.StatusBadRequest, "invalid_query", "limit must be 1..100")
			return
		}
		limit = n
	}
	var cursor *string
	if raw := q.Get("cursor"); raw != "" {
		cursor = &raw
	}
	resp, err := h.maturityService().Rankings(r.Context(), parseUUID(workspaceID), date, metric, limit, cursor)
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load rankings")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetMaturitySuggestions handles GET /api/maturity/suggestions.
func (h *Handler) GetMaturitySuggestions(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	resp, err := h.maturityService().Suggestions(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load suggestions")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetMaturitySuggestionHistory handles GET /api/maturity/suggestions/history.
func (h *Handler) GetMaturitySuggestionHistory(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	q := r.URL.Query()
	limit := 12
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 52 {
			writeApiError(w, http.StatusBadRequest, "invalid_query", "limit must be 1..52")
			return
		}
		limit = n
	}
	var cursor *string
	if raw := q.Get("cursor"); raw != "" {
		cursor = &raw
	}
	resp, err := h.maturityService().SuggestionHistory(r.Context(), parseUUID(workspaceID), limit, cursor)
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load report history")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetMaturityConfig handles GET /api/maturity/config.
func (h *Handler) GetMaturityConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	resp, err := h.maturityService().Config(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeApiError(w, http.StatusInternalServerError, "internal_error", "failed to load maturity config")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
