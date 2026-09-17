// AIFIRST: OutputGuard mount resolution for the per-task env forging (CR-2026-069 TASK-09).
//
// The daemon has exactly one Runtime hooks write point: the claude branch of
// prepareCRGuard. This file resolves the mount *inputs* for that write point so
// crguard_config.go can compose the two hook sets inside the same settings object
// and write it once.
//
// Scope (CR-2026-069 SDD §4.9 step 4): only claude is mounted by the daemon.
// codebuddy / qoder / pi / codex are installed once by their own native config
// surfaces; opening a daemon write point for them would create a second Runtime
// hooks configuration surface on the platform layer.
//
// Contract: only *path references* are produced here. The policy file is never
// copied and never parsed — thresholds stay single-sourced in
// output-guard/policy.json inside the Tools Release.
package execenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvOutputGuardRoot names the environment variable pointing at the Tools Release
// root. Unset means "not installed": the mount is skipped and the existing
// behaviour of prepareCRGuard stays byte-identical.
const EnvOutputGuardRoot = "MULTICA_OUTPUT_GUARD_TOOLS_ROOT"

// outputGuardMount carries the resolved absolute paths of the mount entry points.
type outputGuardMount struct {
	PreHook        string
	PostHook       string
	PolicyPath     string
	AdapterDir     string
	InstallSurface string
}

// outputGuardAdapterEntries lists the hook entry file each Runtime ships. Only
// claude is mounted here; the map exists so the "not written" decision for the
// other four is explicit rather than accidental.
var outputGuardAdapterEntries = map[string][2]string{
	"claude":    {"pretooluse-guard.mjs", "posttooluse-guard.mjs"},
	"codebuddy": {"pretooluse-guard.mjs", "posttooluse-guard.mjs"},
	"qoder":     {"pretooluse-guard.mjs", "posttooluse-guard.mjs"},
	"codex":     {"pretooluse-guard.mjs", "posttooluse-guard.mjs"},
}

// outputGuardMountable reports whether the daemon writes this Runtime's hooks
// itself. Explicit design, not "not implemented yet": daemon write points exist
// for claude only.
func outputGuardMountable(provider string) bool {
	return provider == "claude"
}

// prepareOutputGuard resolves the mount for one task env.
//
// Returns (mount, false) when the Tools Release root is not configured, when the
// provider is not mounted by the daemon, or when the adapter entry files are not
// present. A false result is not an error: prepareCRGuard keeps its previous
// behaviour verbatim.
func prepareOutputGuard(toolsRoot, provider string) (outputGuardMount, bool) {
	root := strings.TrimSpace(toolsRoot)
	if root == "" {
		root = strings.TrimSpace(os.Getenv(EnvOutputGuardRoot))
	}
	if root == "" || !outputGuardMountable(provider) {
		return outputGuardMount{}, false
	}
	entries, known := outputGuardAdapterEntries[provider]
	if !known {
		return outputGuardMount{}, false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return outputGuardMount{}, false
	}
	adapterDir := filepath.Join(absRoot, "output-guard", "adapters", provider)
	pre := filepath.Join(adapterDir, entries[0])
	post := filepath.Join(adapterDir, entries[1])
	if !isRegularFile(pre) || !isRegularFile(post) {
		return outputGuardMount{}, false
	}
	policy := filepath.Join(absRoot, "output-guard", "policy.json")
	if !isRegularFile(policy) {
		return outputGuardMount{}, false
	}
	return outputGuardMount{
		PreHook:        pre,
		PostHook:       post,
		PolicyPath:     policy,
		AdapterDir:     adapterDir,
		InstallSurface: filepath.ToSlash(filepath.Join("output-guard", "adapters", provider)),
	}, true
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// composeOutputGuardHooks appends the OutputGuard hook entries to the hook sets
// already present in the same settings object. Ordering is part of the contract:
// the pre-existing (crctl) segments stay first, the OutputGuard segments follow.
// The returned map is a new value; the caller writes it exactly once.
func composeOutputGuardHooks(existing map[string]any, og outputGuardMount) map[string]any {
	out := make(map[string]any, len(existing)+2)
	for k, v := range existing {
		out[k] = v
	}
	appendHook := func(event, entry string) {
		list, _ := out[event].([]any)
		if list == nil {
			list = []any{}
		}
		out[event] = append(list, map[string]any{
			"matcher": "Bash|Shell|run_in_terminal",
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": fmt.Sprintf("node \"%s\"", entry),
			}},
		})
	}
	appendHook("PreToolUse", og.PreHook)
	appendHook("PostToolUse", og.PostHook)
	return out
}
