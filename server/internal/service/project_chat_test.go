package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newProjectChatTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// createProjectChatFixture creates a fresh workspace/user/project triple —
// isolated per test so concurrent EnsureProject*Issue calls below cannot
// collide with any other test's containers via the partial unique index.
func createProjectChatFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (workspaceID, userID, projectID string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("project-chat-ensure-%d@multica.ai", suffix)
	slug := fmt.Sprintf("project-chat-ensure-%d", suffix)

	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "Project Chat Ensure Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, "Project Chat Ensure Test", slug, "temporary EnsureProject*Issue test workspace", "PCE").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, workspaceID, "Project Chat Ensure Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return workspaceID, userID, projectID
}

// TestEnsureProjectDiscussionIssue_ConcurrentFirstOpenCollapsesToOneRow pins
// the concurrency contract documented on ensureContainerIssue: two callers
// racing to open a project's Discussion tab for the first time must end up
// with exactly one container issue, backstopped by the partial unique index
// issue_project_discussion_unique (migration 161) if both slip past the
// advisory lock. Mirrors the same guarantee CR-2026-006 already relies on for
// EnsureProjectChatIssue — this is the same code path (ensureContainerIssue)
// exercised through the Discussion-specific entry point.
func TestEnsureProjectDiscussionIssue_ConcurrentFirstOpenCollapsesToOneRow(t *testing.T) {
	ctx := context.Background()
	pool := newProjectChatTestPool(t)
	queries := db.New(pool)
	svc := NewIssueService(queries, pool, events.New(), nil, nil)

	workspaceID, userID, projectID := createProjectChatFixture(t, ctx, pool)
	wsUUID := util.MustParseUUID(workspaceID)
	projectUUID := util.MustParseUUID(projectID)
	callerUUID := util.MustParseUUID(userID)

	const workers = 8
	start := make(chan struct{})
	results := make(chan string, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			issue, err := svc.EnsureProjectDiscussionIssue(ctx, wsUUID, projectUUID, callerUUID)
			if err != nil {
				errs <- err
				return
			}
			results <- util.UUIDToString(issue.ID)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("EnsureProjectDiscussionIssue failed under concurrency: %v", err)
	}

	seen := map[string]bool{}
	for id := range results {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly one distinct container issue id across %d concurrent callers, got %d: %v", workers, len(seen), seen)
	}

	var count int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM issue WHERE project_id = $1 AND origin_type = 'project_discussion'
	`, projectID).Scan(&count); err != nil {
		t.Fatalf("count discussion containers: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one project_discussion row in the database, got %d", count)
	}
}

// TestEnsureProjectChatAndDiscussionIssue_ShareTheSamePlumbing pins the
// PSUG-002 concern raised in the tech-design review: extracting
// ensureContainerIssue must keep EnsureProjectChatIssue and
// EnsureProjectDiscussionIssue behaviorally equivalent aside from
// origin_type/title. Both must be independently idempotent for the same
// project, and must never collide with each other's container row.
func TestEnsureProjectChatAndDiscussionIssue_ShareTheSamePlumbing(t *testing.T) {
	ctx := context.Background()
	pool := newProjectChatTestPool(t)
	queries := db.New(pool)
	svc := NewIssueService(queries, pool, events.New(), nil, nil)

	workspaceID, userID, projectID := createProjectChatFixture(t, ctx, pool)
	wsUUID := util.MustParseUUID(workspaceID)
	projectUUID := util.MustParseUUID(projectID)
	callerUUID := util.MustParseUUID(userID)

	chatIssue, err := svc.EnsureProjectChatIssue(ctx, wsUUID, projectUUID, callerUUID)
	if err != nil {
		t.Fatalf("EnsureProjectChatIssue: %v", err)
	}
	discussionIssue, err := svc.EnsureProjectDiscussionIssue(ctx, wsUUID, projectUUID, callerUUID)
	if err != nil {
		t.Fatalf("EnsureProjectDiscussionIssue: %v", err)
	}

	if util.UUIDToString(chatIssue.ID) == util.UUIDToString(discussionIssue.ID) {
		t.Fatalf("chat and discussion containers must not be the same issue row")
	}
	if chatIssue.OriginType.String != "project_chat" {
		t.Fatalf("chat container origin_type = %q, want project_chat", chatIssue.OriginType.String)
	}
	if discussionIssue.OriginType.String != "project_discussion" {
		t.Fatalf("discussion container origin_type = %q, want project_discussion", discussionIssue.OriginType.String)
	}
	if chatIssue.Title != "Team Agent Chat" {
		t.Fatalf("chat container title = %q, want %q", chatIssue.Title, "Team Agent Chat")
	}
	if discussionIssue.Title != "Discussion" {
		t.Fatalf("discussion container title = %q, want %q", discussionIssue.Title, "Discussion")
	}

	// Idempotency: calling either again returns the same row, not a new one.
	chatAgain, err := svc.EnsureProjectChatIssue(ctx, wsUUID, projectUUID, callerUUID)
	if err != nil {
		t.Fatalf("EnsureProjectChatIssue (second call): %v", err)
	}
	if util.UUIDToString(chatAgain.ID) != util.UUIDToString(chatIssue.ID) {
		t.Fatalf("EnsureProjectChatIssue is not idempotent: got a different issue id on the second call")
	}
	discussionAgain, err := svc.EnsureProjectDiscussionIssue(ctx, wsUUID, projectUUID, callerUUID)
	if err != nil {
		t.Fatalf("EnsureProjectDiscussionIssue (second call): %v", err)
	}
	if util.UUIDToString(discussionAgain.ID) != util.UUIDToString(discussionIssue.ID) {
		t.Fatalf("EnsureProjectDiscussionIssue is not idempotent: got a different issue id on the second call")
	}
}

// TestProjectContainerOriginConstraintRejectsUnknownOrigin keeps the repaired
// issue_origin_type_check honest: both project container values must be
// accepted by the service path while an unrelated value remains rejected.
func TestProjectContainerOriginConstraintRejectsUnknownOrigin(t *testing.T) {
	ctx := context.Background()
	pool := newProjectChatTestPool(t)
	queries := db.New(pool)
	svc := NewIssueService(queries, pool, events.New(), nil, nil)

	workspaceID, userID, projectID := createProjectChatFixture(t, ctx, pool)
	wsUUID := util.MustParseUUID(workspaceID)
	projectUUID := util.MustParseUUID(projectID)
	callerUUID := util.MustParseUUID(userID)
	if _, err := svc.EnsureProjectChatIssue(ctx, wsUUID, projectUUID, callerUUID); err != nil {
		t.Fatalf("project_chat container: %v", err)
	}
	if _, err := svc.EnsureProjectDiscussionIssue(ctx, wsUUID, projectUUID, callerUUID); err != nil {
		t.Fatalf("project_discussion container: %v", err)
	}

	var issueID string
	err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position, origin_type)
		VALUES ($1, $2, 'invalid origin regression', 'todo', 'medium', $3, 'member', $4, 0, 'not_a_real_origin')
		RETURNING id`, workspaceID, projectID, userID, time.Now().UnixNano()%1000000000).Scan(&issueID)
	if err == nil {
		t.Fatalf("invalid origin_type insert unexpectedly succeeded with issue %s", issueID)
	}
}
