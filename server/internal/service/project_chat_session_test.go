package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newProjectChatSessionTestPool(t *testing.T) *pgxpool.Pool {
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

// seedProjectChatSessionFixture creates an isolated workspace/user/agent/project
// triple with the Team Agent bound in project.settings.
func seedProjectChatSessionFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) (workspaceID, userID, agentID, projectID string) {
	t.Helper()
	suffix := time.Now().UnixNano()

	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Session Kernel Test", fmt.Sprintf("pcs-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		fmt.Sprintf("PCS %d", suffix), fmt.Sprintf("pcs-%d", suffix), "temporary session kernel workspace", "PCS").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		workspaceID, userID, role); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'pcs-runtime', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2)
		RETURNING id`, workspaceID, userID).Scan(&runtimeID); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, model, thinking_level)
		VALUES ($1, 'pcs-agent', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb, 'claude-opus-5', 'high')
		RETURNING id`, workspaceID, runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings)
		VALUES ($1, 'PCS Project', jsonb_build_object('team_agent_id', $2::text))
		RETURNING id`, workspaceID, agentID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return workspaceID, userID, agentID, projectID
}

func newSessionKernelService(pool *pgxpool.Pool) *IssueService {
	return &IssueService{
		Queries:   db.New(pool),
		TxStarter: pool,
		ChatCatalog: &fakeChatCatalogPort{cacheResult: agent.Catalog{Models: []agent.Model{
			{ID: "claude-opus-5", Default: true, Thinking: &agent.ModelThinking{SupportedLevels: []agent.ThinkingLevel{{Value: "high"}, {Value: "low"}}}},
		}}, cacheOK: true},
	}
}

func u(s string) pgtype.UUID { return util.MustParseUUID(s) }

// TestApplyChatConfigFieldPatch pins the three-state fold (SDD FR-6): absent
// keeps the current value, clear writes SQL NULL, set writes the value; an
// empty string never lands in an override column.
func TestApplyChatConfigFieldPatch(t *testing.T) {
	t.Parallel()
	current := pgtype.Text{String: "old", Valid: true}

	if got := applyChatConfigFieldPatch(current, ChatConfigFieldPatch{}); got != current {
		t.Fatalf("absent patch changed value: %+v", got)
	}
	if got := applyChatConfigFieldPatch(current, ChatConfigFieldPatch{Present: true, Clear: true}); got.Valid {
		t.Fatalf("clear patch did not null the value: %+v", got)
	}
	if got := applyChatConfigFieldPatch(current, ChatConfigFieldPatch{Present: true, Value: "new"}); got.String != "new" || !got.Valid {
		t.Fatalf("set patch: %+v", got)
	}
	if got := applyChatConfigFieldPatch(pgtype.Text{}, ChatConfigFieldPatch{Present: true, Value: "new"}); got.String != "new" {
		t.Fatalf("set over null current: %+v", got)
	}
}

// TestSnapshotAgentDefaults pins the base_* snapshot conversion: NULL agent
// values become the empty follow-runtime sentinel.
func TestSnapshotAgentDefaults(t *testing.T) {
	t.Parallel()
	agent := db.Agent{Model: pgtype.Text{}, ThinkingLevel: pgtype.Text{}}
	model, thinking := snapshotAgentDefaults(agent)
	if !model.Valid || model.String != "" || !thinking.Valid || thinking.String != "" {
		t.Fatalf("NULL agent defaults must snapshot as empty sentinels: %+v %+v", model, thinking)
	}
	agent = db.Agent{Model: pgtype.Text{String: "claude-opus-5", Valid: true}, ThinkingLevel: pgtype.Text{String: "high", Valid: true}}
	model, thinking = snapshotAgentDefaults(agent)
	if model.String != "claude-opus-5" || thinking.String != "high" {
		t.Fatalf("agent defaults not preserved: %+v %+v", model, thinking)
	}
}

// TestEnsureProjectChatSessionGreenFieldNoIssue is AC-11: the first GET creates
// the session and NO container issue.
func TestEnsureProjectChatSessionGreenFieldNoIssue(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID, _, projectID := seedProjectChatSessionFixture(t, ctx, pool, "owner")
	svc := newSessionKernelService(pool)

	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(userID))
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if view.SessionID == "" || view.TeamAgentID == "" {
		t.Fatalf("view missing session/team agent: %+v", view)
	}
	if view.IssueID != nil {
		t.Fatalf("green-field GET must not bind a container (AC-11): %+v", view)
	}
	if view.ModelSource != ChatConfigSourceSessionDefault || view.Model != "claude-opus-5" {
		t.Fatalf("resolved model = %q (%s), want claude-opus-5 (session_default)", view.Model, view.ModelSource)
	}

	var issueCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND origin_type = 'project_chat'`, workspaceID).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if issueCount != 0 {
		t.Fatalf("GET created %d project_chat issues, want 0 (AC-11)", issueCount)
	}

	// Idempotent: the second GET returns the same session.
	again, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(userID))
	if err != nil || again.SessionID != view.SessionID {
		t.Fatalf("second ensure: %v, %+v vs %+v", err, again, view)
	}
}

