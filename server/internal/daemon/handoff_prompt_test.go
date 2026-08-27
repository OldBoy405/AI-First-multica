package daemon

import (
	"strings"
	"testing"
)

// TestBuildPrompt_HandoffNote_AssignmentBranch verifies a handoff note on an
// issue-assignment task renders through the assignment branch — it appears in
// the prompt, framed as a handoff (not a comment to reply to), and does not
// trip the quick-create branch.
func TestBuildPrompt_HandoffNote_AssignmentBranch(t *testing.T) {
	note := "Only touch the login flow; do not change payments."
	out := BuildPrompt(Task{IssueID: "issue-123", HandoffNote: note}, "claude")

	if !strings.Contains(out, note) {
		t.Fatalf("handoff note missing from prompt:\n%s", out)
	}
	if !strings.Contains(out, "handoff note") {
		t.Fatalf("expected handoff framing in prompt:\n%s", out)
	}
	if strings.Contains(out, "quick-create assistant") {
		t.Fatalf("handoff task must not use the quick-create prompt branch:\n%s", out)
	}
	// Still an assignment task: should point the agent at `multica issue get`.
	if !strings.Contains(out, "multica issue get issue-123") {
		t.Fatalf("expected assignment prompt body:\n%s", out)
	}
}

// TestBuildPrompt_NoHandoffNote_Unchanged verifies the assignment prompt is
// unchanged when no handoff note is present (no stray handoff framing).
func TestBuildPrompt_NoHandoffNote_Unchanged(t *testing.T) {
	out := BuildPrompt(Task{IssueID: "issue-123"}, "claude")
	if strings.Contains(out, "handoff note") {
		t.Fatalf("unexpected handoff framing when no note set:\n%s", out)
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
