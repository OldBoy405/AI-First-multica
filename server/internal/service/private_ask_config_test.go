package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// latchCatalogPort is a ChatCatalogPort whose LiveLoad blocks on a channel
// after an always-miss CacheLoad. It lets the BLOCK-008/010 fixtures park a
// send transaction at the catalog round trip — after the backfill, holding
// only the session row lock — while the test connection performs the
// production unbind sequence or a competing PATCH/clear.
type latchCatalogPort struct {
	liveResult agent.Catalog
	liveErr    error
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
	mu         sync.Mutex
	liveCalls  int
}

func (f *latchCatalogPort) CacheLoad(context.Context, string) (agent.Catalog, bool, error) {
	return agent.Catalog{}, false, nil // always a cache miss
}

func (f *latchCatalogPort) LiveLoad(ctx context.Context, _ string) (agent.Catalog, error) {
	f.mu.Lock()
	f.liveCalls++
	f.mu.Unlock()
	f.once.Do(func() { close(f.entered) })
	select {
	case <-ctx.Done():
		return agent.Catalog{}, ctx.Err()
	case <-f.release:
	}
	return f.liveResult, f.liveErr
}

func (f *latchCatalogPort) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveCalls
}

// validPrivateAskCatalog is the catalog all Private Ask fixtures validate
// against: claude-opus-5 with thinking high/low.
func validPrivateAskCatalog() agent.Catalog {
	return agent.Catalog{Models: []agent.Model{
		{ID: "claude-opus-5", Default: true, Thinking: &agent.ModelThinking{SupportedLevels: []agent.ThinkingLevel{{Value: "high"}, {Value: "low"}}}},
	}}
}

// privateAskWorld seeds an isolated workspace with a creator (owner), a
// second member, a Team Agent (model claude-opus-5 / thinking high, cloud
// runtime online, provider claude) and a project bound to it.
type privateAskWorld struct {
	workspaceID string
	creatorID   string
	otherID     string
	agentID     string
	projectID   string
	runtimeID   string
}

func seedPrivateAskWorld(t *testing.T, pool *pgxpool.Pool) privateAskWorld {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	w := privateAskWorld{}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Private Ask Creator", fmt.Sprintf("pa-creator-%d@multica.ai", suffix)).Scan(&w.creatorID); err != nil {
		t.Fatalf("create creator: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Private Ask Other", fmt.Sprintf("pa-other-%d@multica.ai", suffix)).Scan(&w.otherID); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, 'PAK') RETURNING id`,
		fmt.Sprintf("PA %d", suffix), fmt.Sprintf("pa-%d", suffix), "temporary private ask workspace").Scan(&w.workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	for _, m := range []struct{ id, role string }{{w.creatorID, "owner"}, {w.otherID, "member"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			w.workspaceID, m.id, m.role); err != nil {
			t.Fatalf("seed member: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'pa-runtime', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2) RETURNING id`,
		w.workspaceID, w.creatorID).Scan(&w.runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, model, thinking_level)
		VALUES ($1, 'pa-agent', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb, 'claude-opus-5', 'high')
		RETURNING id`, w.workspaceID, w.runtimeID, w.creatorID).Scan(&w.agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings)
		VALUES ($1, 'Private Ask Project', jsonb_build_object('team_agent_id', $2::text)) RETURNING id`,
		w.workspaceID, w.agentID).Scan(&w.projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		pool.Exec(cctx, `DELETE FROM agent_task_queue WHERE chat_session_id IN (SELECT id FROM chat_session WHERE workspace_id = $1)`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE workspace_id = $1)`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM chat_session WHERE workspace_id = $1`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM project WHERE id = $1`, w.projectID)
		pool.Exec(cctx, `DELETE FROM agent WHERE workspace_id = $1`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM agent_runtime WHERE workspace_id = $1`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM member WHERE workspace_id = $1`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM workspace WHERE id = $1`, w.workspaceID)
		pool.Exec(cctx, `DELETE FROM "user" WHERE id = ANY($1::uuid[])`, []string{w.creatorID, w.otherID})
	})
	return w
}

