package skill

// AIFIRST: CR-2026-048 TASK-04 tests.

import (
	"strings"
	"testing"
)

func validContent() string {
	return `---
name: cr-reviewer
description: Reviews CR PRDs.
applicable-scenarios: "CR docs"
context-dependencies: "specs/ present"
permission-declaration: "read specs/**; write change-requests/{CR}/review-annotations/"
failure-handling: "fail -> blockers[]"
---
# body
`
}

func TestEvaluatePublishPassesCleanInput(t *testing.T) {
	res := EvaluatePublish(validContent(), nil, "Ray", "skill-uuid", "hash-1")
	if res.Blocked() {
		t.Fatalf("clean input blocked: %+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

func TestEvaluatePublishRejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"name", func(s string) string { return strings.Replace(s, "name: cr-reviewer", "name: \"\"", 1) }, ReasonFrontmatterNameMissing},
		{"description", func(s string) string {
			return strings.Replace(s, "description: Reviews CR PRDs.", "description: \"\"", 1)
		}, ReasonFrontmatterDescriptionMissing},
		{"metadata", func(s string) string { return strings.Replace(s, "applicable-scenarios: \"CR docs\"\n", "", 1) }, "metadata_applicable-scenarios_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := EvaluatePublish(tc.mutate(validContent()), nil, "Ray", "skill-uuid", "hash-1")
			found := false
			for _, r := range res.Reasons {
				if r == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons = %v, want %q", res.Reasons, tc.want)
			}
		})
	}

	res := EvaluatePublish(validContent(), nil, "", "skill-uuid", "hash-1")
	if !contains(res.Reasons, ReasonOwnerMissing) {
		t.Fatalf("empty owner not rejected: %v", res.Reasons)
	}
}

func TestEvaluatePublishScansContentAndFiles(t *testing.T) {
	content := validContent() + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("A", 40) + "\n"
	files := map[string]string{"scripts/fetch.sh": `ref C:\Users\alice\secret.txt` + "\n"}
	res := EvaluatePublish(content, files, "Ray", "skill-uuid", "hash-1")
	if len(res.Findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(res.Findings), res.Findings)
	}
	for _, f := range res.Findings {
		if strings.Contains(f.Excerpt, "ghp_") || strings.Contains(f.Excerpt, `C:\Users\alice`) {
			t.Errorf("finding leaks plaintext: %q", f.Excerpt)
		}
		if f.AppealID == "" {
			t.Error("finding missing appeal id")
		}
		if f.File != "SKILL.md" && f.File != "scripts/fetch.sh" {
			t.Errorf("unexpected file %q", f.File)
		}
	}
}

func TestEvaluatePublishHonorsApprovedAppeals(t *testing.T) {
	content := validContent() + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("A", 40) + "\n"
	// First pass: blocked, grab the appeal id.
	first := EvaluatePublish(content, nil, "Ray", "skill-uuid", "hash-1")
	if len(first.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(first.Findings))
	}
	approvedID := first.Findings[0].AppealID
	approved := func(id string) bool { return id == approvedID }
	second := EvaluatePublish(content, nil, "Ray", "skill-uuid", "hash-1").Release(approved)
	if second.Blocked() {
		t.Fatalf("approved finding still blocks: %+v", second)
	}
	// Content change invalidates the old appeal: new hash, old id no longer matches.
	third := EvaluatePublish(content+"\n", nil, "Ray", "skill-uuid", "hash-2").Release(approved)
	if len(third.Findings) != 1 {
		t.Fatalf("content change must invalidate old appeal, findings = %d", len(third.Findings))
	}
}

func TestEvaluatePublishWarnsOnProtectedPaths(t *testing.T) {
	content := strings.Replace(validContent(),
		"permission-declaration: \"read specs/**; write change-requests/{CR}/review-annotations/\"",
		"permission-declaration: \"write change-requests/_backlog.yml\"", 1)
	res := EvaluatePublish(content, nil, "Ray", "skill-uuid", "hash-1")
	if !contains(res.Warnings, WarningProtectedPaths) {
		t.Fatalf("warnings = %v, want protected path warning", res.Warnings)
	}
	if res.Blocked() {
		t.Fatalf("warning must not block: %+v", res)
	}
}

func TestAppealIDIsDeterministicAndContentSensitive(t *testing.T) {
	a := AppealID("s", "h", "SKILL.md", 3, "github_token")
	b := AppealID("s", "h", "SKILL.md", 3, "github_token")
	if a != b {
		t.Fatal("AppealID not deterministic")
	}
	if a == AppealID("s", "h2", "SKILL.md", 3, "github_token") {
		t.Fatal("AppealID not content-sensitive")
	}
	if len(a) != 64 {
		t.Fatalf("AppealID len = %d, want sha256 hex 64", len(a))
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
