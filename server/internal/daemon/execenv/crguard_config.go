// AIFIRST: per-task controlled-shell forging (CR-2026-002 TASK-09, P1 §C.3).
//
// When MULTICA_CONTROLLED_SHELL_RULES is configured, Prepare calls
// prepareCRGuard to forge two defense layers into the task environment:
//
//   - layer 2, PATH shim: {envRoot}/bin/git(.cmd) re-execs
//     `multica gitguard-exec <real-git> <agent> <args...>` so the agent's
//     default `git` hits the whitelist. Windows gets a .cmd + a sh shim pair
//     (PowerShell/cmd and Git Bash resolve differently). An absolute
//     /usr/bin/git bypasses this — documented, the CAS+gate layer backstops.
//   - layer 3, IDE hooks (Claude backend only): materialize a per-task
//     .claude/settings.json wiring the tools PreToolUse guard, whose path is
//     derived from the rules.json location (same package). Backends without
//     hook support degrade to the shim + context section (honest downgrade).
//
// Layer 1 (--disallowedTools Bash) is NOT wired: agent permission.bash from
// the tools frontmatter is not persisted by the platform (M0 finding:
// fieldsReadNotPersisted) — there is nothing trustworthy to key it on yet.
package execenv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

// CRGuardResult reports what was forged so the daemon can wire PATH/env.
type CRGuardResult struct {
	ShimDir      string // prepend to the agent PATH; empty = guard not configured
	HooksWritten bool
}

// prepareCRGuard forges the shim (and Claude hooks) for one task env.
// Not configured → zero result, no error (upstream behavior untouched).
func prepareCRGuard(envRoot, workDir, provider, agentCaller string, logger *slog.Logger) (CRGuardResult, error) {
	rulesPath := strings.TrimSpace(os.Getenv(gitguard.EnvRulesPath))
	if rulesPath == "" {
		return CRGuardResult{}, nil
	}
	// Validate rules up front: a broken file must fail the task preparation
	// loudly, not produce a shim that denies everything with no explanation.
	if _, err := gitguard.Load(rulesPath); err != nil {
		return CRGuardResult{}, fmt.Errorf("controlled-shell rules unusable: %w", err)
	}

	selfBin, err := os.Executable()
	if err != nil {
		return CRGuardResult{}, fmt.Errorf("resolve multica binary: %w", err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		// No git on the machine — nothing to shim.
		logger.Warn("crguard: git not found on PATH; shim skipped")
		return CRGuardResult{}, nil
	}

	binDir := filepath.Join(envRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return CRGuardResult{}, fmt.Errorf("create shim dir: %w", err)
	}
	if caller := strings.TrimSpace(agentCaller); caller == "" {
		agentCaller = "unknown-agent"
	}
	// sh shim (Git Bash on Windows, everything on unix)
	sh := fmt.Sprintf("#!/bin/sh\nexec \"%s\" gitguard-exec \"%s\" \"%s\" \"$@\"\n",
		filepath.ToSlash(selfBin), filepath.ToSlash(realGit), agentCaller)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(sh), 0o755); err != nil {
		return CRGuardResult{}, err
	}
	if runtime.GOOS == "windows" {
		// cmd/PowerShell shim — forged in pair with the sh shim (this machine
		// mixes both, per the M0 field notes).
		cmdShim := fmt.Sprintf("@echo off\r\n\"%s\" gitguard-exec \"%s\" \"%s\" %%*\r\n", selfBin, realGit, agentCaller)
		if err := os.WriteFile(filepath.Join(binDir, "git.cmd"), []byte(cmdShim), 0o755); err != nil {
			return CRGuardResult{}, err
		}
	}

	result := CRGuardResult{ShimDir: binDir}

	// Claude Code hooks: what used to be a manual adapter install becomes part
	// of the env forging. Hook scripts live in the tools package next to the
	// rules file — derive, do not configure twice.
	if provider == "claude" {
		hooksDir := filepath.Join(filepath.Dir(rulesPath), "..", "crctl", "adapters", "claude-code", "hooks")
		guardScript := filepath.Join(hooksDir, "pretooluse-guard.mjs")
		if _, err := os.Stat(guardScript); err != nil {
			logger.Warn("crguard: pretooluse-guard.mjs not found next to rules.json; hooks skipped", "path", guardScript)
			return result, nil
		}
		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "Bash|Write|Edit|NotebookEdit",
					"hooks": []any{map[string]any{
						"type":    "command",
						"command": fmt.Sprintf("node \"%s\"", guardScript),
					}},
				}},
			},
		}
		claudeDir := filepath.Join(workDir, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return result, err
		}
		settingsPath := filepath.Join(claudeDir, "settings.json")
		if _, err := os.Stat(settingsPath); err == nil {
			// local_directory flows may carry a user settings file — never clobber it.
			logger.Warn("crguard: per-task .claude/settings.json already exists; hooks not overwritten", "path", settingsPath)
			return result, nil
		}
		raw, _ := json.MarshalIndent(settings, "", "  ")
		if err := os.WriteFile(settingsPath, append(raw, '\n'), 0o644); err != nil {
			return result, err
		}
		result.HooksWritten = true
	}
	return result, nil
}
