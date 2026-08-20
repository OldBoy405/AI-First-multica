package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMaturityHandlerGuards covers the 400 walls of the maturity read API
// (SDD §3.3/§3.4): no user rankings, no foreign-user trend series, and no
// unknown dimensions. Auth/membership stay with the router middlewares.
func TestMaturityHandlerGuards(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture unavailable")
	}

	t.Run("rankings scope=user rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/rankings?scope=user", nil)
		testHandler.GetMaturityRankings(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("scope=user: code %d, body %s", w.Code, w.Body.String())
		}
		if !containsMaturity(w.Body.String(), "unsupported_scope") {
			t.Fatalf("missing unsupported_scope code in %s", w.Body.String())
		}
	})

	t.Run("token-trend foreign user rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/token-trend?dimension=user&dimension_id=11111111-1111-1111-1111-111111111111", nil)
		testHandler.GetMaturityTokenTrend(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("foreign user: code %d, body %s", w.Code, w.Body.String())
		}
		if !containsMaturity(w.Body.String(), "unsupported_user_dimension") {
			t.Fatalf("missing unsupported_user_dimension code in %s", w.Body.String())
		}
	})

	t.Run("token-trend unknown dimension rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/token-trend?dimension=team", nil)
		testHandler.GetMaturityTokenTrend(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad dimension: code %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("config reachable by a workspace member", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/config", nil)
		testHandler.GetMaturityConfig(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("config: code %d, body %s", w.Code, w.Body.String())
		}
		if !containsMaturity(w.Body.String(), "calibration_status") {
			t.Fatalf("config body lacks calibration_status: %s", w.Body.String())
		}
	})

	t.Run("rankings invalid limit rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/rankings?limit=1000", nil)
		testHandler.GetMaturityRankings(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("limit=1000: code %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("rankings unknown metric rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/rankings?metric=made_up", nil)
		testHandler.GetMaturityRankings(w, req)
		if w.Code != http.StatusBadRequest || !containsMaturity(w.Body.String(), "invalid_query") {
			t.Fatalf("unknown metric: code %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("report history invalid cursor rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/suggestions/history?cursor=not-base64!", nil)
		testHandler.GetMaturitySuggestionHistory(w, req)
		if w.Code != http.StatusBadRequest || !containsMaturity(w.Body.String(), "invalid_query") {
			t.Fatalf("invalid cursor: code %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("overall invalid date rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/maturity/overall?date=not-a-date", nil)
		testHandler.GetMaturityOverall(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad date: code %d, body %s", w.Code, w.Body.String())
		}
	})
}

func containsMaturity(s, sub string) bool { return strings.Contains(s, sub) }
