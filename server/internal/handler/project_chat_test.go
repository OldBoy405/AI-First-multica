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

// TestGetProjectDiscussion covers the CR-2026-059 entry endpoint (AC-1): it
// must lazily create the shared Discussion session on first call and return
// the same session_id on every subsequent call, issue_id stays null forever,
// and NO container issue (origin_type='project_discussion') is created.
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
		if resp.SessionID == "" {
			t.Fatalf("expected a non-empty session_id")
		}
		if resp.IssueID != nil {
			t.Fatalf("issue_id must stay null, got %q", *resp.IssueID)
		}
		return resp
	}

	first := call()
	second := call()
	if second.SessionID != first.SessionID {
		t.Fatalf("GetProjectDiscussion is not idempotent: first=%s second=%s", first.SessionID, second.SessionID)
	}

	// AC-1: opening the pane never creates a container issue.
	var issueCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM issue WHERE workspace_id = $1 AND origin_type = 'project_discussion'
	`, testWorkspaceID).Scan(&issueCount); err != nil {
		t.Fatalf("count container issues: %v", err)
	}
	if issueCount != 0 {
		t.Fatalf("GetProjectDiscussion created %d container issue(s), want 0 (AC-1)", issueCount)
	}

	// The shared session row exists exactly once (kind=project_shared).
	var sessionKind string
	if err := testPool.QueryRow(ctx, `
		SELECT kind FROM chat_session WHERE id = $1
	`, first.SessionID).Scan(&sessionKind); err != nil {
		t.Fatalf("read shared session: %v", err)
	}
	if sessionKind != "project_shared" {
		t.Fatalf("session kind = %q, want project_shared", sessionKind)
	}
}
