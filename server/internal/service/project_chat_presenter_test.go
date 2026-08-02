package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestSendProjectChatMessagePresenterGuard is CR-2026-010 TASK-03's core proof
// (SDD §4.3/DD-6, acceptance criteria 1-2): with no active presenter, only
// owner/admin may send (CR-A default, a strict subset); with an active
// presenter, only the presenter or owner/admin may send, and owner/admin lose
// their queue-jump priority (100 -> ordinary tier) so they no longer preempt
// the presenter's own messages.
func TestSendProjectChatMessagePresenterGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	fx := createChatPresenterFixture(t, ctx, pool)

	t.Run("no presenter: owner sends, enqueued at issue priority tier", func(t *testing.T) {
		_, task, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.ownerID, "owner message, no presenter")
		if err != nil {
			t.Fatalf("owner send with no presenter: %v", err)
		}
		if task.Priority != PreemptPriorityOwnerAdmin {
			t.Fatalf("owner with no presenter should still preempt: want priority %d, got %d", PreemptPriorityOwnerAdmin, task.Priority)
		}
		freeIssueAgentSlot(t, ctx, pool, task.ID)
	})

	t.Run("no presenter: plain member rejected", func(t *testing.T) {
		commentsBefore := countProjectChatComments(t, ctx, pool, fx.issue.ID)
		tasksBefore := countProjectChatTasks(t, ctx, pool, fx.issue.ID)

		_, _, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.memberID, "plain member, no presenter")
		var required *ErrPresenterRequired
		if !errors.As(err, &required) {
			t.Fatalf("expected ErrPresenterRequired for plain member with no presenter, got %v", err)
		}
		if required.PresenterUserID != "" {
			t.Fatalf("no active presenter yet: expected empty PresenterUserID, got %q", required.PresenterUserID)
		}
		assertNoNewRows(t, ctx, pool, fx.issue.ID, commentsBefore, tasksBefore)
	})

	// Promote memberID to presenter for the remaining subtests.
	grantActivePresenter(t, ctx, pool, fx.workspaceID, fx.projectID, fx.memberID, fx.ownerID)

	t.Run("active presenter: presenter sends at ordinary priority", func(t *testing.T) {
		_, task, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.memberID, "presenter message")
		if err != nil {
			t.Fatalf("presenter send: %v", err)
		}
		if task.Priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("presenter is a plain member: priority must not be the owner/admin preempt tier")
		}
		freeIssueAgentSlot(t, ctx, pool, task.ID)
	})

	t.Run("active presenter: owner sends but priority suppressed to non-preempt", func(t *testing.T) {
		_, task, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.ownerID, "owner message while presenter active")
		if err != nil {
			t.Fatalf("owner send while presenter active: %v", err)
		}
		if task.Priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("owner/admin must not preempt an active presenter's queue position: got priority %d", task.Priority)
		}
		freeIssueAgentSlot(t, ctx, pool, task.ID)
	})

	t.Run("active presenter: admin sends but priority suppressed to non-preempt", func(t *testing.T) {
		_, task, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.adminID, "admin message while presenter active")
		if err != nil {
			t.Fatalf("admin send while presenter active: %v", err)
		}
		if task.Priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("owner/admin must not preempt an active presenter's queue position: got priority %d", task.Priority)
		}
		freeIssueAgentSlot(t, ctx, pool, task.ID)
	})

	t.Run("active presenter: other member rejected, presenter id surfaced", func(t *testing.T) {
		commentsBefore := countProjectChatComments(t, ctx, pool, fx.issue.ID)
		tasksBefore := countProjectChatTasks(t, ctx, pool, fx.issue.ID)

		_, _, err := svc.SendProjectChatMessage(ctx, fx.issue, fx.agentID, fx.otherID, "other member, presenter active")
		var required *ErrPresenterRequired
		if !errors.As(err, &required) {
			t.Fatalf("expected ErrPresenterRequired for non-presenter member, got %v", err)
		}
		if want := util.UUIDToString(fx.memberID); required.PresenterUserID != want {
			t.Fatalf("expected presenter_user_id %q, got %q", want, required.PresenterUserID)
		}
		assertNoNewRows(t, ctx, pool, fx.issue.ID, commentsBefore, tasksBefore)
	})
}

