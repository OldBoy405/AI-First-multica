// AIFIRST: gitguard — the Go consumer of the controlled-shell whitelist
// (CR-2026-002 TASK-09, P1 design §C.3).
//
// Single source of truth: tools skills/shared/controlled-shell/rules.json
// (the same file crctl.mjs and the Claude Code PreToolUse hook read). This
// package ports crctl's triple whitelist (subcommand + shape regex + caller)
// so the daemon can (a) guard its own worktree operations and (b) back the
// per-task PATH shim that intercepts an agent's default `git`.
//
// Threat model (honest, per §C.1): the machine owner can do anything; this
// guards the MODEL's default path. exec.Command never goes through a shell —
// equivalent to crctl's spawnSync(shell:false). An absolute /usr/bin/git
// bypasses the shim; the CAS+gate layer and CI remain the backstop.
package gitguard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Error codes mirror crctl's controlled-shell errors exactly.
const (
	CodeForbiddenSubcommand = "FORBIDDEN_SUBCOMMAND"
	CodeForbiddenFlag       = "FORBIDDEN_FLAG"
	CodeUnavailable         = "SHELL_UNAVAILABLE"
)

// EnvRulesPath names the environment variable pointing at rules.json.
// Unset = gitguard not configured (callers fall back to their pre-guard
// behavior); set but unreadable = fail closed (deny everything).
const EnvRulesPath = "MULTICA_CONTROLLED_SHELL_RULES"

// SystemCaller is the caller identity for the daemon's own git operations
// (matrix convention: base skills roll up to the orchestrator).
const SystemCaller = "system-orchestrator"

// Error is a structured denial. The message never includes argument bodies —
// audit minimization (§C.5) starts at the type.
type Error struct {
	Code    string
	Caller  string
	Sub     string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Guard holds the parsed whitelist.
type Guard struct {
	whitelist      map[string][]*regexp.Regexp
	forbiddenFlags []string
	// OnDeny, when set, observes every denial (caller, sub, code) — the
	// TASK-10 audit channel hooks in here. Never receives argument bodies.
	OnDeny func(caller, sub, code string)
}

type rulesFile struct {
	V   int `json:"v"`
	Git []struct {
		Sub    string            `json:"sub"`
		Shapes []json.RawMessage `json:"shapes"`
	} `json:"git"`
	ForbiddenFlags []string `json:"forbiddenFlags"`
}

// Load parses rules.json. Any parse failure is an error — callers decide
// whether that means fail-closed (shim) or fallback (unconfigured).
func Load(path string) (*Guard, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf rulesFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("rules.json parse: %w", err)
	}
	if rf.V != 1 || len(rf.Git) == 0 || len(rf.ForbiddenFlags) == 0 {
		return nil, fmt.Errorf("rules.json shape unexpected (v=%d git=%d flags=%d)", rf.V, len(rf.Git), len(rf.ForbiddenFlags))
	}
	g := &Guard{whitelist: make(map[string][]*regexp.Regexp, len(rf.Git)), forbiddenFlags: rf.ForbiddenFlags}
	for _, entry := range rf.Git {
		for _, shape := range entry.Shapes {
			re, err := parseShape(shape)
			if err != nil {
				return nil, fmt.Errorf("rules.json sub %s: %w", entry.Sub, err)
			}
			g.whitelist[entry.Sub] = append(g.whitelist[entry.Sub], re)
		}
	}
	return g, nil
}

// parseShape accepts "regex-source" or {"re": "...", "flags": "s"} — the same
// two encodings crctl.mjs loads. Only the s (dotall) flag is used by the rules.
func parseShape(raw json.RawMessage) (*regexp.Regexp, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return regexp.Compile(s)
	}
	var obj struct {
		Re    string `json:"re"`
		Flags string `json:"flags"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Re == "" {
		return nil, fmt.Errorf("shape neither string nor {re, flags}: %s", string(raw))
	}
	src := obj.Re
	if strings.Contains(obj.Flags, "s") {
		src = "(?s)" + src
	}
	return regexp.Compile(src)
}

// FromEnv loads the guard from MULTICA_CONTROLLED_SHELL_RULES.
// ("", nil) when the variable is unset — not configured, caller's choice.
func FromEnv() (*Guard, error) {
	path := strings.TrimSpace(os.Getenv(EnvRulesPath))
	if path == "" {
		return nil, nil
	}
	return Load(path)
}

// Check applies the triple whitelist. nil = allowed.
func (g *Guard) Check(sub string, args []string, caller string) error {
	deny := func(code, msg string) error {
		if g.OnDeny != nil {
			g.OnDeny(caller, sub, code)
		}
		return &Error{Code: code, Caller: caller, Sub: sub, Message: msg}
	}
	for _, a := range args {
		for _, f := range g.forbiddenFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return deny(CodeForbiddenFlag, fmt.Sprintf("git %s: flag %s is config-injection class and never passes", sub, f))
			}
		}
	}
	patterns, ok := g.whitelist[sub]
	if !ok {
		return deny(CodeForbiddenSubcommand, fmt.Sprintf("git %s is not in the controlled-shell whitelist", sub))
	}
	joined := strings.Join(args, " ")
	for _, re := range patterns {
		if re.MatchString(joined) {
			return nil
		}
	}
	return deny(CodeForbiddenSubcommand, fmt.Sprintf("git %s: argument shape not allowed by the whitelist", sub))
}

// Run checks then executes `git -C dir sub args...` without a shell,
// returning combined output.
func (g *Guard) Run(dir, sub string, args []string, caller string) ([]byte, error) {
	if err := g.Check(sub, args, caller); err != nil {
		return nil, err
	}
	cmd := exec.Command("git", append([]string{"-C", dir, sub}, args...)...)
	return cmd.CombinedOutput()
}
