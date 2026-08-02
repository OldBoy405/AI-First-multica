package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestClaimTaskCrossAgentProjectSingleWriter is CR-2026-010's core concurrency
// proof: two different agents each have one queued task against the same
// project. ClaimAgentTask's NOT EXISTS check alone cannot prevent both from
// claiming (READ COMMITTED lets both transactions pass it before either
// commits) — the advisory-lock recheck in ClaimTask must be what closes the
// race. Requires migrations 161-163 applied (project_id column + index).
func TestClaimTaskCrossAgentProjectSingleWriter(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	agentAID, agentBID, projectID := createCrossAgentProjectFixture(t, ctx, pool)
	createProjectClaimSleepTrigger(t, ctx, pool, projectID)
	svc := NewTaskService(queries, pool, nil, events.New())

	agentUUIDs := []string{agentAID, agentBID}
	claimed := make(chan string, len(agentUUIDs))
	errs := make(chan error, len(agentUUIDs))
	start := make(chan struct{})

	var wg sync.WaitGroup
	for _, agentID := range agentUUIDs {
		wg.Add(1)
		go func(agentID string) {
			defer wg.Done()
			<-start
			task, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
			if err != nil {
				errs <- err
				return
			}
			if task != nil {
				claimed <- util.UUIDToString(task.ID)
			}
		}(agentID)
	}

	close(start)
	wg.Wait()
	close(claimed)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("claim task: %v", err)
		}
	}

	var claimedIDs []string
	for id := range claimed {
		claimedIDs = append(claimedIDs, id)
	}
	// Either 0 (both raced, the loser requeued and neither has been retried
	// yet within this single ClaimTask call) or 1 claimed task is a correct
	// outcome; 2 is the single-writer guarantee failing.
	if len(claimedIDs) > 1 {
		t.Fatalf("expected at most 1 claimed task across 2 agents sharing a project, got %d (%v)", len(claimedIDs), claimedIDs)
	}

	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE project_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')
	`, projectID).Scan(&active); err != nil {
		t.Fatalf("count active project tasks: %v", err)
	}
	if active > 1 {
		t.Fatalf("single-writer violated: %d active tasks for project %s (want at most 1)", active, projectID)
	}

	// The loser (if any) must have been requeued, not stuck dispatched with
	// no runner — the whole point of the requeue path.
	var stuckQueuedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE project_id = $1 AND status = 'queued'
	`, projectID).Scan(&stuckQueuedCount); err != nil {
		t.Fatalf("count queued project tasks: %v", err)
	}
	if len(claimedIDs)+stuckQueuedCount != 2 {
		t.Fatalf("expected claimed+queued to account for both fixture tasks, got claimed=%d queued=%d", len(claimedIDs), stuckQueuedCount)
	}
}