type chatPresenterFixture struct {
	workspaceID string
	projectID   string
	issue       db.Issue
	agentID     pgtype.UUID
	ownerID     pgtype.UUID
	adminID     pgtype.UUID
	memberID    pgtype.UUID
	otherID     pgtype.UUID
}

func createChatPresenterFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) chatPresenterFixture {
	t.Helper()

	suffix := time.Now().UnixNano()
	slug := fmt.Sprintf("chat-presenter-guard-%d", suffix)

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Chat Presenter Guard Test", slug, "temporary CR-2026-010 TASK-03 test workspace", "CPG").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	makeUser := func(label string) string {
		email := fmt.Sprintf("chat-presenter-guard-%s-%d@multica.ai", label, suffix)
		var userID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
		`, "Chat Presenter Guard "+label, email).Scan(&userID); err != nil {
			t.Fatalf("create user %s: %v", label, err)
		}
		return userID
	}
	ownerID := makeUser("owner")
	adminID := makeUser("admin")
	memberID := makeUser("member")
	otherID := makeUser("other")

	for _, m := range []struct{ id, role string }{
		{ownerID, "owner"}, {adminID, "admin"}, {memberID, "member"}, {otherID, "member"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)
		`, workspaceID, m.id, m.role); err != nil {
			t.Fatalf("create member %s: %v", m.role, err)
		}
	}

	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, workspaceID, "Chat Presenter Guard Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, NULL, $2, 'cloud', 'chat_presenter_guard_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Chat Presenter Guard Runtime", ownerID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 10, $4)
		RETURNING id
	`, workspaceID, "Chat Presenter Guard Agent", runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position, origin_type)
		VALUES ($1, $2, 'Team Agent Chat', 'todo', 'medium', $3, 'member', $4, 0, 'project_chat')
		RETURNING id
	`, workspaceID, projectID, ownerID, 930000+suffix%100000).Scan(&issueID); err != nil {
		t.Fatalf("create chat issue: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		pool.Exec(cleanupCtx, `DELETE FROM comment WHERE issue_id = $1`, issueID)
		pool.Exec(cleanupCtx, `DELETE FROM project_presenter_grant WHERE project_id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		for _, uid := range []string{ownerID, adminID, memberID, otherID} {
			pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, uid)
		}
	})

	issue, err := db.New(pool).GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load chat issue: %v", err)
	}

	return chatPresenterFixture{
		workspaceID: workspaceID,
		projectID:   projectID,
		issue:       issue,
		agentID:     util.MustParseUUID(agentID),
		ownerID:     util.MustParseUUID(ownerID),
		adminID:     util.MustParseUUID(adminID),
		memberID:    util.MustParseUUID(memberID),
		otherID:     util.MustParseUUID(otherID),
	}
}

// grantActivePresenter inserts an already-active presenter grant directly
// (bypassing request/approve) since this test only needs the resulting state,
// not the transition path (covered by project_presenter_test.go).
func grantActivePresenter(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, projectID string, userID, grantedBy pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_presenter_grant (workspace_id, project_id, user_id, status, granted_by)
		VALUES ($1, $2, $3, 'active', $4)
	`, workspaceID, projectID, util.UUIDToString(userID), util.UUIDToString(grantedBy)); err != nil {
		t.Fatalf("grant active presenter: %v", err)
	}
}

// freeIssueAgentSlot moves a just-enqueued task out of 'queued' so the next
// subtest's send doesn't trip idx_one_pending_task_per_issue_agent (migration
// 037: at most one queued/dispatched task per issue+agent) — this test
// intentionally reuses one issue+agent across many successful sends, which a
// real client would only do once the daemon has claimed the prior message.
func freeIssueAgentSlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("free issue/agent task slot: %v", err)
	}
}

func countProjectChatComments(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID pgtype.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	return n
}

func countProjectChatTasks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID pgtype.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

func assertNoNewRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID pgtype.UUID, commentsBefore, tasksBefore int64) {
	t.Helper()
	if got := countProjectChatComments(t, ctx, pool, issueID); got != commentsBefore {
		t.Fatalf("rejected send must not persist a comment: before=%d after=%d", commentsBefore, got)
	}
	if got := countProjectChatTasks(t, ctx, pool, issueID); got != tasksBefore {
		t.Fatalf("rejected send must not enqueue a task: before=%d after=%d", tasksBefore, got)
	}
}
