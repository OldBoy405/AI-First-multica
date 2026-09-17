// AIFIRST: regression tests for the OutputGuard managed mount (CR-2026-069 TASK-09).
//
// Scope under test: the single write point in prepareCRGuard (claude only) plus
// the mount resolver in outputguard_config.go. The four invariants asserted here
// are the ones the CR depends on:
//   - claude: pre-existing crctl hook segment first, OutputGuard segment after, one write
//   - an existing settings file is never clobbered
//   - an unavailable Tools Release mount leaves the written bytes verbatim unchanged
//   - providers other than claude write no hooks at all
package execenv

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

const outputGuardRulesFixture = `{
  "v": 1,
  "git": [{"sub": "status", "shapes": ["--short"]}],
  "forbiddenFlags": ["--no-verify"]
}`

// outputGuardFixture builds a minimal Tools Release tree plus the per-task env
// dirs, and returns (toolsRoot, envRoot, workDir, rulesPath).
func outputGuardFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	toolsRoot := t.TempDir()
	rulesPath := filepath.Join(toolsRoot, "skills", "shared", "controlled-shell", "rules.json")
	outputGuardWrite(t, rulesPath, outputGuardRulesFixture)
	outputGuardWrite(t, filepath.Join(toolsRoot, "skills", "shared", "crctl", "adapters", "claude-code", "hooks", "pretooluse-guard.mjs"), "// fixture\n")
	outputGuardWrite(t, filepath.Join(toolsRoot, "output-guard", "policy.json"), "{}\n")
	outputGuardWrite(t, filepath.Join(toolsRoot, "output-guard", "adapters", "claude", "pretooluse-guard.mjs"), "// fixture\n")
	outputGuardWrite(t, filepath.Join(toolsRoot, "output-guard", "adapters", "claude", "posttooluse-guard.mjs"), "// fixture\n")
	return toolsRoot, t.TempDir(), t.TempDir(), rulesPath
}

func outputGuardWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// baselineSettingsJSON is the exact byte content the pre-CR code produced:
// one PreToolUse segment pointing at the crctl guard script.
func baselineSettingsJSON(t *testing.T, rulesPath string) string {
	t.Helper()
	guardScript := filepath.Join(filepath.Dir(rulesPath), "..", "crctl", "adapters", "claude-code", "hooks", "pretooluse-guard.mjs")
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash|Write|Edit|NotebookEdit",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "node \"" + guardScript + "\"",
				}},
			}},
		},
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	return string(append(raw, '\n'))
}

func readSettings(t *testing.T, workDir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("settings not JSON: %v", err)
	}
	return out
}

func hookSegments(t *testing.T, settings map[string]any, event string) []any {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings.hooks missing")
	}
	list, _ := hooks[event].([]any)
	return list
}

func segmentCommand(t *testing.T, segment any) string {
	t.Helper()
	m, ok := segment.(map[string]any)
	if !ok {
		t.Fatalf("segment not an object")
	}
	inner, _ := m["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("segment must carry exactly one hook entry, got %d", len(inner))
	}
	cmd, _ := inner[0].(map[string]any)
	s, _ := cmd["command"].(string)
	return s
}

func TestOutputGuardClaudeComposesBothHookSetsInOneWrite(t *testing.T) {
	toolsRoot, envRoot, workDir, rulesPath := outputGuardFixture(t)
	t.Setenv(gitguard.EnvRulesPath, rulesPath)
	t.Setenv(EnvOutputGuardRoot, toolsRoot)

	res, err := prepareCRGuard(envRoot, workDir, "claude", "dev-agent", slog.Default())
	if err != nil {
		t.Fatalf("prepareCRGuard: %v", err)
	}
	if !res.HooksWritten {
		t.Fatalf("claude hooks must be written")
	}
	settings := readSettings(t, workDir)

	pre := hookSegments(t, settings, "PreToolUse")
	if len(pre) != 2 {
		t.Fatalf("PreToolUse must have 2 segments (crctl first, OutputGuard after), got %d", len(pre))
	}
	if cmd := segmentCommand(t, pre[0]); !strings.Contains(cmd, "claude-code") {
		t.Fatalf("pre-existing crctl segment must stay first, got %q", cmd)
	}
	if cmd := segmentCommand(t, pre[1]); !strings.Contains(cmd, "output-guard") {
		t.Fatalf("OutputGuard segment must come after, got %q", cmd)
	}
	post := hookSegments(t, settings, "PostToolUse")
	if len(post) != 1 {
		t.Fatalf("PostToolUse must have exactly the OutputGuard segment, got %d", len(post))
	}
	if cmd := segmentCommand(t, post[0]); !strings.Contains(cmd, "posttooluse-guard.mjs") {
		t.Fatalf("PostToolUse must point at the OutputGuard post hook, got %q", cmd)
	}
}

