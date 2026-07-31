// AIFIRST: tool-call summary aggregation (CR-2026-002 TASK-10, AC-6②③).
//
// Zero new probes: the daemon already streams every tool_use/tool_result to
// the server (task_message rows); at task completion the handler aggregates
// that stream into a compact audit summary persisted inside the task result.
// The summary carries tool name, target path and outcome only — never the
// input or output bodies.
package governance

import "encoding/json"

// ToolCall is one summarized invocation.
type ToolCall struct {
	Seq    int    `json:"seq"`
	Tool   string `json:"tool"`
	Target string `json:"target,omitempty"`
	Status string `json:"status"` // "ok" (result observed) | "no_result"
}

// ToolCallSummary caps the persisted list; Total keeps the real count.
type ToolCallSummary struct {
	Total int        `json:"total"`
	Calls []ToolCall `json:"calls"`
}

// MaxSummarizedToolCalls bounds result-JSONB growth on long tasks.
const MaxSummarizedToolCalls = 200

// ToolMsg is the minimal projection of one task_message row the aggregator
// needs (keeps this package free of the sqlc types).
type ToolMsg struct {
	Seq   int
	Type  string // "tool_use" | "tool_result" | others (ignored)
	Tool  string
	Input []byte // raw JSON input of a tool_use, or nil
}

// targetKeys are the path-like input fields worth surfacing. Command bodies
// and file contents are deliberately not on this list.
var targetKeys = []string{"file_path", "path", "notebook_path", "url", "pattern"}

// SummarizeToolCalls walks the message stream in seq order and pairs each
// tool_use with the next tool_result of the same tool name (the persisted
// stream has no call ids; per-tool FIFO matches how backends interleave).
func SummarizeToolCalls(msgs []ToolMsg) ToolCallSummary {
	var calls []ToolCall
	pending := map[string][]int{} // tool name -> indexes into calls awaiting a result
	for _, m := range msgs {
		switch m.Type {
		case "tool_use":
			calls = append(calls, ToolCall{Seq: m.Seq, Tool: m.Tool, Target: extractTarget(m.Input), Status: "no_result"})
			pending[m.Tool] = append(pending[m.Tool], len(calls)-1)
		case "tool_result":
			if q := pending[m.Tool]; len(q) > 0 {
				calls[q[0]].Status = "ok"
				pending[m.Tool] = q[1:]
			}
		}
	}
	s := ToolCallSummary{Total: len(calls), Calls: calls}
	if len(calls) > MaxSummarizedToolCalls {
		s.Calls = calls[:MaxSummarizedToolCalls]
	}
	if s.Calls == nil {
		s.Calls = []ToolCall{}
	}
	return s
}

func extractTarget(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range targetKeys {
		if v, ok := m[k].(string); ok && v != "" {
			if len(v) > 256 {
				v = v[:256]
			}
			return v
		}
	}
	return ""
}