// seedPrivateAskSession inserts a chat_session row for the Private Ask tests.
// projectID zero = ordinary 1:1 chat (project_id NULL).
func seedPrivateAskSession(t *testing.T, pool *pgxpool.Pool, w privateAskWorld, creatorID string, projectID pgtype.UUID, baseModel, baseThinking, modelOverride, thinkingOverride pgtype.Text) string {
	t.Helper()
	ctx := context.Background()
	id := dbid.NewV7()
	idStr := util.UUIDToString(id)
	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_session (id, workspace_id, agent_id, creator_id, title, status, project_id,
			base_model, base_thinking_level, model_override, thinking_level_override, explicitly_created_at)
		VALUES ($1, $2, $3, $4, 'Private Ask', 'active', $5, $6, $7, $8, $9, now())
	`, idStr, w.workspaceID, w.agentID, creatorID, projectID, baseModel, baseThinking, modelOverride, thinkingOverride); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, idStr)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, idStr)
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, idStr)
	})
	return idStr
}

func uuids(s string) pgtype.UUID { return util.MustParseUUID(s) }

// newPrivateAskServices builds the IssueService + TaskService pair with a
// scripted catalog port.
func newPrivateAskServices(pool *pgxpool.Pool, port ChatCatalogPort) (*IssueService, *TaskService) {
	queries := db.New(pool)
	taskSvc := NewTaskService(queries, pool, nil, events.New())
	taskSvc.ChatCatalog = port
	issueSvc := &IssueService{
		Queries:     queries,
		TxStarter:   pool,
		Bus:         events.New(),
		TaskService: taskSvc,
		ChatCatalog: port,
	}
	return issueSvc, taskSvc
}

// readTaskChatConfig parses task.context and returns the chat_config map.
func readTaskChatConfig(t *testing.T, pool *pgxpool.Pool, taskID string) map[string]any {
	t.Helper()
	var raw string
	if err := pool.QueryRow(context.Background(), `SELECT context::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&raw); err != nil {
		t.Fatalf("read task context: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("task context is not JSON: %v (%q)", err, raw)
	}
	cc, _ := parsed["chat_config"].(map[string]any)
	return cc
}

