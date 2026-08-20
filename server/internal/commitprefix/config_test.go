package commitprefix

// AIFIRST: CR-2026-049 TASK-08 — generated declaration accessor test.
// The gen script's --check mode guards file-level consistency; this test
// guards the runtime surface (3 repos, SDD §3.3 locked values, no wip:).

import (
	"regexp"
	"testing"
)

func TestGeneratedPrefixesShape(t *testing.T) {
	decls := GeneratedPrefixes()
	if len(decls) != 3 {
		t.Fatalf("repos = %d, want 3", len(decls))
	}
	for _, id := range []string{"ai-first-platform-docs", "multica", "tools"} {
		d, ok := decls[id]
		if !ok {
			t.Fatalf("missing repo %s", id)
		}
		if d.Trunk == "" || d.CanonicalURL == "" || d.Owner == "" || d.Repo == "" {
			t.Errorf("%s: incomplete decl %+v", id, d)
		}
		if len(d.Prefixes) == 0 {
			t.Errorf("%s: empty prefixes", id)
		}
		for _, p := range d.Prefixes {
			if p == "wip:" || len(p) >= 4 && p[:4] == "wip:" {
				t.Errorf("%s: wip: must not enter the whitelist", id)
			}
		}
	}
	if decls["multica"].Trunk != "main" || decls["tools"].Trunk != "custom/main" || decls["ai-first-platform-docs"].Trunk != "master" {
		t.Errorf("trunk mismatch: %s %s %s",
			decls["ai-first-platform-docs"].Trunk, decls["multica"].Trunk, decls["tools"].Trunk)
	}
	if decls["ai-first-platform-docs"].CanonicalURL != "https://github.com/OldBoy405/AI-First-Platform.git" {
		t.Errorf("kb canonical url: %s", decls["ai-first-platform-docs"].CanonicalURL)
	}
}

func TestGeneratedConfigRevIsSHA(t *testing.T) {
	rev := GeneratedConfigRev()
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(rev) {
		t.Fatalf("config rev %q is not a 40-hex SHA", rev)
	}
}