// TestEnsureProjectChatSessionUnconfigured: no Team Agent bound -> empty view,
// no session row.
func TestEnsureProjectChatSessionUnconfigured(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var workspaceID, userID, projectID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('u', $1) RETURNING id`, fmt.Sprintf("u-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ($1, $2, 'W') RETURNING id`,
		fmt.Sprintf("W%d", suffix), fmt.Sprintf("w-%d", suffix)).Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO project (workspace_id, title) VALUES ($1, 'No Agent') RETURNING id`, workspaceID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	svc := newSessionKernelService(pool)

	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(userID))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if view.TeamAgentID != "" || view.SessionID != "" {
		t.Fatalf("unconfigured project must return an empty view: %+v", view)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_chat_session WHERE workspace_id = $1`, workspaceID).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("unconfigured project must not create a session (count=%d)", count)
	}
}

// TestUpdateProjectChatSessionConfigThreeState: set -> view reflects override;
// clear -> back to session_default; omit -> unchanged; empty string never
// lands in the override column.
func TestUpdateProjectChatSessionConfigThreeState(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID, _, projectID := seedProjectChatSessionFixture(t, ctx, pool, "owner")
	svc := newSessionKernelService(pool)

	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(userID))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sessionID := u(view.SessionID)

	patched, err := svc.UpdateProjectChatSessionConfig(ctx, u(workspaceID), u(projectID), sessionID, u(userID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"},
		ChatConfigFieldPatch{Present: true, Value: "low"})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.ModelSource != ChatConfigSourceOverride || patched.Model != "claude-opus-5" {
		t.Fatalf("patched model = %q (%s)", patched.Model, patched.ModelSource)
	}
	if patched.ThinkingLevelSource != ChatConfigSourceOverride || patched.ThinkingLevel != "low" {
		t.Fatalf("patched thinking = %q (%s)", patched.ThinkingLevel, patched.ThinkingLevelSource)
	}
	// The override is written to the column.
	var override string
	if err := pool.QueryRow(ctx, `SELECT model_override FROM project_chat_session WHERE id = $1`, view.SessionID).Scan(&override); err != nil {
		t.Fatalf("read override: %v", err)
	}
	if override != "claude-opus-5" {
		t.Fatalf("override column = %q, want claude-opus-5", override)
	}

	// Clear -> session_default again.
	cleared, err := svc.UpdateProjectChatSessionConfig(ctx, u(workspaceID), u(projectID), sessionID, u(userID), "claude",
		ChatConfigFieldPatch{Present: true, Clear: true},
		ChatConfigFieldPatch{Present: true, Clear: true})
	if err != nil {
		t.Fatalf("clear patch: %v", err)
	}
	if cleared.ModelSource != ChatConfigSourceSessionDefault || cleared.Model != "claude-opus-5" {
		t.Fatalf("cleared model = %q (%s)", cleared.Model, cleared.ModelSource)
	}
	if err := pool.QueryRow(ctx, `SELECT model_override IS NULL FROM project_chat_session WHERE id = $1`, view.SessionID).Scan(&override); err != nil || override != "true" {
		t.Fatalf("override not cleared: %v %q", err, override)
	}
}

// TestUpdateProjectChatSessionConfigForbiddenNonOwner is AC-6: a plain member
// gets ErrForbiddenChatConfig.
func TestUpdateProjectChatSessionConfigForbiddenNonOwner(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, _, _, projectID := seedProjectChatSessionFixture(t, ctx, pool, "owner")
	svc := newSessionKernelService(pool)

	// Second user, plain member role.
	suffix := time.Now().UnixNano()
	var memberUser string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('m', $1) RETURNING id`, fmt.Sprintf("m-%d@multica.ai", suffix)).Scan(&memberUser); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, workspaceID, memberUser); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE project SET settings = jsonb_build_object('team_agent_id', (SELECT id::text FROM agent WHERE workspace_id = $1 LIMIT 1)) WHERE id = $2`, workspaceID, projectID); err != nil {
		t.Fatalf("bind team agent: %v", err)
	}

	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(memberUser))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	_, err = svc.UpdateProjectChatSessionConfig(ctx, u(workspaceID), u(projectID), u(view.SessionID), u(memberUser), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{})
	if !errors.Is(err, ErrForbiddenChatConfig) {
		t.Fatalf("non-owner patch: got %v, want ErrForbiddenChatConfig", err)
	}
}

// TestUpdateProjectChatSessionConfigInvalidModel: a non-catalog model is
// rejected with ErrInvalidModelOrThinkingLevel and no override is written.
func TestUpdateProjectChatSessionConfigInvalidModel(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID, _, projectID := seedProjectChatSessionFixture(t, ctx, pool, "owner")
	svc := newSessionKernelService(pool)

	view, err := svc.EnsureProjectChatSession(ctx, u(workspaceID), u(projectID), u(userID))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	_, err = svc.UpdateProjectChatSessionConfig(ctx, u(workspaceID), u(projectID), u(view.SessionID), u(userID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-madeup-9"}, ChatConfigFieldPatch{})
	if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("invalid model: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT model_override IS NULL FROM project_chat_session WHERE id = $1`, view.SessionID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("failed patch must not write the override: %v %v", err, isNull)
	}
}
