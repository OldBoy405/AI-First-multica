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