// TestClaimTaskChatSessionParallelWithProjectTask verifies the chat_session
// serialization branch (Private Ask / 1:1 chat) is untouched by the project
// branch: a chat task and a project task on the SAME agent must both be
// claimable (they don't share a serialization key), proving DD-2's "chat
// stays parallel with Team Agent" holds at the claim layer.
func TestClaimTaskChatSessionParallelWithProjectTask(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	agentID := createChatAndProjectTaskFixture(t, ctx, pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	var claimedIDs []string
	for i := 0; i < 2; i++ {
		task, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
		if err != nil {
			t.Fatalf("claim task %d: %v", i, err)
		}
		if task == nil {
			break
		}
		claimedIDs = append(claimedIDs, util.UUIDToString(task.ID))
	}

	if len(claimedIDs) != 2 {
		t.Fatalf("expected both the chat task and the project task claimable in sequence (agent has capacity for 2), got %d claimed", len(claimedIDs))
	}
}

func createCrossAgentProjectFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (agentAID, agentBID, projectID string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("claim-project-race-%d@multica.ai", suffix)
	slug := fmt.Sprintf("claim-project-race-%d", suffix)

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Claim Project Race Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Claim Project Race Test", slug, "temporary CR-2026-010 claim project race test workspace", "CPR").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, workspaceID, "Claim Project Race Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var runtimeAID, runtimeBID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, NULL, $2, 'cloud', 'claim_project_race_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Claim Project Race Runtime A", userID).Scan(&runtimeAID); err != nil {
		t.Fatalf("create runtime A: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, NULL, $2, 'cloud', 'claim_project_race_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Claim Project Race Runtime B", userID).Scan(&runtimeBID); err != nil {
		t.Fatalf("create runtime B: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
		RETURNING id
	`, workspaceID, "Claim Project Race Agent A", runtimeAID, userID).Scan(&agentAID); err != nil {
		t.Fatalf("create agent A: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
		RETURNING id
	`, workspaceID, "Claim Project Race Agent B", runtimeBID, userID).Scan(&agentBID); err != nil {
		t.Fatalf("create agent B: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE project_id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id IN ($1, $2)`, agentAID, agentBID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id IN ($1, $2)`, runtimeAID, runtimeBID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	agents := []struct {
		id        string
		runtimeID string
	}{
		{agentAID, runtimeAID},
		{agentBID, runtimeBID},
	}
	for i, a := range agents {
		var issueID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position)
			VALUES ($1, $2, $3, 'in_progress', 'none', $4, 'member', $5, $6)
			RETURNING id
		`, workspaceID, projectID, fmt.Sprintf("claim project race issue %d", i+1), userID, 910000+i, i).Scan(&issueID); err != nil {
			t.Fatalf("create issue %d: %v", i+1, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, issue_id, project_id, status, priority, context, runtime_id)
			VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb, $4)
		`, a.id, issueID, projectID, a.runtimeID); err != nil {
			t.Fatalf("create task %d: %v", i+1, err)
		}
	}

	return agentAID, agentBID, projectID
}

func createChatAndProjectTaskFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (agentID string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("claim-chat-parallel-%d@multica.ai", suffix)
	slug := fmt.Sprintf("claim-chat-parallel-%d", suffix)

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Claim Chat Parallel Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Claim Chat Parallel Test", slug, "temporary CR-2026-010 chat/project parallel test workspace", "CCP").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, workspaceID, "Claim Chat Parallel Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
		VALUES ($1, NULL, $2, 'cloud', 'claim_chat_parallel_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Claim Chat Parallel Runtime", userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 2, $4)
		RETURNING id
	`, workspaceID, "Claim Chat Parallel Agent", runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	var chatSessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id)
		VALUES ($1, $2, $3)
		RETURNING id
	`, workspaceID, agentID, userID).Scan(&chatSessionID); err != nil {
		t.Fatalf("create chat session: %v", err)
	}

	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, $2, $3, 'in_progress', 'none', $4, 'member', $5, $6)
		RETURNING id
	`, workspaceID, projectID, "claim chat parallel issue", userID, 920000, 0).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM chat_session WHERE id = $1`, chatSessionID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	// Project-scoped issue task (project_id set — new branch).
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, project_id, status, priority, context, runtime_id)
		VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb, $4)
	`, agentID, issueID, projectID, runtimeID); err != nil {
		t.Fatalf("create project task: %v", err)
	}
	// Chat task (chat_session_id set, project_id NULL — untouched branch).
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, chat_session_id, status, priority, context, runtime_id)
		VALUES ($1, $2, 'queued', 0, '{}'::jsonb, $3)
	`, agentID, chatSessionID, runtimeID); err != nil {
		t.Fatalf("create chat task: %v", err)
	}

	return agentID
}

// createProjectClaimSleepTrigger widens the cross-agent claim race window
// (see createSleepTrigger's sibling precedent in task_claim_race_test.go)
// so both agents' transactions reliably overlap instead of depending on
// goroutine scheduling luck. Scoped by project_id rather than agent_id since
// the race here is across two different agents.
func createProjectClaimSleepTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	triggerName := fmt.Sprintf("claim_project_race_sleep_%d", suffix)
	functionName := triggerName + "_fn"

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			PERFORM pg_sleep(0.2);
			RETURN NEW;
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create sleep trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON agent_task_queue
		FOR EACH ROW
		WHEN (OLD.status = 'queued' AND NEW.status = 'dispatched' AND NEW.project_id = %s::uuid)
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(projectID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create sleep trigger: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_task_queue", quoteIdent(triggerName)))
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(functionName)))
	})
}
