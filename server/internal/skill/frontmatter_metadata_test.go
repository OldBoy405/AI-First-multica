package skill

// AIFIRST: CR-2026-048 TASK-03.

import "testing"

func TestParseSkillMetadataExtractsCardFields(t *testing.T) {
	in := `---
name: cr-reviewer
description: Reviews CRs.
applicable-scenarios: "CR PRD/SDD review"
context-dependencies: "needs specs/"
permission-declaration: "read specs/**; write change-requests/"
failure-handling: "fail -> blockers[]"
source: session-export
requirements: [git, node]
---
# body
`
	m := ParseSkillMetadata(in)
	if m.Name != "cr-reviewer" || m.Description != "Reviews CRs." {
		t.Fatalf("name/desc = %q/%q", m.Name, m.Description)
	}
	want := map[string]string{
		"applicable-scenarios":   "CR PRD/SDD review",
		"context-dependencies":   "needs specs/",
		"permission-declaration": "read specs/**; write change-requests/",
		"failure-handling":       "fail -> blockers[]",
		"source":                 "session-export",
	}
	for k, v := range want {
		if m.Fields[k] != v {
			t.Errorf("Fields[%q] = %q, want %q", k, m.Fields[k], v)
		}
	}
	// Structured values keep the JSON coercion contract of the TS parser.
	if m.Fields["requirements"] != `["git","node"]` {
		t.Errorf("requirements = %q, want JSON-encoded list", m.Fields["requirements"])
	}
}

func TestParseSkillMetadataToleratesBadInput(t *testing.T) {
	for _, in := range []string{"", "no frontmatter", "---\nname: [broken\n---\n"} {
		m := ParseSkillMetadata(in)
		if m.Name != "" || len(m.Fields) != 0 {
			t.Errorf("input %q: got %+v, want empty metadata", in, m)
		}
	}
}

func TestParseSkillFrontmatterStillCompatible(t *testing.T) {
	in := "---\nname: a\ndescription: b\n---\n"
	n, d := ParseSkillFrontmatter(in)
	if n != "a" || d != "b" {
		t.Fatalf("got %q/%q", n, d)
	}
}
