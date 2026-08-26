package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

// AIFIRST: CR-2026-051 TASK-01 — golden JSON contract tests for the
// approval-gate event payload. The JSON shape is a client-visible WebSocket
// frame contract (broadcast via listeners.go#SubscribeAll), so these tests
// pin the exact key order and the always-present shell_issue_id key.

const (
	testShellIssueID = "11111111-1111-4111-8111-111111111111"
)

func TestApprovalGateEnteredPayloadGoldenJSON(t *testing.T) {
	shell := testShellIssueID
	p := ApprovalGateEnteredPayload{
		CRID:         "CR-2026-051",
		Status:       "tech-design-review-pending",
		EventID:      "CR-2026-051:status:abc1234",
		ShellIssueID: &shell,
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"cr_id":"CR-2026-051","status":"tech-design-review-pending","event_id":"CR-2026-051:status:abc1234","shell_issue_id":"11111111-1111-4111-8111-111111111111"}`
	if string(got) != want {
		t.Errorf("golden JSON mismatch:\n got %s\nwant %s", got, want)
	}

	var back ApprovalGateEnteredPayload
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Errorf("round-trip mismatch: got %+v want %+v", back, p)
	}
}

func TestApprovalGateEnteredPayloadNullShellIssueID(t *testing.T) {
	// The key must stay present with JSON null (no omitempty).
	p := ApprovalGateEnteredPayload{
		CRID:    "CR-2026-051",
		Status:  "tech-design-review-pending",
		EventID: "CR-2026-051:status:abc1234",
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"cr_id":"CR-2026-051","status":"tech-design-review-pending","event_id":"CR-2026-051:status:abc1234","shell_issue_id":null}`
	if string(got) != want {
		t.Errorf("golden JSON mismatch:\n got %s\nwant %s", got, want)
	}

	var back ApprovalGateEnteredPayload
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Errorf("round-trip mismatch: got %+v want %+v", back, p)
	}
}

func TestApprovalGateEnteredEventConstant(t *testing.T) {
	if EventCRApprovalGateEntered != "cr:approval-gate-entered" {
		t.Errorf("unexpected constant value: %q", EventCRApprovalGateEntered)
	}
	if EventCRApprovalGateEntered == EventCRUpdated {
		t.Error("approval-gate event must not alias cr:updated")
	}
}