// assertZeroResidue pins the transaction-rollback residue contract: no
// message, no task, base_* still NULL, updated_at untouched.
func assertZeroResidue(t *testing.T, pool *pgxpool.Pool, sessionID string, updatedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	var msgCount, taskCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if msgCount != 0 || taskCount != 0 {
		t.Fatalf("zero-residue violated: messages=%d tasks=%d", msgCount, taskCount)
	}
	var baseNull bool
	var updatedAtDB time.Time
	if err := pool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL, updated_at FROM chat_session WHERE id = $1`, sessionID).Scan(&baseNull, &updatedAtDB); err != nil {
		t.Fatalf("read session residue: %v", err)
	}
	if !baseNull {
		t.Fatal("failed send left a backfill behind (no partial backfill allowed)")
	}
	if updatedAtDB.After(updatedAt) {
		t.Fatalf("failed send advanced updated_at (TouchChatSession not rolled back): %v -> %v", updatedAt, updatedAtDB)
	}
}

// ---------------------------------------------------------------------------
// PATCH: gates, three-state, backfill, rollback
// ---------------------------------------------------------------------------

// TestPatchChatSessionConfigGatesAndThreeState covers AC-25, the three-state
// fold, the first-PATCH backfill, and the session_id-consistent row reuse.
func TestPatchChatSessionConfigGatesAndThreeState(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	svc, _ := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true})

	legacy := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	ordinary := seedPrivateAskSession(t, pool, w, w.creatorID, pgtype.UUID{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})

	// AC-25: non-creator -> forbidden; ordinary 1:1 -> 404; wrong workspace -> 404.
	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.otherID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{}); !errors.Is(err, ErrForbiddenChatConfig) {
		t.Fatalf("non-creator patch: got %v, want ErrForbiddenChatConfig", err)
	}
	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(ordinary), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{}); !errors.Is(err, ErrChatSessionNotFound) {
		t.Fatalf("ordinary session patch: got %v, want ErrChatSessionNotFound", err)
	}
	if _, err := svc.PatchChatSessionConfig(ctx, dbid.NewV7(), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{}, ChatConfigFieldPatch{}); !errors.Is(err, ErrChatSessionNotFound) {
		t.Fatalf("wrong workspace patch: got %v, want ErrChatSessionNotFound", err)
	}

	// Creator set: override wins, base_* backfilled with the agent defaults.
	view, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{})
	if err != nil {
		t.Fatalf("creator patch: %v", err)
	}
	if view.ModelSource != ChatConfigSourceOverride || view.Model != "claude-opus-5" {
		t.Fatalf("patched view = %q (%s)", view.Model, view.ModelSource)
	}
	if view.ThinkingLevelSource != ChatConfigSourceSessionDefault || view.ThinkingLevel != "high" {
		t.Fatalf("thinking view = %q (%s), want high (session_default)", view.ThinkingLevel, view.ThinkingLevelSource)
	}
	if util.UUIDToString(view.Session.ID) != legacy {
		t.Fatalf("view row id mismatch")
	}
	var baseModel, baseThinking, overrideModel string
	if err := pool.QueryRow(ctx, `SELECT base_model, base_thinking_level, model_override FROM chat_session WHERE id = $1`, legacy).Scan(&baseModel, &baseThinking, &overrideModel); err != nil {
		t.Fatalf("read patched row: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" || overrideModel != "claude-opus-5" {
		t.Fatalf("row = base(%q,%q) override(%q)", baseModel, baseThinking, overrideModel)
	}

	// Omit: nothing changes.
	view, err = svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{}, ChatConfigFieldPatch{})
	if err != nil {
		t.Fatalf("omit patch: %v", err)
	}
	if view.ModelSource != ChatConfigSourceOverride || view.Model != "claude-opus-5" {
		t.Fatalf("omit changed the override: %q (%s)", view.Model, view.ModelSource)
	}

	// Clear: back to session_default (the backfilled base).
	view, err = svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Clear: true}, ChatConfigFieldPatch{})
	if err != nil {
		t.Fatalf("clear patch: %v", err)
	}
	if view.ModelSource != ChatConfigSourceSessionDefault || view.Model != "claude-opus-5" {
		t.Fatalf("cleared view = %q (%s)", view.Model, view.ModelSource)
	}
	var overrideNull bool
	if err := pool.QueryRow(ctx, `SELECT model_override IS NULL FROM chat_session WHERE id = $1`, legacy).Scan(&overrideNull); err != nil || !overrideNull {
		t.Fatalf("clear must null the override column: %v %v", err, overrideNull)
	}
}

// TestPatchChatSessionConfigValidationFailureRollsBack: a failed §4.3
// validation rolls the whole transaction back — the backfill included — and
// the released row lock lets the next PATCH proceed immediately (BLOCK-008 ③).
func TestPatchChatSessionConfigValidationFailureRollsBack(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	svc, _ := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true})

	legacy := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})

	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-madeup-9"}, ChatConfigFieldPatch{}); !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("invalid model patch: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	var baseNull, overrideNull bool
	if err := pool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL, model_override IS NULL FROM chat_session WHERE id = $1`, legacy).Scan(&baseNull, &overrideNull); err != nil {
		t.Fatalf("read rolled-back row: %v", err)
	}
	if !baseNull || !overrideNull {
		t.Fatalf("failed patch must roll back backfill and override (baseNull=%v overrideNull=%v)", baseNull, overrideNull)
	}

	// The row lock was released with the rollback: the next PATCH succeeds.
	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{}); err != nil {
		t.Fatalf("patch after rollback must proceed (lock released): %v", err)
	}
}

