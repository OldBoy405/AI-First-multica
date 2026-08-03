package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// TestDiscussionCoordinatorIDFromSettings covers the settings-bag parsing
// branches: an unset, malformed, or invalid binding must degrade to the zero
// UUID ("unconfigured" — the Discussion container stays agent-free per the
// CR-2026-009 red line), never an error or panic, while a well-formed agent
// UUID round-trips.
func TestDiscussionCoordinatorIDFromSettings(t *testing.T) {
	t.Parallel()

	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	wantAgent := util.MustParseUUID(agentID)

	cases := []struct {
		name     string
		settings string
		want     bool // whether the zero-value UUID is expected
	}{
		{"empty object", "{}", true},
		{"malformed json", "{not json", true},
		{"wrong type", `{"` + ProjectSettingDiscussionCoordinatorID + `": 42}`, true},
		{"invalid uuid", `{"` + ProjectSettingDiscussionCoordinatorID + `": "not-a-uuid"}`, true},
		{"empty string", `{"` + ProjectSettingDiscussionCoordinatorID + `": ""}`, true},
		{"other keys only", `{"team_agent_queue_limit": 10}`, true},
		{"valid agent id", `{"` + ProjectSettingDiscussionCoordinatorID + `": "` + agentID + `"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := discussionCoordinatorIDFromSettings([]byte(c.settings))
			if c.want && got.Valid {
				t.Fatalf("discussionCoordinatorIDFromSettings(%q) = %v, want zero UUID", c.settings, got)
			}
			if !c.want && got != wantAgent {
				t.Fatalf("discussionCoordinatorIDFromSettings(%q) = %v, want %v", c.settings, got, wantAgent)
			}
		})
	}
}
