package handler

// AIFIRST: CompleteTask tool-call summary aggregation test (CR-2026-002
// TASK-10, AC-6②③): the persisted tool_use/tool_result stream is summarized
// at completion into result.tool_calls — names, target paths and outcomes,
// never input/output bodies.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestCompleteTask_AggregatesToolCallSummary(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'task-10 tool call summary fixture', 'in_progress', 'none', $2, 'member', 4210, 0)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("setup: create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
		VALUES ($1, $2, $3, 'running', 0, now())
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	for _, m := range []struct {
		seq   int
		typ   string
		tool  string
		input string
	}{
		{1, "tool_use", "Read", `{"file_path": "/repo/main.go"}`},
		{2, "tool_result", "Read", ""},
		{3, "tool_use", "Bash", `{"command": "curl -H 'Authorization: hunter2' example.com"}`},
		{4, "tool_result", "Bash", ""},
		{5, "tool_use", "Edit", `{"file_path": "/repo/util.go", "old_string": "private body"}`},
	} {
		input := []byte(m.input)
		if m.input == "" {
			input = nil
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO task_message (task_id, seq, type, tool, input)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		`, taskID, m.seq, m.typ, m.tool, input); err != nil {
			t.Fatalf("setup: insert task message seq %d: %v", m.seq, err)
		}
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM task_message WHERE task_id = $1`, taskID) })

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": "done"}, testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rawResult []byte
	if err := testPool.QueryRow(ctx, `SELECT result FROM agent_task_queue WHERE id = $1`, taskID).Scan(&rawResult); err != nil {
		t.Fatalf("read task result: %v", err)
	}
	var result struct {
		ToolCalls struct {
			Total int `json:"total"`
			Calls []struct {
				Tool   string `json:"tool"`
				Target string `json:"target"`
				Status string `json:"status"`
			} `json:"calls"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("task result not JSON: %v (%s)", err, rawResult)
	}
	tc := result.ToolCalls
	if tc.Total != 3 || len(tc.Calls) != 3 {
		t.Fatalf("want 3 summarized calls, got %+v", tc)
	}
	if tc.Calls[0].Tool != "Read" || tc.Calls[0].Target != "/repo/main.go" || tc.Calls[0].Status != "ok" {
		t.Fatalf("bad Read summary: %+v", tc.Calls[0])
	}
	if tc.Calls[1].Tool != "Bash" || tc.Calls[1].Target != "" {
		t.Fatalf("Bash command body must not surface as target: %+v", tc.Calls[1])
	}
	if tc.Calls[2].Status != "no_result" {
		t.Fatalf("unpaired Edit must be no_result: %+v", tc.Calls[2])
	}
	if s := string(rawResult); strings.Contains(s, "hunter2") || strings.Contains(s, "private body") {
		t.Fatalf("result leaked tool input bodies: %s", s)
	}
}
