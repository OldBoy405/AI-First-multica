package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// TestProjectTeamAgentID covers the settings-bag extraction branches: an unset
// or malformed settings blob must degrade to "" (an unconfigured Team Agent is
// a normal state, not an error), while a well-formed value round-trips.
func TestProjectTeamAgentID(t *testing.T) {
	agentID := "550e8400-e29b-41d4-a716-446655440000"
	cases := []struct {
		name     string
		settings string
		want     string
	}{
		{"nil settings", "", ""},
		{"empty object", "{}", ""},
		{"malformed json", "{not json", ""},
		{"wrong type", `{"` + service.ProjectSettingTeamAgentID + `": 42}`, ""},
		{"other keys only", `{"team_agent_queue_limit": 10}`, ""},
		{"valid agent id", `{"` + service.ProjectSettingTeamAgentID + `": "` + agentID + `"}`, agentID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b []byte
			if c.settings != "" {
				b = []byte(c.settings)
			}
			if got := projectTeamAgentID(b); got != c.want {
				t.Fatalf("projectTeamAgentID(%q) = %q, want %q", c.settings, got, c.want)
			}
		})
	}
}

// TestGetProjectDiscussion covers the CR-2026-009 entry endpoint: it must
// lazily create the Discussion container issue on first call and return the
// same issue_id on every subsequent call for the same project (idempotent
// lazy creation, mirroring GetProjectChat's contract).
func TestGetProjectDiscussion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, testWorkspaceID, "GetProjectDiscussion Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	call := func() ProjectDiscussionResponse {
		t.Helper()
		req := withURLParam(newRequest("GET", "/api/projects/"+projectID+"/discussion", nil), "id", projectID)
		rr := httptest.NewRecorder()
		testHandler.GetProjectDiscussion(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp ProjectDiscussionResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.IssueID == "" {
			t.Fatalf("expected a non-empty issue_id")
		}
		return resp
	}

	first := call()
	second := call()
	if second.IssueID != first.IssueID {
		t.Fatalf("GetProjectDiscussion is not idempotent: first=%s second=%s", first.IssueID, second.IssueID)
	}
}
