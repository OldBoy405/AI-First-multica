package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newSendKernelService wires an IssueService with a TaskService and a static
// catalog so the full send kernel (guards -> resolve -> 搂4.3 -> tx) runs
// against a real database.
func newSendKernelService(pool *pgxpool.Pool) *IssueService {
	queries := db.New(pool)
	taskSvc := NewTaskService(queries, pool, nil, events.New())
	return &IssueService{
		Queries:     queries,
		TxStarter:   pool,
		Bus:         events.New(),
		TaskService: taskSvc,
		ChatCatalog: &fakeChatCatalogPort{cacheResult: agent.Catalog{Models: []agent.Model{
			{ID: "claude-opus-5", Default: true, Thinking: &agent.ModelThinking{SupportedLevels: []agent.ThinkingLevel{{Value: "high"}, {Value: "low"}}}},
		}}, cacheOK: true},
	}
}

// sendKernelFixture seeds a workspace with owner/admin/member/other members,
// a bound Team Agent (claude, online) and an ensured active session.
type sendKernelFixture struct {
	workspaceID string
	projectID   string
	sessionID   string
	agentID     pgtype.UUID
	ownerID     pgtype.UUID
	adminID     pgtype.UUID
	memberID    pgtype.UUID
	otherID     pgtype.UUID
}

func newSendKernelFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) sendKernelFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	slug := fmt.Sprintf("send-kernel-%d", suffix)

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, "Send Kernel Test", slug, "temporary CR-2026-056 send kernel workspace", "SKT").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	makeUser := func(label string) string {
		var userID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
		`, "Send Kernel "+label, fmt.Sprintf("send-kernel-%s-%d@multica.ai", label, suffix)).Scan(&userID); err != nil {
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
		if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			workspaceID, m.id, m.role); err != nil {
			t.Fatalf("create member %s: %v", m.role, err)
		}
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'sk-runtime', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2) RETURNING id
	`, workspaceID, ownerID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, model, thinking_level)
		VALUES ($1, 'sk-agent', 'cloud', '{}'::jsonb, $2, 'workspace', 10, $3, '', '{}'::jsonb, '[]'::jsonb, 'claude-opus-5', 'high')
		RETURNING id
	`, workspaceID, runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings)
		VALUES ($1, 'Send Kernel Project', jsonb_build_object('team_agent_id', $2::text)) RETURNING id
	`, workspaceID, agentID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	svc := newSendKernelService(pool)
	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(ownerID))
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if view.SessionID == "" {
		t.Fatalf("expected an ensured session")
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM project_presenter_grant WHERE project_id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM project_chat_session WHERE project_id = $1`, projectID)
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

	return sendKernelFixture{
		workspaceID: workspaceID,
		projectID:   projectID,
		sessionID:   view.SessionID,
		agentID:     u(agentID),
		ownerID:     u(ownerID),
		adminID:     u(adminID),
		memberID:    u(memberID),
		otherID:     u(otherID),
	}
}

// TestMergeChatConfigContext pins the shared chat_config merge (SDD 搂2.3),
// the single implementation consumed by BOTH seams: the Team Agent enqueue
// (CreateAgentTask merges its output over the SQL-built head_sha key) and the
// Private Ask send path (CreateChatTask context parameter, TASK-13/BLOCK-005).
func TestMergeChatConfigContext(t *testing.T) {
	t.Parallel()

	// Nil existing: only chat_config.
	got := mergeChatConfigContext(nil, "claude-opus-5", "high")
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("merged context is not valid JSON: %v (%q)", err, got)
	}
	cc, ok := parsed["chat_config"].(map[string]any)
	if !ok {
		t.Fatalf("chat_config key missing: %s", got)
	}
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("chat_config values: %+v", cc)
	}
	if len(parsed) != 1 {
		t.Fatalf("nil existing must yield exactly the chat_config key: %+v", parsed)
	}

	// Existing keys are preserved (key-preservation contract, one test shared
	// by both seams): head_sha stays byte-identical next to chat_config.
	got = mergeChatConfigContext([]byte(`{"head_sha":"abc123"}`), "m", "t")
	parsed = map[string]any{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("merged context: %v", err)
	}
	if parsed["head_sha"] != "abc123" {
		t.Fatalf("existing head_sha key lost: %+v", parsed)
	}
	if parsed["chat_config"].(map[string]any)["model"] != "m" {
		t.Fatalf("chat_config not merged alongside existing keys: %+v", parsed)
	}

	// thinking_level="" is recorded verbatim ("do not inject").
	got = mergeChatConfigContext(nil, "", "")
	parsed = map[string]any{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("empty-sentinel merge: %v", err)
	}
	if parsed["chat_config"].(map[string]any)["thinking_level"] != "" || parsed["chat_config"].(map[string]any)["model"] != "" {
		t.Fatalf("empty sentinels must survive verbatim: %+v", parsed)
	}

	// Unparsable existing JSON is discarded, never fails the merge.
	got = mergeChatConfigContext([]byte("not json"), "m", "")
	parsed = map[string]any{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unparsable existing must degrade to chat_config only: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("unparsable existing must be dropped: %+v", parsed)
	}
}

// TestSendProjectChatMessagePresenterGuard is the CR-2026-010 presenter
// contract re-pinned on the CR-2026-056 send kernel (SDD 搂4.3/DD-6): with no
// active presenter, only owner/admin may send; with an active presenter, only
// the presenter or owner/admin may send, and owner/admin lose their
// queue-jump priority so they no longer preempt the presenter's own messages.
// The guard runs before any write, so rejections leave the session unbound.
func TestSendProjectChatMessagePresenterGuard(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	t.Run("no presenter: owner sends, enqueued at preempt tier", func(t *testing.T) {
		result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "owner message, no presenter", nil)
		if err != nil {
			t.Fatalf("owner send with no presenter: %v", err)
		}
		var priority int32
		if err := pool.QueryRow(ctx, `SELECT priority FROM agent_task_queue WHERE id = $1`, result.TaskID).Scan(&priority); err != nil {
			t.Fatalf("load task: %v", err)
		}
		if priority != PreemptPriorityOwnerAdmin {
			t.Fatalf("owner with no presenter should still preempt: want priority %d, got %d", PreemptPriorityOwnerAdmin, priority)
		}
		freeIssueAgentSlot(t, ctx, pool, u(result.TaskID))
	})

	t.Run("no presenter: plain member rejected, nothing persisted", func(t *testing.T) {
		before := sendKernelState(t, ctx, pool, fx)
		_, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.memberID, "plain member, no presenter", nil)
		var required *ErrPresenterRequired
		if !errors.As(err, &required) {
			t.Fatalf("expected ErrPresenterRequired for plain member with no presenter, got %v", err)
		}
		if required.PresenterUserID != "" {
			t.Fatalf("no active presenter yet: expected empty PresenterUserID, got %q", required.PresenterUserID)
		}
		sendKernelStateUnchanged(t, ctx, pool, fx, before)
	})

	// Promote memberID to presenter for the remaining subtests.
	grantActivePresenter(t, ctx, pool, fx.workspaceID, fx.projectID, fx.memberID, fx.ownerID)

	t.Run("active presenter: presenter sends at ordinary priority", func(t *testing.T) {
		result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.memberID, "presenter message", nil)
		if err != nil {
			t.Fatalf("presenter send: %v", err)
		}
		var priority int32
		if err := pool.QueryRow(ctx, `SELECT priority FROM agent_task_queue WHERE id = $1`, result.TaskID).Scan(&priority); err != nil {
			t.Fatalf("load task: %v", err)
		}
		if priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("presenter is a plain member: priority must not be the owner/admin preempt tier")
		}
		freeIssueAgentSlot(t, ctx, pool, u(result.TaskID))
	})

	t.Run("active presenter: owner sends but priority suppressed to non-preempt", func(t *testing.T) {
		result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "owner message while presenter active", nil)
		if err != nil {
			t.Fatalf("owner send while presenter active: %v", err)
		}
		var priority int32
		if err := pool.QueryRow(ctx, `SELECT priority FROM agent_task_queue WHERE id = $1`, result.TaskID).Scan(&priority); err != nil {
			t.Fatalf("load task: %v", err)
		}
		if priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("owner/admin must not preempt an active presenter's queue position: got priority %d", priority)
		}
		freeIssueAgentSlot(t, ctx, pool, u(result.TaskID))
	})

	t.Run("active presenter: admin sends but priority suppressed to non-preempt", func(t *testing.T) {
		result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.adminID, "admin message while presenter active", nil)
		if err != nil {
			t.Fatalf("admin send while presenter active: %v", err)
		}
		var priority int32
		if err := pool.QueryRow(ctx, `SELECT priority FROM agent_task_queue WHERE id = $1`, result.TaskID).Scan(&priority); err != nil {
			t.Fatalf("load task: %v", err)
		}
		if priority == PreemptPriorityOwnerAdmin {
			t.Fatalf("owner/admin must not preempt an active presenter's queue position: got priority %d", priority)
		}
		freeIssueAgentSlot(t, ctx, pool, u(result.TaskID))
	})

	t.Run("active presenter: other member rejected, presenter id surfaced", func(t *testing.T) {
		before := sendKernelState(t, ctx, pool, fx)
		_, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.otherID, "other member, presenter active", nil)
		var required *ErrPresenterRequired
		if !errors.As(err, &required) {
			t.Fatalf("expected ErrPresenterRequired for non-presenter member, got %v", err)
		}
		if want := util.UUIDToString(fx.memberID); required.PresenterUserID != want {
			t.Fatalf("expected presenter_user_id %q, got %q", want, required.PresenterUserID)
		}
		sendKernelStateUnchanged(t, ctx, pool, fx, before)
	})
}

// sendKernelStateSnapshot captures every row class the send transaction may write.
type sendKernelStateSnapshot struct {
	issueCount   int64
	commentCount int64
	taskCount    int64
	issueID      *string // session.issue_id
}

func sendKernelState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fx sendKernelFixture) sendKernelStateSnapshot {
	t.Helper()
	var s sendKernelStateSnapshot
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'`, fx.projectID).Scan(&s.issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, fx.projectID).Scan(&s.commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, fx.projectID).Scan(&s.taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM project_chat_session WHERE id = $1`, fx.sessionID).Scan(&s.issueID); err != nil {
		t.Fatalf("read session issue_id: %v", err)
	}
	return s
}

func sendKernelStateUnchanged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fx sendKernelFixture, before sendKernelStateSnapshot) {
	t.Helper()
	after := sendKernelState(t, ctx, pool, fx)
	if after.issueCount != before.issueCount || after.commentCount != before.commentCount || after.taskCount != before.taskCount {
		t.Fatalf("rejected send persisted rows: before %+v after %+v", before, after)
	}
	if (after.issueID == nil) != (before.issueID == nil) || (after.issueID != nil && before.issueID != nil && *after.issueID != *before.issueID) {
		t.Fatalf("rejected send changed session.issue_id: before %v after %v", before.issueID, after.issueID)
	}
}

// TestSendProjectChatMessageFirstSendBindsContainerAndSnapshotsConfig pins the
// FR-16 single-transaction send: the first send binds the container (origin_id
// = session id), writes the comment + task, and stamps the task context with
// the resolved chat_config snapshot; a follow-up send reuses the same
// container (idempotent bind, AC-13's same-issue guarantee).
func TestSendProjectChatMessageFirstSendBindsContainerAndSnapshotsConfig(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	result, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "first message", nil)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if result.SessionID != fx.sessionID {
		t.Fatalf("session_id must echo the request: got %q want %q", result.SessionID, fx.sessionID)
	}
	if result.IssueID == "" || result.CommentID == "" || result.TaskID == "" {
		t.Fatalf("send result missing ids: %+v", result)
	}

	// session.issue_id is written to the bound container.
	var sessionIssueID, originID string
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM project_chat_session WHERE id = $1`, fx.sessionID).Scan(&sessionIssueID); err != nil {
		t.Fatalf("read session issue: %v", err)
	}
	if sessionIssueID != result.IssueID {
		t.Fatalf("session.issue_id = %s, want %s", sessionIssueID, result.IssueID)
	}
	if err := pool.QueryRow(ctx, `SELECT origin_id::text FROM issue WHERE id = $1`, result.IssueID).Scan(&originID); err != nil {
		t.Fatalf("read container origin: %v", err)
	}
	if originID != fx.sessionID {
		t.Fatalf("container origin_id = %s, want session %s", originID, fx.sessionID)
	}

	// The task snapshot is the resolved config, merged into context.
	var contextRaw string
	if err := pool.QueryRow(ctx, `SELECT context::text FROM agent_task_queue WHERE id = $1`, result.TaskID).Scan(&contextRaw); err != nil {
		t.Fatalf("read task context: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(contextRaw), &parsed); err != nil {
		t.Fatalf("task context is not JSON: %v (%q)", err, contextRaw)
	}
	cc, ok := parsed["chat_config"].(map[string]any)
	if !ok {
		t.Fatalf("task context missing chat_config: %s", contextRaw)
	}
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("chat_config snapshot = %+v, want resolved claude-opus-5/high", cc)
	}

	// Follow-up send reuses the same container (Bind is idempotent).
	freeIssueAgentSlot(t, ctx, pool, u(result.TaskID))
	again, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "second message", nil)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if again.IssueID != result.IssueID {
		t.Fatalf("second send bound a different container: %s vs %s", again.IssueID, result.IssueID)
	}
}

