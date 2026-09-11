package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// AIFIRST: CR-2026-053 TASK-08 (SDD §3.3, FR-B1) — CR thin-wrapper commands.
// bind-current-task only relays the mat_ task token and the CR-ID to
// POST /api/crs/{cr_id}/bind-current-task and passes the structured result
// through verbatim; no business judgment, no ledger writes, no body
// construction (identity fields are server-derived from the token).

var crCmd = &cobra.Command{
	Use:   "cr",
	Short: "Work with AIFIRST change requests",
}

func init() {
	crCmd.AddCommand(crBindCurrentTaskCmd)
	crCmd.AddCommand(crBindPromotionRunCmd)
	// --output must be registered here (not only in tests): the unit tests
	// used to build a synthetic command with the flag, hiding the gap — the
	// real command rejected `--output json` with "unknown flag".
	crBindCurrentTaskCmd.Flags().String("output", "json", "Output format: table or json")
	crBindPromotionRunCmd.Flags().String("output", "json", "Output format: table or json")
	crBindPromotionRunCmd.Flags().String("run-id", "", "Pre-built promotion pipeline run id to bind")
}

var crBindCurrentTaskCmd = &cobra.Command{
	Use:   "bind-current-task <cr-id>",
	Short: "Bind the current task to a CR and its source issue",
	Long: `Bind the current task to a CR and its source issue (task-scoped).

Requires a mat_ task token: the task/agent/workspace/issue/project identity is
derived server-side from the token — the request body carries nothing but the
CR-ID. Prints the structured result {cr_id, task_id, issue_id, project_id,
changed}; a failed bind exits non-zero with the server error code
(TASK_CONTEXT_REQUIRED / TASK_ISSUE_REQUIRED / CR_NOT_FOUND /
TASK_PROJECT_MISMATCH / TASK_CR_CONFLICT / CR_ISSUE_CONFLICT / CR_BIND_FAILED).`,
	Args: exactArgs(1),
	RunE: runCrBindCurrentTask,
}

func runCrBindCurrentTask(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	crID := args[0]

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var out map[string]any
	if err := client.PostJSON(ctx, "/api/crs/"+crID+"/bind-current-task", map[string]any{}, &out); err != nil {
		return fmt.Errorf("bind current task to %s: %w", crID, err)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "table" {
		changed := "false"
		if b, ok := out["changed"].(bool); ok && b {
			changed = "true"
		}
		cli.PrintTable(os.Stdout, []string{"CR", "TASK", "ISSUE", "PROJECT", "CHANGED"}, [][]string{{
			strVal(out, "cr_id"),
			strVal(out, "task_id"),
			strVal(out, "issue_id"),
			strVal(out, "project_id"),
			changed,
		}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, out)
}

// crBindPromotionRunCmd is the thin relay for the promotion pre-built run
// binding (CR-2026-061 SDD §3.3): relays the mat_ task token and {run_id} to
// POST /api/crs/{cr_id}/bind-promotion-run and passes the structured result
// through verbatim — no business judgment, no ledger writes (same contract
// as bind-current-task).
var crBindPromotionRunCmd = &cobra.Command{
	Use:   "bind-promotion-run <cr-id>",
	Short: "Bind a Discussion-promotion pre-built run to a CR",
	Long: `Bind a Discussion-promotion pre-built pipeline run to a CR.

Requires a mat_ task token: workspace/issue identity derives server-side from
the token and the run row; the body carries only the run id from --run-id.
Prints the structured result {cr_id, run_id, issue_id, changed}; a failed
bind exits non-zero with the server error code (TASK_CONTEXT_REQUIRED /
INVALID_RUN_ID / RUN_NOT_FOUND / CR_NOT_FOUND / RUN_CR_CONFLICT /
CR_ISSUE_CONFLICT / CR_BIND_FAILED).`,
	Args: exactArgs(1),
	RunE: runCrBindPromotionRun,
}

func runCrBindPromotionRun(cmd *cobra.Command, args []string) error {
	// Validate the arguments before anything else. newAPIClient resolves the
	// server URL through a resolver that terminates the process when no server
	// is configured, so checking --run-id afterwards turned a pure usage error
	// into a hard exit on a machine without a configured profile (CI), instead
	// of the usage message the caller and the tests expect.
	crID := args[0]
	runID, err := cmd.Flags().GetString("run-id")
	if err != nil || runID == "" {
		return fmt.Errorf("bind promotion run to %s: --run-id is required", crID)
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var out map[string]any
	if err := client.PostJSON(ctx, "/api/crs/"+crID+"/bind-promotion-run", map[string]any{
		"run_id": runID,
	}, &out); err != nil {
		return fmt.Errorf("bind promotion run to %s: %w", crID, err)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "table" {
		changed := "false"
		if b, ok := out["changed"].(bool); ok && b {
			changed = "true"
		}
		cli.PrintTable(os.Stdout, []string{"CR", "RUN", "ISSUE", "CHANGED"}, [][]string{{
			strVal(out, "cr_id"),
			strVal(out, "run_id"),
			strVal(out, "issue_id"),
			changed,
		}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, out)
}