// TestPatchChatSessionConfigCatalogLiveFailureRollsBack: a cache miss whose
// LiveLoad fails rejects the patch and rolls the backfill back too.
func TestPatchChatSessionConfigCatalogLiveFailureRollsBack(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	svc, _ := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheOK: false, liveErr: errors.New("injected live failure")})

	legacy := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})

	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(legacy), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{}); !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("live-failure patch: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	var baseNull bool
	if err := pool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL FROM chat_session WHERE id = $1`, legacy).Scan(&baseNull); err != nil {
		t.Fatalf("read rolled-back row: %v", err)
	}
	if !baseNull {
		t.Fatal("catalog failure must roll the backfill back too")
	}
}

// ---------------------------------------------------------------------------
// Send: snapshot seam, zero residue, ordinary-chat regression
// ---------------------------------------------------------------------------

// TestPrivateAskSendSnapshotsChatConfig is BLOCK-005 + AC-19: the first send
// backfills base_* and snapshots the resolved output into task.context;
// a later agent-default change never affects the session's effective values;
// an override wins over the base.
func TestPrivateAskSendSnapshotsChatConfig(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	_, taskSvc := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	sent, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "first private ask turn", nil, "member", uuids(w.creatorID))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	cc := readTaskChatConfig(t, pool, util.UUIDToString(sent.Task.ID))
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("task snapshot = %+v, want the agent defaults", cc)
	}
	var baseModel, baseThinking string
	if err := pool.QueryRow(ctx, `SELECT base_model, base_thinking_level FROM chat_session WHERE id = $1`, sessionID).Scan(&baseModel, &baseThinking); err != nil {
		t.Fatalf("read backfill: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" {
		t.Fatalf("backfill = (%q, %q), want the agent defaults", baseModel, baseThinking)
	}

	// AC-19: change the agent's defaults; the session keeps its snapshot.
	if _, err := pool.Exec(ctx, `UPDATE agent SET model = 'claude-opus-5', thinking_level = 'low' WHERE id = $1`, w.agentID); err != nil {
		t.Fatalf("update agent defaults: %v", err)
	}
	sent2, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "second turn", nil, "member", uuids(w.creatorID))
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	cc2 := readTaskChatConfig(t, pool, util.UUIDToString(sent2.Task.ID))
	if cc2["model"] != "claude-opus-5" || cc2["thinking_level"] != "high" {
		t.Fatalf("second snapshot = %+v, must stay on the session snapshot, not the new agent defaults", cc2)
	}

	// Override wins over the base in the snapshot (BLOCK-005).
	if _, err := pool.Exec(ctx, `UPDATE agent SET thinking_level = 'high' WHERE id = $1`, w.agentID); err != nil {
		t.Fatalf("restore agent: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_session SET thinking_level_override = 'low' WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("set override: %v", err)
	}
	sent3, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "third turn", nil, "member", uuids(w.creatorID))
	if err != nil {
		t.Fatalf("override send: %v", err)
	}
	cc3 := readTaskChatConfig(t, pool, util.UUIDToString(sent3.Task.ID))
	if cc3["model"] != "claude-opus-5" || cc3["thinking_level"] != "low" {
		t.Fatalf("override snapshot = %+v, want override thinking low to win", cc3)
	}
}

// TestPrivateAskSendValidationFailureZeroResidue: a §4.3 failure inside the
// send transaction rolls everything back — no message, no task, no backfill.
func TestPrivateAskSendValidationFailureZeroResidue(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	_, taskSvc := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheOK: false, liveErr: errors.New("injected live failure")})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM chat_session WHERE id = $1`, sessionID).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	if _, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "turn that must fail", nil, "member", uuids(w.creatorID)); !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("send with broken catalog: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	assertZeroResidue(t, pool, sessionID, updatedAt)
}

