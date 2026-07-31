package gitguard

// AIFIRST: table-driven tests against the REAL rules.json when available
// (MULTICA_CONTROLLED_SHELL_RULES or the sibling tools checkout), falling back
// to an embedded copy of the production rules so CI without a tools checkout
// still verifies the semantics (CR-2026-002 TASK-09, AC-5①②).

import (
	"os"
	"path/filepath"
	"testing"
)

// minimal but production-shaped rules for hermetic runs
const embeddedRules = `{
  "v": 1,
  "git": [
    { "sub": "push", "shapes": ["^-u origin \\S+$", "^origin \\S+$", "^origin --delete \\S+$"], "callers": ["*"] },
    { "sub": "commit", "shapes": [{ "re": "^-m (wip: |\\[cr\\] |merge\\().*$", "flags": "s" }], "callers": ["*"] },
    { "sub": "status", "shapes": ["^--short$"], "callers": ["*"] },
    { "sub": "rev-parse", "shapes": ["^(HEAD|origin/\\S+)$", "^--show-toplevel$"], "callers": ["*"] },
    { "sub": "worktree", "shapes": ["^add -b \\S+ .+$", "^remove( --force)? .+$", "^list$"], "callers": ["*"] }
  ],
  "forbiddenFlags": ["-c", "-C", "--exec", "--upload-pack", "--receive-pack", "--config-env"]
}`

func loadTestGuard(t *testing.T) *Guard {
	t.Helper()
	// Prefer the real single source of truth when reachable.
	candidates := []string{os.Getenv(EnvRulesPath)}
	for i := 1; i <= 8; i++ {
		rel := filepath.Join(append(make([]string, 0, i+4), append(repeat("..", i), "tools", "skills", "shared", "controlled-shell", "rules.json")...)...)
		candidates = append(candidates, rel)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if abs, err := filepath.Abs(c); err == nil {
			if g, err := Load(abs); err == nil {
				t.Logf("using real rules.json: %s", abs)
				return g
			}
		}
	}
	tmp := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(tmp, []byte(embeddedRules), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := Load(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestCheckTable(t *testing.T) {
	g := loadTestGuard(t)
	cases := []struct {
		name     string
		sub      string
		args     []string
		wantCode string // "" = allowed
	}{
		// AC-5①: force push is not a whitelisted shape
		{"force push", "push", []string{"--force", "origin", "main"}, CodeForbiddenSubcommand},
		// AC-5②: config injection flag
		{"config injection", "status", []string{"--short", "-c"}, CodeForbiddenFlag},
		{"config injection value form", "status", []string{"--config-env=core.editor=x", "--short"}, CodeForbiddenFlag},
		// not whitelisted at all
		{"rebase", "rebase", []string{"main"}, CodeForbiddenSubcommand},
		{"reset", "reset", []string{"--hard", "HEAD~1"}, CodeForbiddenSubcommand},
		// allowed forms
		{"plain push", "push", []string{"origin", "master"}, ""},
		{"tracking push", "push", []string{"-u", "origin", "requirement/CR-2026-002"}, ""},
		{"status short", "status", []string{"--short"}, ""},
		{"rev-parse head", "rev-parse", []string{"HEAD"}, ""},
		{"rev-parse toplevel", "rev-parse", []string{"--show-toplevel"}, ""},
		{"worktree add", "worktree", []string{"add", "-b", "requirement/CR-X", "/tmp/x", "master"}, ""},
		// commit message discipline (s-flag shape survives the Go port)
		{"cr commit", "commit", []string{"-m", "[cr] CR-2026-002: multi\nline body"}, ""},
		{"conventional commit rejected", "commit", []string{"-m", "feat: nope"}, CodeForbiddenSubcommand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := g.Check(c.sub, c.args, "test-caller")
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("want allowed, got %v", err)
				}
				return
			}
			ge, ok := err.(*Error)
			if !ok || ge.Code != c.wantCode {
				t.Fatalf("want %s, got %v", c.wantCode, err)
			}
		})
	}
}

func TestOnDenyObserverAndMessageMinimization(t *testing.T) {
	g := loadTestGuard(t)
	var gotCaller, gotSub, gotCode string
	g.OnDeny = func(caller, sub, code string) { gotCaller, gotSub, gotCode = caller, sub, code }
	err := g.Check("push", []string{"--force", "origin", "main", "/secret/path"}, "agent-42")
	if err == nil {
		t.Fatal("expected denial")
	}
	if gotCaller != "agent-42" || gotSub != "push" || gotCode != CodeForbiddenSubcommand {
		t.Fatalf("OnDeny got (%s,%s,%s)", gotCaller, gotSub, gotCode)
	}
	// Audit minimization: argument bodies never leak into the error text.
	if msg := err.Error(); containsAny(msg, "/secret/path", "--force origin main") {
		t.Fatalf("denial message must not include argument bodies: %q", msg)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && (stringIndex(s, sub) >= 0) {
			return true
		}
	}
	return false
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestRealRulesLoadUnderRE2 guards the cross-engine invariant: every shape in
// the production rules.json must compile under Go's RE2 (no lookahead/
// backreference). crctl uses JS regex which is more permissive, so a shape can
// pass there and break here — this test fails loudly if that regresses.
// Skips only when the real rules.json cannot be located at all.
func TestRealRulesLoadUnderRE2(t *testing.T) {
	var found bool
	candidates := []string{os.Getenv(EnvRulesPath)}
	for i := 1; i <= 8; i++ {
		candidates = append(candidates, filepath.Join(append(repeat("..", i), "tools", "skills", "shared", "controlled-shell", "rules.json")...))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if abs, err := filepath.Abs(c); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				if _, err := Load(abs); err != nil {
					t.Fatalf("real rules.json failed to load under RE2 — a shape uses PCRE-only syntax: %v", err)
				}
				found = true
				break
			}
		}
	}
	if !found {
		t.Skip("real rules.json not locatable; covered by loadTestGuard fallback elsewhere")
	}
}

func TestFromEnvSemantics(t *testing.T) {
	t.Setenv(EnvRulesPath, "")
	if g, err := FromEnv(); g != nil || err != nil {
		t.Fatalf("unset env must mean (nil, nil), got (%v, %v)", g, err)
	}
	t.Setenv(EnvRulesPath, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := FromEnv(); err == nil {
		t.Fatal("configured-but-missing rules must error (fail closed at the caller)")
	}
}
