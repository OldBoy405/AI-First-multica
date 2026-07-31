package governance

// AIFIRST: tool-call summary aggregation tests (CR-2026-002 TASK-10, AC-6②).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarizeToolCalls(t *testing.T) {
	msgs := []ToolMsg{
		{Seq: 1, Type: "tool_use", Tool: "Read", Input: []byte(`{"file_path":"/repo/a.go"}`)},
		{Seq: 2, Type: "tool_result", Tool: "Read"},
		{Seq: 3, Type: "tool_use", Tool: "Bash", Input: []byte(`{"command":"rm -rf / --secret"}`)},
		{Seq: 4, Type: "tool_result", Tool: "Bash"},
		{Seq: 5, Type: "text"},
		{Seq: 6, Type: "tool_use", Tool: "Edit", Input: []byte(`{"file_path":"/repo/b.go","old_string":"body"}`)},
		// no result for the Edit — stream ended mid-flight
	}
	s := SummarizeToolCalls(msgs)
	if s.Total != 3 || len(s.Calls) != 3 {
		t.Fatalf("want 3 calls, got %+v", s)
	}
	if s.Calls[0].Tool != "Read" || s.Calls[0].Target != "/repo/a.go" || s.Calls[0].Status != "ok" {
		t.Fatalf("bad first call: %+v", s.Calls[0])
	}
	// Audit minimization: a Bash command body is not a target path.
	if s.Calls[1].Target != "" {
		t.Fatalf("command bodies must not surface as targets: %+v", s.Calls[1])
	}
	if s.Calls[2].Status != "no_result" {
		t.Fatalf("unpaired tool_use must be no_result: %+v", s.Calls[2])
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "old_string") {
		t.Fatalf("summary leaked input bodies: %s", raw)
	}
}

func TestSummarizeToolCallsCap(t *testing.T) {
	msgs := make([]ToolMsg, 0, MaxSummarizedToolCalls+50)
	for i := 0; i < MaxSummarizedToolCalls+50; i++ {
		msgs = append(msgs, ToolMsg{Seq: i, Type: "tool_use", Tool: "Read"})
	}
	s := SummarizeToolCalls(msgs)
	if s.Total != MaxSummarizedToolCalls+50 || len(s.Calls) != MaxSummarizedToolCalls {
		t.Fatalf("cap broken: total=%d calls=%d", s.Total, len(s.Calls))
	}
}

func TestSummarizeToolCallsEmpty(t *testing.T) {
	s := SummarizeToolCalls(nil)
	if s.Total != 0 || s.Calls == nil || len(s.Calls) != 0 {
		t.Fatalf("empty stream must give Total=0 and a non-nil empty list: %+v", s)
	}
}
