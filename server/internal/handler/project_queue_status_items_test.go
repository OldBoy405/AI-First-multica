package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CR-2026-007 DD-3: GET /api/projects/{id}/queue-status?include=items appends
// the pending queue rows to the depth/limit payload. These tests pin the
// opt-in contract: the no-param response keeps its exact shape, and the items
// branch reads with the same pending filter as the depth count so the two can
// never disagree.

// seedQueueStatusTask inserts a queued task on the issue with the given
// priority, originator (empty string means NULL — autopilot/agent-sourced
// tasks are deliberately unattributed) and trigger summary (empty string
// means NULL — quick-create tasks have no trigger snapshot).
func seedQueueStatusTask(t *testing.T, agentID, issueID string, priority int32, originatorID, summary string) string {
	t.Helper()
	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id, originator_user_id, trigger_summary)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'queued', $2, $3, NULLIF($4, '')::uuid, NULLIF($5, ''))
		RETURNING id
	`, agentID, priority, issueID, originatorID, summary).Scan(&taskID); err != nil {
		t.Fatalf("seed queued task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return taskID
}

func getQueueStatus(t *testing.T, projectID, query string) (int, []byte) {
	t.Helper()
	req := newRequest(http.MethodGet, "/api/projects/"+projectID+"/queue-status"+query, nil)
	req = withURLParam(req, "id", projectID)
	w := httptest.NewRecorder()
	testHandler.GetProjectQueueStatus(w, req)
	return w.Code, w.Body.Bytes()
}

// TestProjectQueueStatusNoIncludeBackwardCompatible: without ?include=items
// the response carries exactly queue_depth and queue_limit — no items key —
// so existing consumers see the pre-CR payload unchanged (AC-5).
func TestProjectQueueStatusNoIncludeBackwardCompatible(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "QItemsCompatAgent", []byte("[]"))
	projectID := createCapacityTestProject(t, "qitems-compat", "")
	issueID := createCapacityTestIssue(t, projectID, agentID, testUserID, 8101)
	seedQueueStatusTask(t, agentID, issueID, 0, testUserID, "compat check")

	code, body := getQueueStatus(t, projectID, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := payload["items"]; ok {
		t.Fatalf("no-param response must not contain items key: %s", body)
	}
	if len(payload) != 2 {
		t.Fatalf("expected exactly queue_depth + queue_limit, got keys %v", payload)
	}
	var depth int64
	if err := json.Unmarshal(payload["queue_depth"], &depth); err != nil || depth != 1 {
		t.Fatalf("expected queue_depth 1, got %s (err %v)", payload["queue_depth"], err)
	}
}

type queueStatusItemsResponse struct {
	QueueDepth int64 `json:"queue_depth"`
	QueueLimit int64 `json:"queue_limit"`
	Items      []struct {
		TaskID     string `json:"task_id"`
		Status     string `json:"status"`
		Priority   int32  `json:"priority"`
		CreatedAt  string `json:"created_at"`
		Originator *struct {
			ID        string  `json:"id"`
			Name      string  `json:"name"`
			AvatarURL *string `json:"avatar_url"`
		} `json:"originator"`
		Summary string `json:"summary"`
	} `json:"items"`
}

// TestProjectQueueStatusIncludeItems covers the DD-3 item contract in one
// seeded queue: NULL-originator tasks are kept (LEFT JOIN — depth and item
// count must match), priority orders before FIFO, a NULL trigger_summary
// reads as "", and the originator object carries the user's display fields.
func TestProjectQueueStatusIncludeItems(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "QItemsAgent", []byte("[]"))
	projectID := createCapacityTestProject(t, "qitems-items", "")
	issueA := createCapacityTestIssue(t, projectID, agentID, testUserID, 8102)
	issueB := createCapacityTestIssue(t, projectID, agentID, testUserID, 8103)

	// Created first at priority 0 with a human originator and a summary;
	// the later priority-100 system task (NULL originator, NULL summary)
	// must still jump ahead of it.
	memberTask := seedQueueStatusTask(t, agentID, issueA, 0, testUserID, "please fix the login flow")
	systemTask := seedQueueStatusTask(t, agentID, issueB, 100, "", "")

	code, body := getQueueStatus(t, projectID, "?include=items")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var resp queueStatusItemsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if int(resp.QueueDepth) != len(resp.Items) {
		t.Fatalf("queue_depth %d != len(items) %d", resp.QueueDepth, len(resp.Items))
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d: %s", len(resp.Items), body)
	}

	first, second := resp.Items[0], resp.Items[1]
	if first.TaskID != systemTask || second.TaskID != memberTask {
		t.Fatalf("expected priority-100 task first, got order [%s, %s]", first.TaskID, second.TaskID)
	}
	if first.Originator != nil {
		t.Fatalf("system task originator should be null, got %+v", first.Originator)
	}
	if first.Summary != "" {
		t.Fatalf("system task summary should be empty, got %q", first.Summary)
	}
	if first.Status != "queued" || first.Priority != 100 || first.CreatedAt == "" {
		t.Fatalf("unexpected system item fields: %+v", first)
	}
	if second.Originator == nil {
		t.Fatalf("member task originator missing: %s", body)
	}
	if second.Originator.ID != testUserID || second.Originator.Name != handlerTestName {
		t.Fatalf("unexpected originator: %+v", second.Originator)
	}
	if second.Summary != "please fix the login flow" {
		t.Fatalf("unexpected summary: %q", second.Summary)
	}

	// The originator key must be serialized as an explicit null, not omitted,
	// so clients can distinguish "system task" from a missing field.
	var rawItems struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &rawItems); err != nil {
		t.Fatalf("decode raw items: %v", err)
	}
	if raw, ok := rawItems.Items[0]["originator"]; !ok || string(raw) != "null" {
		t.Fatalf("expected originator key with null value, got %s", raw)
	}
}