// TestSendProjectChatMessageRollbackFiveZeroResidue is the BLOCK-003
// acceptance fixture: an in-transaction failure (draft attachment already
// bound -> 409) rolls the WHOLE send back 鈥?no container issue, no
// session.issue_id, no comment, no task, and the attachment is untouched.
// The assertions read rows post-transaction; no compensating deletes exist on
// this path.
func TestSendProjectChatMessageRollbackFiveZeroResidue(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	// Seed a DRAFT attachment that is already bound to an ordinary issue: the
	// bind predicate (five targets empty) excludes it, so the send tx fails
	// AFTER the container bind + comment + enqueue and must roll all of them
	// back.
	var ordinaryIssueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, $2, 'ordinary', 'todo', 'medium', $3, 'member', 900001, 0) RETURNING id
	`, fx.workspaceID, fx.projectID, util.UUIDToString(fx.ownerID)).Scan(&ordinaryIssueID); err != nil {
		t.Fatalf("seed ordinary issue: %v", err)
	}
	var attachmentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO attachment (workspace_id, issue_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
		VALUES ($1, $2, 'member', $3, 'already-bound.pdf', 'https://cdn.test/bound.pdf', 'application/pdf', 10)
		RETURNING id::text
	`, fx.workspaceID, ordinaryIssueID, util.UUIDToString(fx.ownerID)).Scan(&attachmentID); err != nil {
		t.Fatalf("seed bound attachment: %v", err)
	}

	_, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "send that must roll back", []pgtype.UUID{u(attachmentID)})
	if !errors.Is(err, ErrAttachmentAlreadyBound) {
		t.Fatalf("expected ErrAttachmentAlreadyBound, got %v", err)
	}

	// Five-zero-residue assertions (BLOCK-003): read rows, not logs.
	var issueCount, commentCount, taskCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'`, fx.projectID).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, fx.projectID).Scan(&commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, fx.projectID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if issueCount != 0 || commentCount != 0 || taskCount != 0 {
		t.Fatalf("rolled-back send left residue: issues=%d comments=%d tasks=%d", issueCount, commentCount, taskCount)
	}
	var sessionIssueID *string
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM project_chat_session WHERE id = $1`, fx.sessionID).Scan(&sessionIssueID); err != nil {
		t.Fatalf("read session issue_id: %v", err)
	}
	if sessionIssueID != nil {
		t.Fatalf("rolled-back send left session.issue_id = %v", *sessionIssueID)
	}
	// The attachment still points at the ordinary issue 鈥?the bind never ran.
	var stillBound *string
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM attachment WHERE id = $1`, attachmentID).Scan(&stillBound); err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if stillBound == nil || *stillBound != ordinaryIssueID {
		t.Fatalf("attachment link changed by the rolled-back send: %v", stillBound)
	}

	// The row lock was released with the rollback: a clean follow-up send
	// (without the bad attachment) proceeds immediately.
	if _, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "clean retry", nil); err != nil {
		t.Fatalf("retry after rollback must succeed: %v", err)
	}
}