func TestOutputGuardNeverClobbersAnExistingSettingsFile(t *testing.T) {
	toolsRoot, envRoot, workDir, rulesPath := outputGuardFixture(t)
	t.Setenv(gitguard.EnvRulesPath, rulesPath)
	t.Setenv(EnvOutputGuardRoot, toolsRoot)

	preexisting := `{"hooks": {"PreToolUse": [{"matcher": "user", "hooks": []}]}}` + "\n"
	outputGuardWrite(t, filepath.Join(workDir, ".claude", "settings.json"), preexisting)

	res, err := prepareCRGuard(envRoot, workDir, "claude", "dev-agent", slog.Default())
	if err != nil {
		t.Fatalf("prepareCRGuard: %v", err)
	}
	if res.HooksWritten {
		t.Fatalf("existing settings must not be counted as written")
	}
	raw, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(raw) != preexisting {
		t.Fatalf("existing settings must stay byte-identical, got %q", string(raw))
	}
}

func TestOutputGuardWithoutToolsRootKeepsBaselineOutputVerbatim(t *testing.T) {
	_, envRoot, workDir, rulesPath := outputGuardFixture(t)
	t.Setenv(gitguard.EnvRulesPath, rulesPath)
	t.Setenv(EnvOutputGuardRoot, "")

	if _, err := prepareCRGuard(envRoot, workDir, "claude", "dev-agent", slog.Default()); err != nil {
		t.Fatalf("prepareCRGuard: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(raw) != baselineSettingsJSON(t, rulesPath) {
		t.Fatalf("unavailable mount must keep the written bytes verbatim")
	}
}

func TestOutputGuardUnavailableWhenAdapterEntryIsMissing(t *testing.T) {
	toolsRoot, envRoot, workDir, rulesPath := outputGuardFixture(t)
	t.Setenv(gitguard.EnvRulesPath, rulesPath)
	t.Setenv(EnvOutputGuardRoot, toolsRoot)
	if err := os.Remove(filepath.Join(toolsRoot, "output-guard", "adapters", "claude", "posttooluse-guard.mjs")); err != nil {
		t.Fatalf("remove adapter entry: %v", err)
	}

	if _, err := prepareCRGuard(envRoot, workDir, "claude", "dev-agent", slog.Default()); err != nil {
		t.Fatalf("prepareCRGuard: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(raw) != baselineSettingsJSON(t, rulesPath) {
		t.Fatalf("incomplete bundle must degrade to the baseline bytes")
	}
}

func TestOutputGuardNonClaudeProvidersWriteNoHooks(t *testing.T) {
	for _, provider := range []string{"codebuddy", "qoder", "pi", "codex"} {
		toolsRoot, envRoot, workDir, rulesPath := outputGuardFixture(t)
		t.Setenv(gitguard.EnvRulesPath, rulesPath)
		t.Setenv(EnvOutputGuardRoot, toolsRoot)

		res, err := prepareCRGuard(envRoot, workDir, provider, "dev-agent", slog.Default())
		if err != nil {
			t.Fatalf("%s: prepareCRGuard: %v", provider, err)
		}
		if res.HooksWritten {
			t.Fatalf("%s: daemon must not write hooks for this provider", provider)
		}
		if _, err := os.Stat(filepath.Join(workDir, ".claude", "settings.json")); !os.IsNotExist(err) {
			t.Fatalf("%s: no .claude/settings.json may be created", provider)
		}
		if _, err := os.Stat(filepath.Join(envRoot, "bin", "git")); err != nil {
			t.Fatalf("%s: PATH shim must still be forged: %v", provider, err)
		}
	}
}

func TestOutputGuardMountResolverIsPathReferencesOnly(t *testing.T) {
	toolsRoot, _, _, _ := outputGuardFixture(t)
	mount, ok := prepareOutputGuard(toolsRoot, "claude")
	if !ok {
		t.Fatalf("mount must resolve for claude with a complete Release tree")
	}
	for _, p := range []string{mount.PreHook, mount.PostHook, mount.PolicyPath, mount.AdapterDir} {
		if !filepath.IsAbs(p) {
			t.Fatalf("mount path must be absolute: %q", p)
		}
	}
	if !strings.HasSuffix(mount.PolicyPath, filepath.Join("output-guard", "policy.json")) {
		t.Fatalf("mount must reference the policy path, not its content: %q", mount.PolicyPath)
	}
	if _, ok := prepareOutputGuard(toolsRoot, "pi"); ok {
		t.Fatalf("non-claude providers are explicitly not mounted by the daemon")
	}
	if _, ok := prepareOutputGuard("", "claude"); ok {
		t.Fatalf("empty Tools Release root must not mount")
	}
}
