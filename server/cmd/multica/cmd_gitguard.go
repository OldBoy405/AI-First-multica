// AIFIRST: gitguard-exec — the execution backend of the per-task git PATH shim
// (CR-2026-002 TASK-09, P1 design §C.3).
//
// The execenv forges {envRoot}/bin/git(.cmd) that re-execs:
//
//	multica gitguard-exec <real-git> <caller> <git-args...>
//
// This subcommand checks the controlled-shell whitelist
// (MULTICA_CONTROLLED_SHELL_RULES) and either denies with a structured JSON
// error (exit 1) or execs the real git with full stdio passthrough. The shim
// only exists when the rules are configured, but a child that lost the env
// var still fails CLOSED here — never silently open.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

var gitguardExecCmd = &cobra.Command{
	Use:                "gitguard-exec <real-git> <caller> [git args...]",
	Short:              "internal: controlled-shell gateway used by the per-task git shim",
	Hidden:             true,
	DisableFlagParsing: true,
	Args:               cobra.MinimumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		realGit, caller := args[0], args[1]
		gitArgs := args[2:]
		sub, rest := gitArgs[0], gitArgs[1:]

		denyExit := func(code, message string) error {
			out, _ := json.Marshal(map[string]any{"error": map[string]string{
				"code": code, "message": message, "attempted": "git " + sub,
			}})
			fmt.Fprintln(os.Stderr, string(out))
			os.Exit(1)
			return nil
		}

		g, err := gitguard.FromEnv()
		if err != nil {
			return denyExit(gitguard.CodeUnavailable, "controlled-shell rules configured but unusable; git denied (fail closed)")
		}
		if g == nil {
			return denyExit(gitguard.CodeUnavailable, "controlled-shell rules not visible in this environment; git denied (fail closed). Use `crctl git` for whitelisted operations.")
		}
		if err := g.Check(sub, rest, caller); err != nil {
			if ge, ok := err.(*gitguard.Error); ok {
				// TASK-10 audit: spool {caller, sub, code} to the crctl outbox —
				// the daemon collector reports it as an activity_log row. Best
				// effort: the denial stands whether or not the spool lands.
				_ = gitguard.SpoolDenial(os.Getenv("CRCTL_WORKSPACE"), caller, sub, ge.Code)
				return denyExit(ge.Code, ge.Message+" — use `crctl git` for whitelisted operations; state files are written by crctl only")
			}
			return denyExit(gitguard.CodeUnavailable, err.Error())
		}

		real := exec.Command(realGit, gitArgs...)
		real.Stdin, real.Stdout, real.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := real.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			return err
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(gitguardExecCmd)
}
