package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// claimChatConfigSnapshot claims the given task on the given runtime and
// returns the TaskAgentData payload.
func claimChatConfigSnapshot(t *testing.T, runtimeID, taskID string) *TaskAgentData {
	t.Helper()
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil, testWorkspaceID, "chat-config-snapshot-daemon")
	req = withURLParam(req, "runtimeId", runtimeID)
	w := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)
	var claimResp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	w.JSON(&claimResp)
	if claimResp.Task == nil || claimResp.Task.Agent == nil {
		t.Fatalf("missing task agent in response: %s", w.Body.String())
	}
	return claimResp.Task.Agent
}

// TestClaimTaskByRuntime_ChatConfigSnapshot pins FR-14 (SDD §4.8): a task
// enqueued with a context.chat_config snapshot claims with the SNAPSHOT
// model/thinking, not the agent's current columns; re-claiming the same task
// row returns the same snapshot; tasks without chat_config keep the agent
// columns; and an empty snapshot model passes through verbatim.
func TestClaimTaskByRuntime_ChatConfigSnapshot(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "chat-config snapshot runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "chat-config snapshot agent")
	dbfx.Exec(t, `UPDATE agent SET model = 'agent-current-model', thinking_level = 'agent-current-level' WHERE id = $1`, agentID)

	t.Run("snapshot wins over agent columns", func(t *testing.T) {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID,
			"issue_id":   issueID,
			"context":    testutil.Raw(`'{"chat_config":{"model":"claude-opus-5","thinking_level":"low"}}'::jsonb`),
		})
		agent := claimChatConfigSnapshot(t, runtimeID, taskID)
		if agent.Model != "claude-opus-5" || agent.ThinkingLevel != "low" {
			t.Fatalf("snapshot not honored: model=%q thinking=%q", agent.Model, agent.ThinkingLevel)
		}

		// Change the agent columns: a re-claim of the SAME task row must keep
		// the immutable snapshot (FR-14: retries never re-read the agent).
		dbfx.Exec(t, `UPDATE agent SET model = 'changed-after-enqueue' WHERE id = $1`, agentID)
		agent = claimChatConfigSnapshot(t, runtimeID, taskID)
		if agent.Model != "claude-opus-5" {
			t.Fatalf("re-claim drifted off the snapshot: model=%q", agent.Model)
		}
	})

	t.Run("legacy task without chat_config keeps agent columns", func(t *testing.T) {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID,
			"issue_id":   issueID,
		})
		agent := claimChatConfigSnapshot(t, runtimeID, taskID)
		if agent.Model != "changed-after-enqueue" || agent.ThinkingLevel != "agent-current-level" {
			t.Fatalf("legacy task must read agent columns: model=%q thinking=%q", agent.Model, agent.ThinkingLevel)
		}
	})

	t.Run("empty snapshot model passes through verbatim", func(t *testing.T) {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID,
			"issue_id":   issueID,
			"context":    testutil.Raw(`'{"chat_config":{"model":"","thinking_level":""}}'::jsonb`),
		})
		agent := claimChatConfigSnapshot(t, runtimeID, taskID)
		if agent.Model != "" || agent.ThinkingLevel != "" {
			t.Fatalf("empty snapshot sentinels must pass through verbatim: model=%q thinking=%q", agent.Model, agent.ThinkingLevel)
		}
	})
}

// TestClaimTaskByRuntime_ChatConfigSnapshotIgnoresMalformedContext pins the
// degradation contract: an unparsable context or a chat_config with the wrong
// shape falls back to the agent columns instead of failing the claim.
func TestClaimTaskByRuntime_ChatConfigSnapshotIgnoresMalformedContext(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "chat-config malformed runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "chat-config malformed agent")
	dbfx.Exec(t, `UPDATE agent SET model = 'fallback-model' WHERE id = $1`, agentID)

	for name, contextValue := range map[string]string{
		"unparsable json": `'not json'::jsonb`,
		"wrong shape":     `'{"chat_config":"oops"}'::jsonb`,
	} {
		t.Run(name, func(t *testing.T) {
			taskID := dbfx.Task(t, agentID, testutil.Cols{
				"runtime_id": runtimeID,
				"issue_id":   issueID,
				"context":    testutil.Raw(contextValue),
			})
			agent := claimChatConfigSnapshot(t, runtimeID, taskID)
			if agent.Model != "fallback-model" {
				t.Fatalf("malformed context must fall back to agent columns: model=%q", agent.Model)
			}
		})
	}
}