// TestOrdinaryChatSendUnchanged is BLOCK-006: an ordinary 1:1 chat (project_id
// NULL) keeps the baseline byte-for-byte — no backfill, no catalog I/O
// (a broken catalog must not matter), task.context stays NULL — and its
// PATCH is refused with 404.
func TestOrdinaryChatSendUnchanged(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	svc, taskSvc := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheOK: false, liveErr: errors.New("injected live failure")})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, pgtype.UUID{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	sent, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "ordinary chat turn", nil, "member", uuids(w.creatorID))
	if err != nil {
		t.Fatalf("ordinary send with broken catalog must succeed: %v", err)
	}
	var contextNull bool
	if err := pool.QueryRow(ctx, `SELECT context IS NULL FROM agent_task_queue WHERE id = $1`, util.UUIDToString(sent.Task.ID)).Scan(&contextNull); err != nil {
		t.Fatalf("read ordinary task: %v", err)
	}
	var sessionBaseNull bool
	if err := pool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&sessionBaseNull); err != nil {
		t.Fatalf("read ordinary session: %v", err)
	}
	if !contextNull {
		t.Fatal("ordinary chat task.context must stay NULL (no chat_config snapshot)")
	}
	if !sessionBaseNull {
		t.Fatal("ordinary chat must never be backfilled")
	}

	// The ordinary session refuses config patches (404).
	if _, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(sessionID), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{}); !errors.Is(err, ErrChatSessionNotFound) {
		t.Fatalf("ordinary session patch: got %v, want ErrChatSessionNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency fixtures (BLOCK-008/009/010)
// ---------------------------------------------------------------------------

// backendPID returns the backend pid of the given transaction.
func backendPID(t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var pid int
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	return pid
}

// waitForBlockedBy polls pg_blocking_pids until some backend waits on a lock
// held by holderPID, or fails after the timeout.
func waitForBlockedBy(t *testing.T, pool *pgxpool.Pool, holderPID int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE pg_blocking_pids(pid) @> ARRAY[$1::int] AND state = 'active'`,
			holderPID).Scan(&waiting); err != nil {
			t.Fatalf("probe blocked-by: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend ever blocked on holder pid %d", holderPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProjectClearRacingSendClearFirst is BLOCK-010 ①: the clear commits
// while the send is already parked on the session row lock; the send then
// re-reads the row under its lock, sees project_id NULL and takes the
// ordinary path — with a broken catalog proving no catalog I/O, and the
// production unbind sequence proving the carrier-guard failure leaves zero
// residue and a rebind re-sends cleanly.
func TestProjectClearRacingSendClearFirst(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	// Broken catalog: if the Private Ask branch ran, validation would fail.
	_, taskSvc := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheOK: false, liveErr: errors.New("injected live failure")})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	// The clear takes the session row lock in its own transaction and holds
	// it until we commit — the send must park on the same row lock.
	clearTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin clear tx: %v", err)
	}
	defer clearTx.Rollback(ctx)
	clearQ := db.New(pool).WithTx(clearTx)
	if err := clearQ.ClearChatSessionProjectByProject(ctx, db.ClearChatSessionProjectByProjectParams{
		ProjectID: uuids(w.projectID), WorkspaceID: uuids(w.workspaceID),
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	clearPID := backendPID(t, clearTx)

	done := make(chan error, 1)
	go func() {
		_, serr := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "clear-first turn", nil, "member", uuids(w.creatorID))
		done <- serr
	}()
	// The send is parked on the row lock the clear holds.
	waitForBlockedBy(t, pool, clearPID, 10*time.Second)
	select {
	case err := <-done:
		t.Fatalf("send must be parked on the row lock; finished early with %v", err)
	default:
	}

	if err := clearTx.Commit(ctx); err != nil {
		t.Fatalf("commit clear: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clear-first send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("send never resumed after the clear committed")
	}

	// Ordinary path after the clear: base_* still NULL, task context NULL,
	// and the broken catalog never mattered.
	var baseNull, contextNull bool
	var taskID string
	if err := pool.QueryRow(ctx, `SELECT base_model IS NULL AND base_thinking_level IS NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&baseNull); err != nil {
		t.Fatalf("read cleared session: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id, context IS NULL FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskID, &contextNull); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !baseNull || !contextNull {
		t.Fatalf("clear-first send must take the ordinary path (baseNull=%v contextNull=%v)", baseNull, contextNull)
	}
	// Message/task link: the user message is owned by the task (input batch).
	var msgCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND task_id = $2`, sessionID, taskID).Scan(&msgCount); err != nil {
		t.Fatalf("count linked messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("clear-first send must leave one message linked to the task (got %d)", msgCount)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID); err != nil {
		t.Fatalf("cleanup task: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID); err != nil {
		t.Fatalf("cleanup messages: %v", err)
	}

	// Failure injection b: the production unbind sequence makes the carrier
	// guard (task.go:2448-2456) reject the send. The session's project_id is
	// still NULL from the committed clear, so the unbind send takes the
	// ordinary path (no catalog I/O) and fails at the carrier re-read.
	var updatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM chat_session WHERE id = $1`, sessionID).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	unbindRuntime(t, pool, w)
	if _, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "unbound turn", nil, "member", uuids(w.creatorID)); !errors.Is(err, ErrChatTaskAgentNoRuntime) {
		t.Fatalf("unbound send: got %v, want ErrChatTaskAgentNoRuntime", err)
	}
	assertZeroResidue(t, pool, sessionID, updatedAt)

	// Rebind through the production agent update path; the next send works
	// (the row lock was released with the rollback).
	rebindRuntime(t, pool, w)
	agentRow, err = taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("reload agent after rebind: %v", err)
	}
	if _, err := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "rebound turn", nil, "member", uuids(w.creatorID)); err != nil {
		t.Fatalf("resend after rebind must succeed: %v", err)
	}
}