// TestSendProjectChatMessageInvalidModelPreTx pins the pre-transaction 搂4.3
// contract: a resolved config outside the catalog rejects with
// ErrInvalidModelOrThinkingLevel BEFORE the transaction opens 鈥?zero residue,
// container never created.
func TestSendProjectChatMessageInvalidModelPreTx(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)
	// A catalog without the session's snapshot model (claude-opus-5).
	svc.ChatCatalog = &fakeChatCatalogPort{cacheResult: agent.Catalog{Models: []agent.Model{
		{ID: "some-other-model", Default: true},
	}}, cacheOK: true}

	before := sendKernelState(t, ctx, pool, fx)
	_, err := svc.SendProjectChatMessage(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "invalid config send", nil)
	if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("expected ErrInvalidModelOrThinkingLevel, got %v", err)
	}
	sendKernelStateUnchanged(t, ctx, pool, fx, before)
}

// TestEnsureProjectChatContainer pins POST /chat/container semantics: the
// bind is idempotent per session (repeat call returns the same issue), the
// container is stamped origin_id = session id, and a 搂4.3 validation failure
// leaves the session unbound.
func TestEnsureProjectChatContainer(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	issue, view, err := svc.EnsureProjectChatContainer(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "claude")
	if err != nil {
		t.Fatalf("bind container: %v", err)
	}
	if view.IssueID == nil || *view.IssueID != util.UUIDToString(issue.ID) {
		t.Fatalf("view issue_id = %v, want %s", view.IssueID, util.UUIDToString(issue.ID))
	}
	var originID string
	if err := pool.QueryRow(ctx, `SELECT origin_id::text FROM issue WHERE id = $1`, issue.ID).Scan(&originID); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if originID != fx.sessionID {
		t.Fatalf("container origin = %s, want session %s", originID, fx.sessionID)
	}

	// Idempotent repeat.
	again, viewAgain, err := svc.EnsureProjectChatContainer(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "claude")
	if err != nil {
		t.Fatalf("repeat bind: %v", err)
	}
	if again.ID != issue.ID {
		t.Fatalf("repeat bind created a second container: %s vs %s", again.ID, issue.ID)
	}
	if viewAgain.IssueID == nil || *viewAgain.IssueID != *view.IssueID {
		t.Fatalf("repeat bind view drifted: %+v vs %+v", viewAgain, view)
	}
}

