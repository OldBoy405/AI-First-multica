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

// TestClaimTaskCrossAgentProjectSingleWriterStress is CR-2026-010 TASK-07's
// SDD §6.3 group-3 regression: a larger-scale version of
// TestClaimTaskCrossAgentProjectSingleWriter (task_claim_project_race_test.go)
// — N distinct agents, each holding one queued task against the same
// project, all firing ClaimTask concurrently. At most one may win, proven
// against a real DB (advisory lock + FOR UPDATE SKIP LOCKED interaction
// cannot be verified against a mock).
func TestClaimTaskCrossAgentProjectSingleWriterStress(t *testing.T) {
	const agentCount = 12

	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	agentIDs, projectID := createNAgentProjectFixture(t, ctx, pool, agentCount)
	createProjectClaimSleepTrigger(t, ctx, pool, projectID)
	svc := NewTaskService(queries, pool, nil, events.New())

	claimed := make(chan string, agentCount)
	errs := make(chan error, agentCount)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for _, agentID := range agentIDs {
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

	t0 := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(t0)
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
	if len(claimedIDs) > 1 {
		t.Fatalf("single-writer violated: %d of %d concurrent agents claimed a task (want at most 1): %v",
			len(claimedIDs), agentCount, claimedIDs)
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

	var queuedAfter int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE project_id = $1 AND status = 'queued'
	`, projectID).Scan(&queuedAfter); err != nil {
		t.Fatalf("count queued project tasks: %v", err)
	}
	if len(claimedIDs)+queuedAfter != agentCount {
		t.Fatalf("expected claimed+requeued to account for all %d fixture tasks, got claimed=%d queued=%d",
			agentCount, len(claimedIDs), queuedAfter)
	}

	// §6.3 evidence: concurrency, task count, wall-clock, and the SQL
	// assertion that at most one task ended up active.
	t.Logf("stress params: concurrency=%d tasks=%d elapsed=%s claimed=%d active_after=%d requeued=%d",
		agentCount, agentCount, elapsed, len(claimedIDs), active, queuedAfter)
}

// createNAgentProjectFixture is the N-agent generalization of
// createCrossAgentProjectFixture (task_claim_project_race_test.go): one
// project, N agents each with its own runtime and one queued issue task
// against that project.
func createNAgentProjectFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) (agentIDs []string, projectID string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("claim-project-stress-%d@multica.ai", suffix)
	slug := fmt.Sprintf("claim-project-stress-%d", suffix)

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Claim Project Stress Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Claim Project Stress Test", slug, "temporary CR-2026-010 TASK-07 stress test workspace", "CPS").Scan(&workspaceID); err != nil {
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
	`, workspaceID, "Claim Project Stress Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	runtimeIDs := make([]string, n)
	agentIDs = make([]string, n)
	for i := 0; i < n; i++ {
		var runtimeID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id)
			VALUES ($1, NULL, $2, 'cloud', 'claim_project_stress_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
			RETURNING id
		`, workspaceID, fmt.Sprintf("Claim Project Stress Runtime %d", i), userID).Scan(&runtimeID); err != nil {
			t.Fatalf("create runtime %d: %v", i, err)
		}
		runtimeIDs[i] = runtimeID

		var agentID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
			VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
			RETURNING id
		`, workspaceID, fmt.Sprintf("Claim Project Stress Agent %d", i), runtimeID, userID).Scan(&agentID); err != nil {
			t.Fatalf("create agent %d: %v", i, err)
		}
		agentIDs[i] = agentID
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE project_id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		for _, id := range agentIDs {
			pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id = $1`, id)
		}
		for _, id := range runtimeIDs {
			pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, id)
		}
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	for i, agentID := range agentIDs {
		var issueID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position)
			VALUES ($1, $2, $3, 'in_progress', 'none', $4, 'member', $5, $6)
			RETURNING id
		`, workspaceID, projectID, fmt.Sprintf("claim project stress issue %d", i+1), userID, 950000+i, i).Scan(&issueID); err != nil {
			t.Fatalf("create issue %d: %v", i+1, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, issue_id, project_id, status, priority, context, runtime_id)
			VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb, $4)
		`, agentID, issueID, projectID, runtimeIDs[i]); err != nil {
			t.Fatalf("create task %d: %v", i+1, err)
		}
	}

	return agentIDs, projectID
}