// TestProjectClearRacingSendSendFirst is BLOCK-010 ②: the send parks at the
// catalog latch holding the session row lock (already backfilled); the clear
// blocks on the same row lock; once released the send commits its snapshot,
// and the late clear cannot touch the persisted backfill/override/snapshot.
// The failure injection b variant parks the same way, then the production
// unbind sequence commits and the carrier guard rolls the whole turn back.
func TestProjectClearRacingSendSendFirst(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)

	// --- Success order: send commits first; the late clear changes nothing ---
	port := &latchCatalogPort{liveResult: validPrivateAskCatalog(), entered: make(chan struct{}), release: make(chan struct{})}
	_, taskSvc := newPrivateAskServices(pool, port)

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, serr := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "send-first turn", nil, "member", uuids(w.creatorID))
		done <- serr
	}()
	select {
	case <-port.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("send never reached the catalog latch")
	}

	// While the send holds the session row lock at the latch, the project
	// clear must block on the same row.
	clearDone := make(chan error, 1)
	go func() {
		clearTx, terr := pool.Begin(ctx)
		if terr != nil {
			clearDone <- terr
			return
		}
		defer clearTx.Rollback(ctx)
		clearQ := db.New(pool).WithTx(clearTx)
		terr = clearQ.ClearChatSessionProjectByProject(ctx, db.ClearChatSessionProjectByProjectParams{
			ProjectID: uuids(w.projectID), WorkspaceID: uuids(w.workspaceID),
		})
		if terr != nil {
			clearDone <- terr
			return
		}
		clearDone <- clearTx.Commit(ctx)
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-clearDone:
		t.Fatalf("clear must be parked on the send's row lock; finished early with %v", err)
	default:
	}

	close(port.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send-first send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("send never finished after latch release")
	}
	select {
	case err := <-clearDone:
		if err != nil {
			t.Fatalf("clear after send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("clear never resumed after the send committed")
	}

	// Snapshot == the locked-row resolution, backfill written exactly once,
	// and the late clear left every persisted value immutable.
	var baseModel, baseThinking, projectID string
	if err := pool.QueryRow(ctx, `SELECT base_model, base_thinking_level, project_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&baseModel, &baseThinking, &projectID); err != nil {
		t.Fatalf("read session after clear: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" {
		t.Fatalf("backfill = (%q, %q), want the locked-row agent defaults", baseModel, baseThinking)
	}
	if projectID != "" {
		t.Fatalf("project_id = %q after clear, want NULL", projectID)
	}
	var taskID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskID); err != nil {
		t.Fatalf("load task: %v", err)
	}
	cc := readTaskChatConfig(t, pool, taskID)
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("snapshot = %+v, want the locked-row resolution (no stale pre-lock values)", cc)
	}

	// --- Failure injection b: unbind while parked at the latch (after the
	// backfill); the carrier guard rolls the whole turn back. ---
	port2 := &latchCatalogPort{liveResult: validPrivateAskCatalog(), entered: make(chan struct{}), release: make(chan struct{})}
	_, taskSvc2 := newPrivateAskServices(pool, port2)
	session2ID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session2, err := taskSvc2.Queries.GetChatSession(ctx, uuids(session2ID))
	if err != nil {
		t.Fatalf("load session 2: %v", err)
	}
	agentRow2, err := taskSvc2.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent 2: %v", err)
	}
	var updatedAt2 time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM chat_session WHERE id = $1`, session2ID).Scan(&updatedAt2); err != nil {
		t.Fatalf("read updated_at 2: %v", err)
	}

	done2 := make(chan error, 1)
	go func() {
		_, serr := taskSvc2.SendDirectChatMessage(ctx, session2, agentRow2, uuids(w.creatorID), "turn with enqueue failure", nil, "member", uuids(w.creatorID))
		done2 <- serr
	}()
	select {
	case <-port2.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("second send never reached the catalog latch")
	}

	// The transaction holds only the session row lock here (the carrier
	// re-read comes later), so the production unbind sequence can commit.
	unbindRuntime(t, pool, w)
	// Release with a VALID model list: the failure below is the carrier
	// guard, not the catalog.
	close(port2.release)
	select {
	case err := <-done2:
		if !errors.Is(err, ErrChatTaskAgentNoRuntime) {
			t.Fatalf("enqueue-failure send: got %v, want ErrChatTaskAgentNoRuntime", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second send never finished after latch release")
	}
	// The rollback covers the backfill that already happened inside the tx.
	assertZeroResidue(t, pool, session2ID, updatedAt2)

	// Rebind; the very same session sends cleanly (lock released on rollback).
	rebindRuntime(t, pool, w)
	agentRow2, err = taskSvc2.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("reload agent 2 after rebind: %v", err)
	}
	if _, err := taskSvc2.SendDirectChatMessage(ctx, session2, agentRow2, uuids(w.creatorID), "resend after enqueue failure", nil, "member", uuids(w.creatorID)); err != nil {
		t.Fatalf("resend after rebind must succeed: %v", err)
	}
}

// TestCreateChatTaskQueryBoundaryInsertZeroRows pins the INSERT 0-row
// semantics at the query seam: with the carrier runtime deleted (after the
// production unbind sequence), the lock_task_owner_rows fence predicate is
// false and CreateChatTask writes no row — sqlc's :one surfaces pgx.ErrNoRows
// (the same seam the send transaction's carrier guard protects earlier).
func TestCreateChatTaskQueryBoundaryInsertZeroRows(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	_, taskSvc := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	unbindRuntime(t, pool, w)

	// Same parameter shape as the send transaction's CreateChatTask call:
	// the fence's runtime owner row is gone, so the INSERT writes 0 rows.
	_, err := taskSvc.Queries.CreateChatTask(ctx, db.CreateChatTaskParams{
		ID:                dbid.NewV7(),
		AgentID:           uuids(w.agentID),
		RuntimeID:         uuids(w.runtimeID), // deleted by the unbind sequence
		Priority:          2,
		ChatSessionID:     uuids(sessionID),
		InitiatorUserID:   uuids(w.creatorID),
		OriginatorUserID:  uuids(w.creatorID),
		AccountableUserID: uuids(w.creatorID),
		ForceFreshSession: pgtype.Bool{Bool: false, Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateChatTask with a deleted runtime owner: got %v, want pgx.ErrNoRows", err)
	}
}

// unbindRuntime performs the production unbind sequence (runtime.sql:355 ->
// UnbindTasksFromRuntime -> DeleteAgentRuntime; the RESTRICT fkey on
// agent.runtime_id is satisfied by the first step).
func unbindRuntime(t *testing.T, pool *pgxpool.Pool, w privateAskWorld) {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	if _, err := q.UnbindUserAgentsFromRuntime(ctx, uuids(w.runtimeID)); err != nil {
		t.Fatalf("unbind user agents: %v", err)
	}
	if _, err := q.UnbindTasksFromRuntime(ctx, uuids(w.runtimeID)); err != nil {
		t.Fatalf("unbind tasks: %v", err)
	}
	if err := q.DeleteAgentRuntime(ctx, uuids(w.runtimeID)); err != nil {
		t.Fatalf("delete runtime: %v", err)
	}
}

// rebindRuntime re-attaches the agent to a fresh runtime row (the production
// agent-update path shape: a new runtime row + the agent's runtime_id write).
func rebindRuntime(t *testing.T, pool *pgxpool.Pool, w privateAskWorld) {
	t.Helper()
	ctx := context.Background()
	var newRuntimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'pa-runtime-2', 'cloud', 'claude', 'online', '', '{}'::jsonb, $2) RETURNING id`,
		w.workspaceID, w.creatorID).Scan(&newRuntimeID); err != nil {
		t.Fatalf("create rebind runtime: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, newRuntimeID, w.agentID); err != nil {
		t.Fatalf("rebind agent: %v", err)
	}
}

// TestPatchRacingSendSerialized is BLOCK-008: PATCH and send serialize on the
// same chat_session row lock. The send parks at the latch after its backfill;
// the PATCH blocks on the row lock; after the release the PATCH re-reads the
// backfilled row and writes its override — no lost update, one consistent
// base_*, and the send snapshot reflects the locked-row resolution (no
// pre-lock stale values).
func TestPatchRacingSendSerialized(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	port := &latchCatalogPort{liveResult: validPrivateAskCatalog(), entered: make(chan struct{}), release: make(chan struct{})}
	svc, taskSvc := newPrivateAskServices(pool, port)

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
	session, err := taskSvc.Queries.GetChatSession(ctx, uuids(sessionID))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	agentRow, err := taskSvc.Queries.GetAgent(ctx, uuids(w.agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, serr := taskSvc.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "patch-race turn", nil, "member", uuids(w.creatorID))
		done <- serr
	}()
	select {
	case <-port.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("send never reached the catalog latch")
	}

	// The PATCH must block on the session row lock the send holds.
	patchDone := make(chan error, 1)
	go func() {
		_, perr := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(sessionID), uuids(w.creatorID), "claude",
			ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"},
			ChatConfigFieldPatch{Present: true, Value: "low"})
		patchDone <- perr
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-patchDone:
		t.Fatalf("patch must be parked on the send's row lock; finished early with %v", err)
	default:
	}

	close(port.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("send never finished")
	}
	select {
	case err := <-patchDone:
		if err != nil {
			t.Fatalf("patch after send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("patch never resumed")
	}

	// One consistent base (the send's locked-row backfill), the patch's
	// override survived, and the send snapshot matches the locked-row
	// resolution at send time (pre-override values — nothing stale).
	var baseModel, baseThinking, overrideModel, overrideThinking string
	if err := pool.QueryRow(ctx, `SELECT base_model, base_thinking_level, model_override, thinking_level_override FROM chat_session WHERE id = $1`, sessionID).Scan(&baseModel, &baseThinking, &overrideModel, &overrideThinking); err != nil {
		t.Fatalf("read final row: %v", err)
	}
	if baseModel != "claude-opus-5" || baseThinking != "high" {
		t.Fatalf("base = (%q, %q), want one consistent backfill", baseModel, baseThinking)
	}
	if overrideModel != "claude-opus-5" || overrideThinking != "low" {
		t.Fatalf("overrides = (%q, %q), want the patch's values to survive", overrideModel, overrideThinking)
	}
	var taskID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskID); err != nil {
		t.Fatalf("load task: %v", err)
	}
	cc := readTaskChatConfig(t, pool, taskID)
	if cc["model"] != "claude-opus-5" || cc["thinking_level"] != "high" {
		t.Fatalf("send snapshot = %+v, want the locked-row pre-patch resolution", cc)
	}

	// Reverse order: a committed PATCH first, then the send snapshots the
	// override (BLOCK-008 ①).
	port2 := &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true}
	svc2, taskSvc2 := newPrivateAskServices(pool, port2)
	if _, err := svc2.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(sessionID), uuids(w.creatorID), "claude",
		ChatConfigFieldPatch{}, ChatConfigFieldPatch{Present: true, Value: "high"}); err != nil {
		t.Fatalf("second patch: %v", err)
	}
	sent, err := taskSvc2.SendDirectChatMessage(ctx, session, agentRow, uuids(w.creatorID), "after patch turn", nil, "member", uuids(w.creatorID))
	if err != nil {
		t.Fatalf("send after patch: %v", err)
	}
	cc2 := readTaskChatConfig(t, pool, util.UUIDToString(sent.Task.ID))
	if cc2["model"] != "claude-opus-5" || cc2["thinking_level"] != "high" {
		t.Fatalf("post-patch snapshot = %+v, want the override values", cc2)
	}
}

// TestConcurrentPatchesNoLostUpdate is BLOCK-008 ②: two concurrent PATCHes
// each setting one field serialize on the row lock; both overrides survive.
func TestConcurrentPatchesNoLostUpdate(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	w := seedPrivateAskWorld(t, pool)
	svc, _ := newPrivateAskServices(pool, &fakeChatCatalogPort{cacheResult: validPrivateAskCatalog(), cacheOK: true})

	sessionID := seedPrivateAskSession(t, pool, w, w.creatorID, uuids(w.projectID), pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})

	modelPatchDone := make(chan error, 1)
	thinkingPatchDone := make(chan error, 1)
	go func() {
		_, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(sessionID), uuids(w.creatorID), "claude",
			ChatConfigFieldPatch{Present: true, Value: "claude-opus-5"}, ChatConfigFieldPatch{})
		modelPatchDone <- err
	}()
	go func() {
		_, err := svc.PatchChatSessionConfig(ctx, uuids(w.workspaceID), uuids(sessionID), uuids(w.creatorID), "claude",
			ChatConfigFieldPatch{}, ChatConfigFieldPatch{Present: true, Value: "low"})
		thinkingPatchDone <- err
	}()
	for i, ch := range []<-chan error{modelPatchDone, thinkingPatchDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("concurrent patch %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("concurrent patch %d never finished", i)
		}
	}

	var modelOverride, thinkingOverride string
	if err := pool.QueryRow(ctx, `SELECT model_override, thinking_level_override FROM chat_session WHERE id = $1`, sessionID).Scan(&modelOverride, &thinkingOverride); err != nil {
		t.Fatalf("read final overrides: %v", err)
	}
	if modelOverride != "claude-opus-5" || thinkingOverride != "low" {
		t.Fatalf("overrides = (%q, %q), want both fields to survive (no lost update)", modelOverride, thinkingOverride)
	}
}
