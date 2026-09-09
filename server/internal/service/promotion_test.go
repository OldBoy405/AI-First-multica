package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AIFIRST: CR-2026-061 TASK-02 tests. Pure-function tests run everywhere;
// DB-backed tests skip when the test database is unreachable (same pattern
// as newResolveOriginatorPool).

func promoUUID(s string) pgtype.UUID { return util.MustParseUUID(s) }

const (
	promoProject = "00000000-0000-0000-0000-00000000000a"
	promoSession = "00000000-0000-0000-0000-00000000000b"
	promoMsgA    = "11111111-1111-1111-1111-111111111111"
	promoMsgB    = "22222222-2222-2222-2222-222222222222"
	promoAtt     = "33333333-3333-3333-3333-333333333333"
)

func TestCanonicalUUIDsSortsDedupesLowercases(t *testing.T) {
	upper := strings.ToUpper(promoMsgB)
	ids := []pgtype.UUID{
		promoUUID(upper),
		promoUUID(promoMsgA),
		promoUUID(promoMsgB),
		promoUUID(promoMsgA),
		{}, // invalid: dropped
	}
	got := canonicalUUIDs(ids)
	want := []string{promoMsgA, promoMsgB}
	if len(got) != len(want) {
		t.Fatalf("canonicalUUIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonicalUUIDs = %v, want %v", got, want)
		}
	}
	if got := canonicalUUIDs(nil); got == nil || len(got) != 0 {
		t.Fatalf("canonicalUUIDs(nil) = %v, want empty non-nil slice", got)
	}
}

func TestPromotionDigestsFixedVectors(t *testing.T) {
	projectID := promoUUID(promoProject)
	sessionID := promoUUID(promoSession)
	msg := []pgtype.UUID{promoUUID(promoMsgB), promoUUID(promoMsgA)} // reordered
	att := []pgtype.UUID{promoUUID(promoAtt)}

	// Vectors computed from the canonical sorted/lowercased JSON
	// (fixed key order), see SDD §4.1.
	const wantFPFalse = "76c9f406d688cfcc7d32413ab61e3cfbaa21c4ef0066ae2d8b5fb7df36b414f2"
	const wantFPTrue = "d2d321040ae31759d4ca17af0f6ad1768083918aeb07d77ca50c5bf79f51f0a2"
	const wantDedupe = "07179c0c8a0932d154e6a0ce56265e65c836f3ba94b83a9d169a23da439b1bcb"

	if got := promotionFingerprint(projectID, sessionID, msg, att, false); got != wantFPFalse {
		t.Errorf("fingerprint(false) = %s, want %s", got, wantFPFalse)
	}
	if got := promotionFingerprint(projectID, sessionID, msg, att, true); got != wantFPTrue {
		t.Errorf("fingerprint(true) = %s, want %s", got, wantFPTrue)
	}
	if got := promotionDedupeKey(sessionID, msg, att); got != wantDedupe {
		t.Errorf("dedupeKey = %s, want %s", got, wantDedupe)
	}

	// upgrade_to_cr must change the fingerprint but never the dedupe key.
	if promotionFingerprint(projectID, sessionID, msg, att, false) == promotionFingerprint(projectID, sessionID, msg, att, true) {
		t.Error("fingerprint identical across upgrade_to_cr")
	}
	if promotionDedupeKey(sessionID, msg, att) != wantDedupe {
		t.Error("dedupe key changed by unrelated reordering")
	}

	// Duplicates and uppercase inputs normalize to the same digests.
	dupMsg := []pgtype.UUID{promoUUID(promoMsgB), promoUUID(promoMsgB), promoUUID(strings.ToUpper(promoMsgA))}
	if got := promotionDedupeKey(sessionID, dupMsg, att); got != wantDedupe {
		t.Errorf("dedupeKey with dup/uppercase = %s, want %s", got, wantDedupe)
	}
	if got := promotionFingerprint(projectID, sessionID, dupMsg, att, false); got != wantFPFalse {
		t.Errorf("fingerprint with dup/uppercase = %s, want %s", got, wantFPFalse)
	}

	// title/description never participate (FR-7): they are not inputs at all,
	// so a different project must change ONLY the fingerprint, not the key.
	otherProject := promoUUID("44444444-4444-4444-4444-444444444444")
	if promotionFingerprint(otherProject, sessionID, msg, att, false) == wantFPFalse {
		t.Error("fingerprint unchanged across projects")
	}
	if promotionDedupeKey(sessionID, msg, att) != wantDedupe {
		t.Error("dedupe key changed across projects (must not — FR-6)")
	}
}

