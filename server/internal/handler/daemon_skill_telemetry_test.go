package handler

// AIFIRST: CR-2026-048 TASK-06: claim-path skill usage telemetry.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestClaimTaskByRuntimeWritesSkillUsageTelemetry locks the claim-path write:
// a task claimed with skill-bundles capability produces one skill_usage_event
// row per materialized skill ref (builtins are always materialized), scoped
// to the runtime workspace.
func TestClaimTaskByRuntimeWritesSkillUsageTelemetry(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "SkillTelemetryClaimAgent", []byte("[]"))
	runtimeID := handlerTestRuntimeID(t)

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority)
		VALUES ($1, $2, 'queued', 1000)
		RETURNING id
	`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		testPool.Exec(context.Background(), `DELETE FROM skill_usage_event WHERE task_id = $1`, taskID)
	})

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest(
		http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/claim",
		nil,
		testWorkspaceID,
		"skill-telemetry-test",
	)
	req = withURLParam(req, "runtimeId", runtimeID)
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilitySkillBundlesV1)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rows, refs int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT skill_ref) FROM skill_usage_event WHERE task_id = $1`, taskID,
	).Scan(&rows, &refs); err != nil {
		t.Fatalf("count telemetry: %v", err)
	}
	if rows == 0 {
		t.Fatal("claim produced no skill usage telemetry rows")
	}
	if refs == 0 {
		t.Fatal("claim produced no distinct skill refs (builtin refs are always materialized)")
	}
	var workspaceOK bool
	if err := testPool.QueryRow(ctx,
		`SELECT bool_and(workspace_id = $2::uuid) FROM skill_usage_event WHERE task_id = $1`, taskID, testWorkspaceID,
	).Scan(&workspaceOK); err != nil {
		t.Fatalf("check workspace scoping: %v", err)
	}
	if !workspaceOK {
		t.Fatal("telemetry rows not scoped to the runtime workspace")
	}
}
