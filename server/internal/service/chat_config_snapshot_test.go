package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// TestCreateAgentTaskContextMergesChatConfigWithHeadSha pins the SQL-side
// half of the shared chat_config merge (SDD §2.3): the CreateAgentTask CASE
// keeps the head_sha key byte-identical and merges the chat_config object
// next to it. The Go-side key-preservation contract is pinned by
// TestMergeChatConfigContext; together the two cover the "existing keys
// survive the merge" guarantee at both seams.
func TestCreateAgentTaskContextMergesChatConfigWithHeadSha(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, util.UUIDToString(fx.agentID)).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position, origin_type)
		VALUES ($1, $2, 'chat config merge fixture', 'todo', 'medium', $3, 'member', 900002, 0, 'project_chat')
		RETURNING id
	`, fx.workspaceID, fx.projectID, util.UUIDToString(fx.ownerID)).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	queries := db.New(pool)
	readContext := func(taskID string) map[string]any {
		t.Helper()
		var raw string
		if err := pool.QueryRow(ctx, `SELECT context::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&raw); err != nil {
			t.Fatalf("read task context: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("task context is not JSON: %v (%q)", err, raw)
		}
		return parsed
	}

	// head_sha + chat_config: both keys coexist, head_sha unchanged.
	task, err := queries.CreateAgentTask(ctx, db.CreateAgentTaskParams{
		ID:                dbid.NewV7(),
		AgentID:           fx.agentID,
		RuntimeID:         u(runtimeID),
		IssueID:           u(issueID),
		Priority:          0,
		HeadSha:           pgtype.Text{String: "abc123", Valid: true},
		ChatConfig:        mergeChatConfigContext(nil, "claude-opus-5", "high"),
		ProjectID:         u(fx.projectID),
		OriginatorUserID:  fx.ownerID,
		AccountableUserID: fx.ownerID,
	})
	if err != nil {
		t.Fatalf("create agent task: %v", err)
	}
	parsed := readContext(util.UUIDToString(task.ID))
	if parsed["head_sha"] != "abc123" {
		t.Fatalf("head_sha lost in merge: %+v", parsed)
	}
	cc, ok := parsed["chat_config"].(map[string]any)
	if !ok {
		t.Fatalf("chat_config missing: %+v", parsed)
	}
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("chat_config values: %+v", cc)
	}

	// chat_config only: context is exactly the chat_config object.
	task2, err := queries.CreateAgentTask(ctx, db.CreateAgentTaskParams{
		ID:                dbid.NewV7(),
		AgentID:           fx.agentID,
		RuntimeID:         u(runtimeID),
		IssueID:           u(issueID),
		Priority:          0,
		ChatConfig:        mergeChatConfigContext(nil, "m2", ""),
		ProjectID:         u(fx.projectID),
		OriginatorUserID:  fx.ownerID,
		AccountableUserID: fx.ownerID,
	})
	if err != nil {
		t.Fatalf("create agent task (chat_config only): %v", err)
	}
	parsed = readContext(util.UUIDToString(task2.ID))
	if len(parsed) != 1 || parsed["chat_config"] == nil {
		t.Fatalf("chat_config-only context shape: %+v", parsed)
	}

	// Neither: context stays NULL (pre-CR behavior preserved).
	task3, err := queries.CreateAgentTask(ctx, db.CreateAgentTaskParams{
		ID:                dbid.NewV7(),
		AgentID:           fx.agentID,
		RuntimeID:         u(runtimeID),
		IssueID:           u(issueID),
		Priority:          0,
		ProjectID:         u(fx.projectID),
		OriginatorUserID:  fx.ownerID,
		AccountableUserID: fx.ownerID,
	})
	if err != nil {
		t.Fatalf("create agent task (no context): %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT context IS NULL FROM agent_task_queue WHERE id = $1`, task3.ID).Scan(&isNull); err != nil {
		t.Fatalf("read null context: %v", err)
	}
	if !isNull {
		t.Fatalf("no head_sha + no chat_config must leave context NULL")
	}
}

// TestRecordPresenterActivityUsesActiveSessionIssue pins SDD §9 #35: presenter
// activity records on the ACTIVE session's bound container, not the project's
// earliest project_chat issue.
func TestRecordPresenterActivityUsesActiveSessionIssue(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "bind the container", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	project, err := svc.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: u(fx.projectID), WorkspaceID: u(fx.workspaceID),
	})
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	svc.TaskService.recordPresenterActivity(ctx, project, fx.ownerID, "presenter_requested", map[string]string{}, nil)

	var issueID string
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM activity_log WHERE issue_id = $1`, result.IssueID).Scan(&issueID); err != nil {
		t.Fatalf("presenter activity must record on the active session's container %s: %v", result.IssueID, err)
	}
	if issueID != result.IssueID {
		t.Fatalf("activity issue_id = %s, want %s", issueID, result.IssueID)
	}
}

// TestRecordPresenterActivitySkipsUnboundSession pins the degradation half of
// SDD §9 #35: with no bound container the activity is skipped (same visible
// behavior as the old "issue not found").
func TestRecordPresenterActivitySkipsUnboundSession(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	project, err := svc.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: u(fx.projectID), WorkspaceID: u(fx.workspaceID),
	})
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	svc.TaskService.recordPresenterActivity(ctx, project, fx.ownerID, "presenter_requested", map[string]string{}, nil)

	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, fx.projectID).Scan(&count); err != nil {
		t.Fatalf("count activities: %v", err)
	}
	if count != 0 {
		t.Fatalf("unbound session must skip presenter activity, found %d rows", count)
	}
}
