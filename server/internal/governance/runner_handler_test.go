package governance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func runnerStartRequest(t *testing.T, pathWorkspace, headerWorkspace, body string) *httptest.ResponseRecorder {
	t.Helper()
	runner := &Runner{}
	router := chi.NewRouter()
	router.Post("/api/workspaces/{workspaceID}/pipeline-runs", runner.HandleStartArchitecture)
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+pathWorkspace+"/pipeline-runs", strings.NewReader(body))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Workspace-ID", headerWorkspace)
	req.Header.Set("X-Agent-ID", "00000000-0000-0000-0000-000000000002")
	req.Header.Set("X-Task-ID", "00000000-0000-0000-0000-000000000003")
	req.Header.Set("X-User-ID", "00000000-0000-0000-0000-000000000004")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestHandleStartArchitectureRejectsPathAuthorityMismatch(t *testing.T) {
	rec := runnerStartRequest(t,
		"00000000-0000-0000-0000-000000000099",
		"00000000-0000-0000-0000-000000000001",
		`{"pipeline_id":"architecture-design","cr_id":"CR-2026-045","inputs":{}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), RunnerErrAuthorityMismatch) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleStartArchitectureRejectsTrailingJSON(t *testing.T) {
	workspace := "00000000-0000-0000-0000-000000000001"
	rec := runnerStartRequest(t, workspace, workspace,
		`{"pipeline_id":"architecture-design","cr_id":"CR-2026-045","inputs":{}} {}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid body") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
