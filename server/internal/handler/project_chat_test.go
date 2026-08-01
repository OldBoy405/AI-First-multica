package handler

import (
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