func TestPromotionAdvisoryKeyPrefixIsolation(t *testing.T) {
	ws := promoUUID(promoProject)
	prj := promoUUID(promoSession)
	key := promotionAdvisoryKey(ws, prj)
	if !strings.HasPrefix(key, promotionAdvisoryPrefix+"|") {
		t.Fatalf("advisory key %q lacks prefix", key)
	}
	if strings.HasPrefix(key, DiscussionSessionAdvisoryPrefix) {
		t.Fatalf("advisory key %q shares the discussion-session prefix (D-4)", key)
	}
	if strings.Contains(key, "||") {
		t.Fatalf("advisory key %q contains empty segment", key)
	}
}

func TestSummarizePromotionContentTruncatesByRune(t *testing.T) {
	if got := summarizePromotionContent("  a\nb\tc  ", 10); got != "a b c" {
		t.Fatalf("flatten = %q", got)
	}
	short := strings.Repeat("x", 120)
	if got := summarizePromotionContent(short, 120); got != short {
		t.Fatalf("exact-limit content truncated: %q", got)
	}
	long := strings.Repeat("中", 121)
	got := summarizePromotionContent(long, 120)
	if len([]rune(got)) != 121 {
		t.Fatalf("truncated rune length = %d, want 121 (120 + ellipsis)", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated content missing ellipsis: %q", got)
	}
}

func TestBuildPromotionEntryRoundTrip(t *testing.T) {
	p := PromoteDiscussionParams{
		ProjectID:     promoUUID(promoProject),
		SessionID:     promoUUID(promoSession),
		CallerID:      promoUUID(promoMsgA),
		MessageIDs:    []pgtype.UUID{promoUUID(promoMsgB), promoUUID(promoMsgA), promoUUID(promoMsgA)},
		AttachmentIDs: []pgtype.UUID{promoUUID(promoAtt)},
		UpgradeToCR:   true,
	}
	dedupe := promotionDedupeKey(p.SessionID, p.MessageIDs, p.AttachmentIDs)
	runID := promoUUID("55555555-5555-5555-5555-555555555555")
	entry := buildPromotionEntry(p, dedupe, runID, time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC))

	if entry.Kind != promotionContextRefKind || entry.DedupeKey != dedupe {
		t.Fatalf("entry kind/key = %q/%q", entry.Kind, entry.DedupeKey)
	}
	if entry.SessionID != promoSession || entry.PromotedBy != promoMsgA {
		t.Fatalf("entry ids = %q/%q", entry.SessionID, entry.PromotedBy)
	}
	if len(entry.MessageIDs) != 2 || entry.MessageIDs[0] != promoMsgA || entry.MessageIDs[1] != promoMsgB {
		t.Fatalf("entry message_ids = %v, want sorted deduped", entry.MessageIDs)
	}
	if entry.PipelineRunID != "55555555-5555-5555-5555-555555555555" {
		t.Fatalf("entry pipeline_run_id = %q", entry.PipelineRunID)
	}

	raw, err := json.Marshal([]promotionContextRefEntry{entry})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	entries, err := parsePromotionContextRefs(raw)
	if err != nil {
		t.Fatalf("parse entry: %v", err)
	}
	got, idx, ok := findPromotionEntry(entries, dedupe)
	if !ok || idx != 0 || got.PipelineRunID != entry.PipelineRunID {
		t.Fatalf("find entry = %+v idx=%d ok=%v", got, idx, ok)
	}
	refs, err := entrySourceRefs(got)
	if err != nil {
		t.Fatalf("entrySourceRefs: %v", err)
	}
	if util.UUIDToString(refs.SessionID) != promoSession || len(refs.MessageIDs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestPromotionSelectionDefensiveRejections(t *testing.T) {
	svc := &IssueService{}
	base := PromoteDiscussionParams{
		WorkspaceID:    promoUUID(promoProject),
		ProjectID:      promoUUID(promoProject),
		SessionID:      promoUUID(promoSession),
		CallerID:       promoUUID(promoMsgA),
		MessageIDs:     []pgtype.UUID{promoUUID(promoMsgA)},
		IdempotencyKey: "k",
	}
	cases := []struct {
		name   string
		mutate func(*PromoteDiscussionParams)
	}{
		{"both empty", func(p *PromoteDiscussionParams) { p.MessageIDs = nil }},
		{"too many messages", func(p *PromoteDiscussionParams) {
			ids := make([]pgtype.UUID, promotionMaxItems+1)
			for i := range ids {
				ids[i] = promoUUID(promoMsgA)
			}
			p.MessageIDs = ids
		}},
		{"too many attachments", func(p *PromoteDiscussionParams) {
			ids := make([]pgtype.UUID, promotionMaxItems+1)
			for i := range ids {
				ids[i] = promoUUID(promoAtt)
			}
			p.AttachmentIDs = ids
		}},
		{"missing key", func(p *PromoteDiscussionParams) { p.IdempotencyKey = "" }},
		{"invalid workspace", func(p *PromoteDiscussionParams) { p.WorkspaceID = pgtype.UUID{} }},
	}
	for _, tc := range cases {
		p := base
		tc.mutate(&p)
		if _, err := svc.PromoteDiscussion(context.Background(), p); !errors.Is(err, ErrInvalidPromotionSelection) {
			t.Errorf("%s: err = %v, want ErrInvalidPromotionSelection", tc.name, err)
		}
	}
}

// ── DB-backed integration tests (skip without a reachable database) ──

func newPromotionPool(t *testing.T) *pgxpool.Pool {
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

// promoFixture seeds workspace/user/member/project/active shared session and
// one authored message; returns ids. All rows are cleaned up on t.Cleanup.
type promoFixture struct {
	WorkspaceID pgtype.UUID
	UserID      pgtype.UUID
	ProjectID   pgtype.UUID
	SessionID   pgtype.UUID
	MessageID   pgtype.UUID
	Attachment  pgtype.UUID
}

func seedPromotionFixture(t *testing.T, pool *pgxpool.Pool) promoFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	f := promoFixture{}

	var ws, user, project, session, message string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('promo ws', $1) RETURNING id`,
		fmt.Sprintf("promo-%d", suffix)).Scan(&ws); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, ws) })
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Promo User', $1) RETURNING id`,
		fmt.Sprintf("promo-%d@multica.test", suffix)).Scan(&user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, user) })
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, ws, user); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO project (workspace_id, title) VALUES ($1, 'Promo Project') RETURNING id`, ws).Scan(&project); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, project_id, kind)
		VALUES ($1, NULL, $2, '', $3, 'project_shared') RETURNING id`, ws, user, project).Scan(&session); err != nil {
		t.Fatalf("seed chat_session: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, author_type, author_id)
		VALUES ($1, 'user', 'first message', 'member', $2) RETURNING id`, session, user).Scan(&message); err != nil {
		t.Fatalf("seed chat_message: %v", err)
	}

	f.WorkspaceID = promoUUID(ws)
	f.UserID = promoUUID(user)
	f.ProjectID = promoUUID(project)
	f.SessionID = promoUUID(session)
	f.MessageID = promoUUID(message)
	return f
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestPromoteDiscussionCreateAndReplay(t *testing.T) {
	pool := newPromotionPool(t)
	ctx := context.Background()
	q := db.New(pool)
	f := seedPromotionFixture(t, pool)
	svc := NewIssueService(q, pool, events.New(), nil, &TaskService{Queries: q})

	params := PromoteDiscussionParams{
		WorkspaceID:    f.WorkspaceID,
		ProjectID:      f.ProjectID,
		SessionID:      f.SessionID,
		CallerID:       f.UserID,
		MessageIDs:     []pgtype.UUID{f.MessageID},
		UpgradeToCR:    true,
		IdempotencyKey: "promo-key-1",
	}

	res, err := svc.PromoteDiscussion(ctx, params)
	if err != nil {
		t.Fatalf("PromoteDiscussion: %v", err)
	}
	if !res.Created || !res.UpgradeToCR || !res.RunID.Valid || !res.IssueID.Valid {
		t.Fatalf("create result = %+v", res)
	}
	if res.SourceRefs.SessionID != f.SessionID || len(res.SourceRefs.MessageIDs) != 1 {
		t.Fatalf("create source_refs = %+v", res.SourceRefs)
	}

	// AC-1: exactly one work issue for the source.
	if n := countRows(t, pool, `SELECT count(*) FROM issue WHERE id = $1`, res.IssueID); n != 1 {
		t.Fatalf("issue rows = %d, want 1", n)
	}
	var status, priority string
	if err := pool.QueryRow(ctx, `SELECT status, priority FROM issue WHERE id = $1`, res.IssueID).Scan(&status, &priority); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if status != promotionCreatedStatus || priority != promotionCreatedPriority {
		t.Errorf("issue status/priority = %s/%s, want %s/%s", status, priority, promotionCreatedStatus, promotionCreatedPriority)
	}

	// AC-3: context_refs entry fields (kind/dedupe/pipeline_run_id).
	var rawRefs []byte
	if err := pool.QueryRow(ctx, `SELECT context_refs FROM issue WHERE id = $1`, res.IssueID).Scan(&rawRefs); err != nil {
		t.Fatalf("read context_refs: %v", err)
	}
	entries, err := parsePromotionContextRefs(rawRefs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, err = %v", entries, err)
	}
	e := entries[0]
	if e.Kind != promotionContextRefKind || e.DedupeKey == "" || e.PipelineRunID != util.UUIDToString(res.RunID) ||
		e.PromotedBy != util.UUIDToString(f.UserID) || e.SessionID != util.UUIDToString(f.SessionID) {
		t.Fatalf("entry = %+v", e)
	}

	// AC-8: pre-built run + first node in the same commit; no agent_task_queue.
	var run db.PipelineRun
	if err := pool.QueryRow(ctx, `SELECT * FROM pipeline_run WHERE id = $1`, res.RunID).Scan(
		&run.ID, &run.WorkspaceID, &run.PipelineID, &run.CrID, &run.IssueID, &run.Status,
		&run.Inputs, &run.ExecutionContext, &run.StartedBy, &run.CreatedAt, &run.CompletedAt,
	); err != nil {
		t.Fatalf("read pipeline_run: %v", err)
	}
	if run.PipelineID != "requirement-authoring" || run.CrID.Valid || run.Status != "running" ||
		util.UUIDToString(run.IssueID) != util.UUIDToString(res.IssueID) || run.StartedBy != f.UserID {
		t.Fatalf("run = %+v", run)
	}
	var inputs promotionRunInputs
	if err := json.Unmarshal(run.Inputs, &inputs); err != nil {
		t.Fatalf("decode run inputs: %v", err)
	}
	if inputs.SessionID != util.UUIDToString(f.SessionID) || len(inputs.MessageIDs) != 1 {
		t.Fatalf("run inputs = %+v", inputs)
	}
	var execCtx promotionRunExecutionContext
	if err := json.Unmarshal(run.ExecutionContext, &execCtx); err != nil {
		t.Fatalf("decode execution_context: %v", err)
	}
	if execCtx.Intent != "discussion-promotion" || !execCtx.ExpectBind || execCtx.Source.ProjectID != util.UUIDToString(f.ProjectID) {
		t.Fatalf("execution_context = %+v", execCtx)
	}
	var nodeRun db.PipelineNodeRun
	if err := pool.QueryRow(ctx, `SELECT * FROM pipeline_node_run WHERE run_id = $1`, res.RunID).Scan(
		&nodeRun.ID, &nodeRun.RunID, &nodeRun.NodeID, &nodeRun.Ref, &nodeRun.Kind, &nodeRun.Seq, &nodeRun.Status,
		&nodeRun.Attempt, &nodeRun.ApprovalID, &nodeRun.OutputNote, &nodeRun.Detail, &nodeRun.StartedAt, &nodeRun.CompletedAt,
	); err != nil {
		t.Fatalf("read pipeline_node_run: %v", err)
	}
	if nodeRun.NodeID != promotionFirstNodeID || nodeRun.Ref.String != promotionFirstNodeRef ||
		nodeRun.Kind != "skill" || nodeRun.Seq != 1 || nodeRun.Status != "running" || nodeRun.Attempt != 1 {
		t.Fatalf("node run = %+v", nodeRun)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM agent_task_queue WHERE pipeline_node_run_id = $1`, nodeRun.ID); n != 0 {
		t.Fatalf("agent_task_queue rows for promotion node = %d, want 0 (AC-8)", n)
	}

	// AC-4: chat rows untouched (FR-5 zero-write).
	if n := countRows(t, pool, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, f.SessionID); n != 1 {
		t.Fatalf("chat_message rows = %d, want 1", n)
	}

	// AC-5: same key + same payload replays the stored response, created=false.
	replay, err := svc.PromoteDiscussion(ctx, params)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Created || replay.IssueID != res.IssueID || replay.RunID != res.RunID || replay.IssueNumber != res.IssueNumber {
		t.Fatalf("replay = %+v, want created=false same issue/run", replay)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND id = $2`, f.WorkspaceID, res.IssueID); n != 1 {
		t.Fatalf("issue rows after replay = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_run WHERE issue_id = $1`, res.IssueID); n != 1 {
		t.Fatalf("pipeline_run rows after replay = %d, want 1", n)
	}

	// Same key, different payload (added attachment) → 409, zero writes.
	conflict := params
	conflict.AttachmentIDs = []pgtype.UUID{promoUUID(promoAtt)}
	if _, err := svc.PromoteDiscussion(ctx, conflict); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("conflict err = %v, want ErrIdempotencyKeyReused", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1`, f.WorkspaceID); n != 1 {
		t.Fatalf("issue rows after conflict = %d, want 1", n)
	}
}

func TestPromoteDiscussionDedupeHitAndUpgradeBackfill(t *testing.T) {
	pool := newPromotionPool(t)
	ctx := context.Background()
	q := db.New(pool)
	f := seedPromotionFixture(t, pool)
	svc := NewIssueService(q, pool, events.New(), nil, &TaskService{Queries: q})

	// Plain promotion first (upgrade_to_cr=false).
	plain := PromoteDiscussionParams{
		WorkspaceID:    f.WorkspaceID,
		ProjectID:      f.ProjectID,
		SessionID:      f.SessionID,
		CallerID:       f.UserID,
		MessageIDs:     []pgtype.UUID{f.MessageID},
		UpgradeToCR:    false,
		IdempotencyKey: "plain-key",
	}
	first, err := svc.PromoteDiscussion(ctx, plain)
	if err != nil {
		t.Fatalf("plain promotion: %v", err)
	}
	if !first.Created || first.RunID.Valid {
		t.Fatalf("plain result = %+v", first)
	}

	// Upgrade promotion with a NEW key: same source → dedupe hit, same
	// issue, backfilled run (FR-6 / FR-10).
	upgrade := plain
	upgrade.IdempotencyKey = "upgrade-key"
	upgrade.UpgradeToCR = true
	second, err := svc.PromoteDiscussion(ctx, upgrade)
	if err != nil {
		t.Fatalf("upgrade promotion: %v", err)
	}
	if second.Created || second.IssueID != first.IssueID || !second.UpgradeToCR || !second.RunID.Valid {
		t.Fatalf("upgrade result = %+v", second)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_run WHERE issue_id = $1`, first.IssueID); n != 1 {
		t.Fatalf("pipeline_run rows after backfill = %d, want 1", n)
	}

	// Entry now carries pipeline_run_id.
	var rawRefs []byte
	if err := pool.QueryRow(ctx, `SELECT context_refs FROM issue WHERE id = $1`, first.IssueID).Scan(&rawRefs); err != nil {
		t.Fatalf("read context_refs: %v", err)
	}
	entries, err := parsePromotionContextRefs(rawRefs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v err = %v", entries, err)
	}
	if entries[0].PipelineRunID != util.UUIDToString(second.RunID) {
		t.Fatalf("entry pipeline_run_id = %q, want %s", entries[0].PipelineRunID, util.UUIDToString(second.RunID))
	}

	// Second upgrade with yet another key: no second run, same id returned.
	third, err := svc.PromoteDiscussion(ctx, func() PromoteDiscussionParams {
		p := upgrade
		p.IdempotencyKey = "upgrade-key-2"
		return p
	}())
	if err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	if third.Created || third.RunID != second.RunID {
		t.Fatalf("second upgrade result = %+v, want same run id %s", third, util.UUIDToString(second.RunID))
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_run WHERE issue_id = $1`, first.IssueID); n != 1 {
		t.Fatalf("pipeline_run rows after second upgrade = %d, want 1", n)
	}

	// Plain promotion replay with new key (upgrade_to_cr=false again): same
	// issue, no run change, created=false.
	plainAgain := plain
	plainAgain.IdempotencyKey = "plain-key-2"
	fourth, err := svc.PromoteDiscussion(ctx, plainAgain)
	if err != nil {
		t.Fatalf("plain again: %v", err)
	}
	if fourth.Created || fourth.IssueID != first.IssueID || fourth.RunID.Valid {
		t.Fatalf("plain again result = %+v", fourth)
	}
}

// TestPromoteDiscussionBackfillPreservesHeterogeneousContextRefs is the
// B-CODE-01 regression test: the upgrade backfill must merge pipeline_run_id
// into ONLY the matched promotion element, leaving every other array element
// (legacy non-promotion entry, other promotion entry) and every unknown
// extension field byte-for-byte intact (SDD §2.1 append semantics).
func TestPromoteDiscussionBackfillPreservesHeterogeneousContextRefs(t *testing.T) {
	pool := newPromotionPool(t)
	ctx := context.Background()
	q := db.New(pool)
	f := seedPromotionFixture(t, pool)
	svc := NewIssueService(q, pool, events.New(), nil, &TaskService{Queries: q})

	// Plain promotion first: creates the issue and the matched entry.
	plain := PromoteDiscussionParams{
		WorkspaceID:    f.WorkspaceID,
		ProjectID:      f.ProjectID,
		SessionID:      f.SessionID,
		CallerID:       f.UserID,
		MessageIDs:     []pgtype.UUID{f.MessageID},
		UpgradeToCR:    false,
		IdempotencyKey: "plain-preserve-key",
	}
	first, err := svc.PromoteDiscussion(ctx, plain)
	if err != nil {
		t.Fatalf("plain promotion: %v", err)
	}

	// Simulate historical / extension data on the SAME issue:
	// [0] the promotion entry gains unknown extra fields (flat + nested),
	// [1] a legacy non-promotion entry of another kind,
	// [2] a second promotion entry for a DIFFERENT source set.
	legacy := fmt.Sprintf(`{"kind":"source_link","source_issue_id":"%s","label":"upstream","meta":{"n":7}}`, promoMsgB)
	const extras = `{"extra_field":"keep-me","nested_obj":{"k":"v","n":[1,2,3]}}`
	const otherPromo = `{"kind":"discussion_promotion","session_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","message_ids":[],"attachment_ids":[],"dedupe_key":"other-dedupe","promoted_by":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","promoted_at":"2026-01-01T00:00:00Z"}`
	if _, err := pool.Exec(ctx, `
		UPDATE issue
		SET context_refs = jsonb_build_array(
			context_refs -> 0 || $2::jsonb,
			$3::jsonb,
			$4::jsonb
		)
		WHERE id = $1`, first.IssueID, extras, legacy, otherPromo); err != nil {
		t.Fatalf("seed heterogeneous context_refs: %v", err)
	}

	// Upgrade with a NEW key: same source → dedupe hit → backfill merge.
	upgrade := plain
	upgrade.UpgradeToCR = true
	upgrade.IdempotencyKey = "upgrade-preserve-key"
	second, err := svc.PromoteDiscussion(ctx, upgrade)
	if err != nil {
		t.Fatalf("upgrade promotion: %v", err)
	}
	if second.Created || !second.UpgradeToCR || !second.RunID.Valid || second.IssueID != first.IssueID {
		t.Fatalf("upgrade result = %+v", second)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_run WHERE issue_id = $1`, first.IssueID); n != 1 {
		t.Fatalf("pipeline_run rows after backfill = %d, want 1", n)
	}

	// Array length unchanged: no element added or dropped.
	var length int
	if err := pool.QueryRow(ctx, `SELECT jsonb_array_length(context_refs) FROM issue WHERE id = $1`, first.IssueID).Scan(&length); err != nil {
		t.Fatalf("array length: %v", err)
	}
	if length != 3 {
		t.Fatalf("context_refs length = %d, want 3", length)
	}

	// Matched element: pipeline_run_id merged in, extras preserved.
	var runID, extra, nestedK string
	var nestedN1 int
	if err := pool.QueryRow(ctx, `
		SELECT context_refs->0->>'pipeline_run_id',
		       context_refs->0->>'extra_field',
		       context_refs->0->'nested_obj'->>'k',
		       (context_refs->0->'nested_obj'->'n'->>1)::int
		FROM issue WHERE id = $1`, first.IssueID).Scan(&runID, &extra, &nestedK, &nestedN1); err != nil {
		t.Fatalf("read matched element: %v", err)
	}
	if runID != util.UUIDToString(second.RunID) {
		t.Fatalf("matched pipeline_run_id = %q, want %s", runID, util.UUIDToString(second.RunID))
	}
	if extra != "keep-me" || nestedK != "v" || nestedN1 != 2 {
		t.Fatalf("matched element extras = %q/%q/%d, want keep-me/v/2", extra, nestedK, nestedN1)
	}

	// Legacy non-promotion entry: jsonb identity preserved.
	var legacyEq bool
	if err := pool.QueryRow(ctx, `SELECT context_refs->1 = $2::jsonb FROM issue WHERE id = $1`, first.IssueID, legacy).Scan(&legacyEq); err != nil {
		t.Fatalf("compare legacy entry: %v", err)
	}
	if !legacyEq {
		t.Fatalf("legacy entry changed by backfill")
	}

	// Other promotion entry (different source): untouched, no run id.
	var otherRunID, otherDedupe string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(context_refs->2->>'pipeline_run_id', ''), context_refs->2->>'dedupe_key'
		FROM issue WHERE id = $1`, first.IssueID).Scan(&otherRunID, &otherDedupe); err != nil {
		t.Fatalf("read other promotion entry: %v", err)
	}
	if otherDedupe != "other-dedupe" || otherRunID != "" {
		t.Fatalf("unmatched promotion entry mutated: dedupe=%q run=%q", otherDedupe, otherRunID)
	}

	// The promote/dedupe read path still resolves the right source refs.
	if second.SourceRefs.SessionID != f.SessionID || len(second.SourceRefs.MessageIDs) != 1 ||
		second.SourceRefs.MessageIDs[0] != f.MessageID {
		t.Fatalf("source_refs = %+v", second.SourceRefs)
	}
}

func TestPromoteDiscussionForbiddenAndZeroWrites(t *testing.T) {
	pool := newPromotionPool(t)
	ctx := context.Background()
	q := db.New(pool)
	f := seedPromotionFixture(t, pool)
	svc := NewIssueService(q, pool, events.New(), nil, &TaskService{Queries: q})

	// Remove the member: the in-transaction re-check must reject before any
	// write (SDD §4.3 step 8).
	if _, err := pool.Exec(ctx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, f.WorkspaceID, f.UserID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	before := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1`, f.WorkspaceID)
	_, err := svc.PromoteDiscussion(ctx, PromoteDiscussionParams{
		WorkspaceID:    f.WorkspaceID,
		ProjectID:      f.ProjectID,
		SessionID:      f.SessionID,
		CallerID:       f.UserID,
		MessageIDs:     []pgtype.UUID{f.MessageID},
		UpgradeToCR:    true,
		IdempotencyKey: "forbidden-key",
	})
	if !errors.Is(err, ErrPromotionForbidden) {
		t.Fatalf("err = %v, want ErrPromotionForbidden", err)
	}
	if after := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1`, f.WorkspaceID); after != before {
		t.Fatalf("issue rows changed %d → %d on forbidden", before, after)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM chat_idempotency WHERE scope_type = $1 AND scope_id = $2`,
		promotionIdempotencyScope, f.ProjectID); n != 0 {
		t.Fatalf("idempotency rows after forbidden = %d, want 0", n)
	}
}

// TestCreateInTxPromotionRunFailureZeroResidue injects a run-side write
// failure through createInTx (SDD AC-8's injection point) and asserts the
// whole transaction rolls back: no issue, no context_refs, no run/node rows.
func TestCreateInTxPromotionRunFailureZeroResidue(t *testing.T) {
	pool := newPromotionPool(t)
	ctx := context.Background()
	q := db.New(pool)
	f := seedPromotionFixture(t, pool)
	svc := NewIssueService(q, pool, events.New(), nil, &TaskService{Queries: q})

	before := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1`, f.WorkspaceID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)

	// Invalid UTF-8 inputs make the jsonb insert fail at the run write —
	// the same class of failure as a node-insert error (both are wrapped
	// in ErrPromotionRunCreateFailed).
	_, err = svc.createInTx(ctx, tx, qtx, IssueCreateParams{
		WorkspaceID: f.WorkspaceID,
		Title:       "promotion fixture",
		Status:      promotionCreatedStatus,
		Priority:    promotionCreatedPriority,
		CreatorType: "member",
		CreatorID:   f.UserID,
		ProjectID:   f.ProjectID,
		PromotionRun: &PromotionRunPlan{
			RunID:            promoUUID("66666666-6666-6666-6666-666666666666"),
			Inputs:           []byte{0xff, 0xfe}, // invalid UTF-8 → jsonb insert fails
			ExecutionContext: json.RawMessage(`{}`),
		},
	}, IssueCreateOpts{}, IssueCountPolicy{})
	if !errors.Is(err, ErrPromotionRunCreateFailed) {
		t.Fatalf("err = %v, want ErrPromotionRunCreateFailed", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if after := countRows(t, pool, `SELECT count(*) FROM issue WHERE workspace_id = $1`, f.WorkspaceID); after != before {
		t.Fatalf("issue rows changed %d → %d", before, after)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_run WHERE workspace_id = $1`, f.WorkspaceID); n != 0 {
		t.Fatalf("pipeline_run rows = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM pipeline_node_run WHERE run_id = $1`, promoUUID("66666666-6666-6666-6666-666666666666")); n != 0 {
		t.Fatalf("pipeline_node_run rows = %d, want 0", n)
	}
}

// TestPromotionSourcesNeverWriteChatTables is the FR-5 structural guard
// (AC-4): the promotion SQL and service files contain no INSERT/UPDATE/DELETE
// against chat_message / chat_session / attachment.
func TestPromotionSourcesNeverWriteChatTables(t *testing.T) {
	files := []string{
		"promotion.go",
		filepath.Join("..", "..", "pkg", "db", "queries", "promotion.sql"),
	}
	writeChat := regexp.MustCompile(`(?is)\b(insert|update|delete)\b[^;]*\b(chat_message|chat_session|attachment)\b`)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// Strip SQL line comments so prose mentions (e.g. "no writes
		// against chat_message") never match.
		lines := []string{}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			lines = append(lines, line)
		}
		stripped := strings.Join(lines, "\n")
		if m := writeChat.FindString(stripped); m != "" {
			t.Errorf("%s: chat-table write found: %q", f, m)
		}
	}
}

// TestPromotionReplayBodyShape confirms the stored idempotency body parses
// back into the response contract (used by the replay branch).
func TestPromotionReplayBodyShape(t *testing.T) {
	body := []byte(`{"issue_id":"00000000-0000-0000-0000-00000000000a","issue_number":7,"session_id":"00000000-0000-0000-0000-00000000000b","source_refs":{"session_id":"00000000-0000-0000-0000-00000000000b","message_ids":["11111111-1111-1111-1111-111111111111"],"attachment_ids":[]},"created":true,"upgrade_to_cr":false,"run_id":null}`)
	var res PromotionResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Created || res.UpgradeToCR || res.RunID.Valid || res.IssueNumber != 7 {
		t.Fatalf("parsed = %+v", res)
	}
}

// guard against unused imports when DB tests are compiled out of a run —
// pgx is referenced in the DB-backed tests above.
var _ = pgx.ErrNoRows
var _ = uuid.Nil