// TestEnsureProjectChatContainerInvalidModelNoIssue is AC-23/AC-24 for the
// container endpoint: a 搂4.3 failure binds nothing.
func TestEnsureProjectChatContainerInvalidModelNoIssue(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)
	svc.ChatCatalog = &fakeChatCatalogPort{cacheResult: agent.Catalog{Models: []agent.Model{
		{ID: "some-other-model", Default: true},
	}}, cacheOK: true}

	_, _, err := svc.EnsureProjectChatContainer(ctx, u(fx.workspaceID), u(fx.projectID), u(fx.sessionID), fx.ownerID, "claude")
	if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("expected ErrInvalidModelOrThinkingLevel, got %v", err)
	}
	var issueCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'`, fx.projectID).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if issueCount != 0 {
		t.Fatalf("validation failure created %d container issues", issueCount)
	}
	var sessionIssueID *string
	if err := pool.QueryRow(ctx, `SELECT issue_id::text FROM project_chat_session WHERE id = $1`, fx.sessionID).Scan(&sessionIssueID); err != nil {
		t.Fatalf("read session issue_id: %v", err)
	}
	if sessionIssueID != nil {
		t.Fatalf("validation failure left session.issue_id = %v", *sessionIssueID)
	}
}

// TestUpdateProjectSettingsWithTeamAgentRebind pins the FR-7/AC-18 rebind
// close: a binding change closes the active session under the advisory; the
// next GET creates a NEW session row; re-patching the SAME binding does not
// close anything.
func TestUpdateProjectSettingsWithTeamAgentRebind(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	fx := newSendKernelFixture(t, ctx, pool)
	svc := newSendKernelService(pool)

	// Second agent + runtime for the new binding.
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'sk-runtime-b', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2) RETURNING id
	`, fx.workspaceID, util.UUIDToString(fx.ownerID)).Scan(&runtimeID); err != nil {
		t.Fatalf("create second runtime: %v", err)
	}
	var agentBID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, model, thinking_level)
		VALUES ($1, 'sk-agent-b', 'cloud', '{}'::jsonb, $2, 'workspace', 10, $3, '', '{}'::jsonb, '[]'::jsonb, 'claude-opus-5', 'high')
		RETURNING id
	`, fx.workspaceID, runtimeID, util.UUIDToString(fx.ownerID)).Scan(&agentBID); err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	patch, err := json.Marshal(map[string]any{"team_agent_id": agentBID})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}

	rebound, err := svc.UpdateProjectSettingsWithTeamAgentRebind(ctx, u(fx.workspaceID), u(fx.projectID), fx.agentID, u(agentBID), patch)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !rebound {
		t.Fatalf("binding change must report a rebound")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM project_chat_session WHERE id = $1`, fx.sessionID).Scan(&status); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if status != "closed" {
		t.Fatalf("old session status = %q, want closed", status)
	}

	// Next GET creates a new session stamped with the new binding.
	view, err := svc.EnsureProjectChatSession(ctx, u(fx.workspaceID), u(fx.projectID), fx.ownerID)
	if err != nil {
		t.Fatalf("ensure after rebind: %v", err)
	}
	if view.SessionID == "" || view.SessionID == fx.sessionID {
		t.Fatalf("expected a NEW session after rebind, got %q", view.SessionID)
	}
	if view.TeamAgentID != agentBID {
		t.Fatalf("new session stamped agent %s, want %s", view.TeamAgentID, agentBID)
	}

	// Same-value re-patch must not close the fresh session.
	samePatch, err := json.Marshal(map[string]any{"team_agent_id": agentBID})
	if err != nil {
		t.Fatalf("marshal same patch: %v", err)
	}
	rebound, err = svc.UpdateProjectSettingsWithTeamAgentRebind(ctx, u(fx.workspaceID), u(fx.projectID), u(agentBID), u(agentBID), samePatch)
	if err != nil {
		t.Fatalf("same-value rebind: %v", err)
	}
	if rebound {
		t.Fatalf("same-value patch must not close a session")
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM project_chat_session WHERE id = $1`, view.SessionID).Scan(&status); err != nil {
		t.Fatalf("read new session: %v", err)
	}
	if status != "active" {
		t.Fatalf("same-value patch closed the session: %q", status)
	}
}

// grantActivePresenter inserts an already-active presenter grant directly
// (bypassing request/approve) since the send-kernel tests only need the
// resulting state, not the transition path (covered by
// project_presenter_test.go).
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
// send doesn't trip idx_one_pending_task_per_issue_agent (migration 037: at
// most one queued/dispatched task per issue+agent) 鈥?the presenter-guard test
// intentionally reuses one session across many successful sends, which a real
// client would only do once the daemon has claimed the prior message.
func freeIssueAgentSlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("free issue/agent task slot: %v", err)
	}
}
