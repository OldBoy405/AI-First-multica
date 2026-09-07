package daemon

import (
	"strings"
	"testing"
)

func TestBuildPromptAssignmentHandoffIsPerTurnContext(t *testing.T) {
	note := "Only touch the login flow; do not change payments."
	out := BuildPrompt(Task{IssueID: "issue-123", HandoffNote: note}, "claude")

	for _, want := range []string{note, "handoff note", "multica issue get issue-123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("assignment prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "quick-create assistant") {
		t.Fatalf("handoff task must stay on the assignment prompt branch:\n%s", out)
	}
}

func TestBuildPromptAssignmentWithoutHandoffStaysClean(t *testing.T) {
	out := BuildPrompt(Task{IssueID: "issue-123"}, "claude")
	if strings.Contains(out, "handoff note") {
		t.Fatalf("assignment without a note gained handoff framing:\n%s", out)
	}
}

// TestBuildPrompt_ApprovalContinuation_MergedHandoff (CR-2026-052 AC-5 prompt
// layer, SDD §7.4) verifies that an approval-continuation successor's merged
// handoff_note — one line per appended approval — renders verbatim in the
// opening prompt of a not-yet-claimed queued/deferred successor. The merged
// note carries both approvals' four-field context; the prompt must surface
// both so the woken agent reads them without consulting the grants directory.
func TestBuildPrompt_ApprovalContinuation_MergedHandoff(t *testing.T) {
	note := "CR-2026-052 的 requirement 审批已 approve（approval_record_id=rec-1）。请读取 .crctl/grants/ 与 crctl status/next 确定下一步；本提示不携带任何状态→下一步映射。\n" +
		"CR-2026-052 的 tech-design 审批已 approve（approval_record_id=rec-2）。请读取 .crctl/grants/ 与 crctl status/next 确定下一步；本提示不携带任何状态→下一步映射。"
	out := BuildPrompt(Task{IssueID: "issue-052", HandoffNote: note}, "claude")
	if !strings.Contains(out, "rec-1") || !strings.Contains(out, "rec-2") {
		t.Fatalf("merged handoff must render both approval lines verbatim:\n%s", out)
	}
	if !strings.Contains(out, "requirement") || !strings.Contains(out, "tech-design") {
		t.Fatalf("merged handoff must surface both stages:\n%s", out)
	}
	if strings.Contains(out, "quick-create assistant") {
		t.Fatalf("continuation task must not use the quick-create prompt branch:\n%s", out)
	}
}
