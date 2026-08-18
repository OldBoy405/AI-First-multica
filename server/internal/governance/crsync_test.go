package governance

// AIFIRST: integration tests for the CR projection sync worker (CR-2026-002
// TASK-05, AC-2). Follows the handler package convention: connect to the local
// dev database (or DATABASE_URL), skip gracefully when unreachable. Requires
// migration 158 to be applied.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/middleware"
)

var (
	testPool        *pgxpool.Pool
	testWorkspaceID string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Printf("Skipping governance integration tests: database not reachable: %v\n", err)
		os.Exit(0)
	}
	testPool = pool
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug) VALUES ('governance-tests', 'governance-tests')
		ON CONFLICT (slug) DO UPDATE SET updated_at = now()
		RETURNING id::text`).Scan(&testWorkspaceID); err != nil {
		fmt.Printf("Failed to set up governance test workspace: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	code := m.Run()
	cleanup(ctx, pool)
	pool.Close()
	os.Exit(code)
}

func cleanup(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DELETE FROM cr_sync_event WHERE cr_id LIKE 'CR-9%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM cr WHERE workspace_id = $1::uuid`, testWorkspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspace WHERE slug = 'governance-tests'`)
}

func resetCR(t *testing.T, crID string) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(), `DELETE FROM cr_sync_event WHERE cr_id = $1`, crID)
	_, _ = testPool.Exec(context.Background(), `DELETE FROM cr WHERE cr_id = $1`, crID)
	// CR-2026-011 TASK-04: pre-existing gap — approval_test.go's tests each
	// mint a fresh ed25519 keypair (newTestApprovalService) but
	// approval_record's idempotency key is process-independent (cr_id, stage,
	// evidence_digest). Without this cleanup, a second run against a
	// persistent (non-ephemeral) DB hits the first run's stale grant, signed
	// by a key this run's public key can no longer verify against.
	_, _ = testPool.Exec(context.Background(), `DELETE FROM approval_record WHERE cr_id = $1`, crID)
}

func postEvents(t *testing.T, svc *SyncService, workspaceID string, evs []OutboxEvent) crEventsResponse {
	t.Helper()
	body, _ := json.Marshal(crEventsRequest{WorkspaceRootHash: "test-hash", Events: evs})
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/cr-events", bytes.NewReader(body))
	if workspaceID != "" {
		req = req.WithContext(middleware.WithDaemonContext(req.Context(), workspaceID, "daemon-test"))
	}
	rec := httptest.NewRecorder()
	svc.HandleCREvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cr-events returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp crEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	return resp
}

func ev(crID, kind, from, to, trigger, sha, file string) OutboxEvent {
	return OutboxEvent{
		V: 1, File: file, EventKind: kind, CRID: crID,
		FromStatus: from, ToStatus: to, Trigger: trigger, CommitSHA: sha,
		Actor: "tester", OccurredAt: time.Now(),
	}
}

func crRow(t *testing.T, crID string) (status string, needsReconcile bool, projected string) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT status, needs_reconcile, projected_commit FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`,
		testWorkspaceID, crID).Scan(&status, &needsReconcile, &projected)
	if err != nil {
		t.Fatalf("cr row not found for %s: %v", crID, err)
	}
	return
}

func TestLegalFlowProjectsAndBroadcasts(t *testing.T) {
	crID := "CR-9001-001"
	resetCR(t, crID)
	bus := events.New()
	var mu sync.Mutex
	var received []events.Event
	bus.Subscribe(EventCRUpdated, func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	svc := NewSyncService(testPool, bus)

	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "aaa1111", "f1.json"),
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "bbb2222", "f2.json"),
	})
	if len(resp.Accepted) != 2 || len(resp.Rejected) != 0 {
		t.Fatalf("want 2 accepted / 0 rejected, got %+v", resp)
	}
	status, reconcile, projected := crRow(t, crID)
	if status != "requirement-reviewing" || reconcile || projected != "bbb2222" {
		t.Fatalf("projection wrong: status=%s reconcile=%v projected=%s", status, reconcile, projected)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("want 2 cr:updated WS events, got %d", len(received))
	}
	if received[0].WorkspaceID != testWorkspaceID {
		t.Fatalf("WS event workspace scope wrong: %s", received[0].WorkspaceID)
	}
}

func TestDualChannelSameEventLandsOnce(t *testing.T) {
	crID := "CR-9001-002"
	resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	e := ev(crID, "status", "", "drafting", "requirement-register", "ccc3333", "outbox.json")
	resp1 := postEvents(t, svc, testWorkspaceID, []OutboxEvent{e})
	// Same (cr_id, commit_sha, event_kind) arriving via the commit-scan channel.
	e2 := e
	e2.File = "commit-scan.json"
	resp2 := postEvents(t, svc, testWorkspaceID, []OutboxEvent{e2})

	// Both deliveries are ACKed (daemon must delete both source files) …
	if len(resp1.Accepted) != 1 || len(resp2.Accepted) != 1 {
		t.Fatalf("both deliveries must be accepted: %+v / %+v", resp1, resp2)
	}
	// … but the ledger keeps exactly one row (AC-2 idempotency).
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr_sync_event WHERE cr_id = $1`, crID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("want exactly 1 ledger row, got %d (err=%v)", n, err)
	}
}

func TestReviewEventPersistsEvidence(t *testing.T) {
	const crID = "CR-9001-008"
	seedCR(t, crID, "tech-design-review-pending", false)
	svc := NewSyncService(testPool, nil)
	review := ev(crID, "review", "", "", "", "review-sha-008", "review.json")
	review.Evidence = map[string]string{
		"change-requests/CR-9001-008/review-annotations/sdd.yml": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	review.Payload = json.RawMessage(`{"stage":"tech-design","verdict":"pass","attempt":1,"blockers":[],"reviewed_at":"2026-08-18T10:00:00Z","subject_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{review})
	if len(resp.Accepted) != 1 || len(resp.Rejected) != 0 {
		t.Fatalf("review event rejected: %+v", resp)
	}
	var evidence map[string]string
	if err := testPool.QueryRow(context.Background(), `
		SELECT evidence FROM cr_sync_event
		WHERE cr_id = $1 AND commit_sha = $2 AND event_kind = 'review'`,
		crID, review.CommitSHA).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if got := evidence["change-requests/CR-9001-008/review-annotations/sdd.yml"]; got != review.Evidence["change-requests/CR-9001-008/review-annotations/sdd.yml"] {
		t.Fatalf("review evidence not persisted: %q", got)
	}
}

func TestOutOfOrderFlagsReconcileWithoutCorruptingProjection(t *testing.T) {
	crID := "CR-9001-003"
	resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "d1", "f1.json"),
	})
	// Out-of-order: claims to come from a status the projection is not in.
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "developing", "code-reviewing", "review-code", "d2", "f2.json"),
	})
	status, reconcile, _ := crRow(t, crID)
	if status != "drafting" {
		t.Fatalf("projection must not be forced: status=%s", status)
	}
	if !reconcile {
		t.Fatal("needs_reconcile must be set on out-of-order events")
	}
}

func TestIllegalTransitionFlagsReconcile(t *testing.T) {
	crID := "CR-9001-004"
	resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "e1", "f1.json"),
	})
	// From matches but the transition is not in the state machine.
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "drafting", "code-approved", "approve-code", "e2", "f2.json"),
	})
	status, reconcile, _ := crRow(t, crID)
	if status != "drafting" || !reconcile {
		t.Fatalf("illegal transition must flag reconcile and keep status: status=%s reconcile=%v", status, reconcile)
	}
}

func TestCheckpointFillsProjectedCommitForEmbedded(t *testing.T) {
	crID := "CR-9001-005"
	resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	// --embedded status event: empty commit_sha.
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "", "f1.json"),
	})
	_, _, projected := crRow(t, crID)
	if projected != "" {
		t.Fatalf("projected_commit should start empty, got %s", projected)
	}
	// The push checkpoint completes the pointer (source design §A.5).
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "checkpoint", "", "", "", "fff9999", "f2.json"),
	})
	_, _, projected = crRow(t, crID)
	if projected != "fff9999" {
		t.Fatalf("checkpoint must fill projected_commit, got %q", projected)
	}
}

func TestRejectionPaths(t *testing.T) {
	crID := "CR-9001-006"
	resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	// No daemon context (e.g. a task token never passes DaemonAuth; even if a
	// request reached the handler, missing workspace binding is a hard 403).
	body, _ := json.Marshal(crEventsRequest{Events: []OutboxEvent{ev(crID, "status", "", "drafting", "requirement-register", "x", "f.json")}})
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/cr-events", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.HandleCREvents(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing daemon context must be 403, got %d", rec.Code)
	}

	// Malformed events are rejected per-item with a code, batch still 200.
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		{V: 2, File: "bad-version.json", EventKind: "status", CRID: crID, OccurredAt: time.Now()},
		{V: 1, File: "bad-kind.json", EventKind: "nonsense", CRID: crID, OccurredAt: time.Now()},
		{V: 1, File: "bad-id.json", EventKind: "status", CRID: "OOPS-1", OccurredAt: time.Now()},
	})
	if len(resp.Accepted) != 0 || len(resp.Rejected) != 3 {
		t.Fatalf("want 0 accepted / 3 rejected, got %+v", resp)
	}
	codes := map[string]string{}
	for _, r := range resp.Rejected {
		codes[r.File] = r.Code
	}
	if codes["bad-version.json"] != "BAD_EVENT" || codes["bad-kind.json"] != "UNKNOWN_KIND" || codes["bad-id.json"] != "BAD_EVENT" {
		t.Fatalf("unexpected rejection codes: %v", codes)
	}

	// Oversized batch is a hard 400.
	big := make([]OutboxEvent, MaxEventsPerBatch+1)
	for i := range big {
		big[i] = ev(crID, "status", "", "drafting", "requirement-register", fmt.Sprintf("s%d", i), fmt.Sprintf("f%d.json", i))
	}
	body, _ = json.Marshal(crEventsRequest{Events: big})
	req = httptest.NewRequest(http.MethodPost, "/api/daemon/cr-events", bytes.NewReader(body))
	req = req.WithContext(middleware.WithDaemonContext(req.Context(), testWorkspaceID, "daemon-test"))
	rec = httptest.NewRecorder()
	svc.HandleCREvents(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch must be 400, got %d", rec.Code)
	}
}

// AIFIRST: TestArchiveEventCompletesWritebackRun is the CR-2026-032 TASK-04
// test-only contract against the existing production consumer. The schema v1
// `archive` event crctl emits on a normal writing-back -> archived archive
// must project the terminal status, point projected_commit at the final
// archive SHA, complete the feature-writeback pipeline run, and stay
// idempotent under the (cr_id, commit_sha, event_kind) ledger key. No
// production code, migration, or schema change is part of this test.
func TestArchiveEventCompletesWritebackRun(t *testing.T) {
	crID := "CR-9001-007"
	ctx := context.Background()
	// This test owns pipeline projection rows too; crsync fixtures only reset
	// cr/cr_sync_event, so clean run rows first (same pattern as
	// gate_projection_test.go's resetPipelineState).
	_, _ = testPool.Exec(ctx, `DELETE FROM pipeline_node_run WHERE run_id IN (SELECT id FROM pipeline_run WHERE cr_id = $1)`, crID)
	_, _ = testPool.Exec(ctx, `DELETE FROM pipeline_run WHERE cr_id = $1`, crID)
	resetCR(t, crID)
	ensureTestWorkspaceOwner(t)

	// Seed the projection in writing-back with an active feature-writeback run.
	_, _ = testPool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, status, projected_commit, needs_reconcile)
		VALUES ($1::uuid, $2, 'writing-back', '', false)
		ON CONFLICT (workspace_id, cr_id) DO UPDATE
		  SET status = 'writing-back', projected_commit = '', needs_reconcile = false`,
		testWorkspaceID, crID)
	var ownerID string
	if err := testPool.QueryRow(ctx,
		`SELECT user_id::text FROM member WHERE workspace_id = $1::uuid AND role = 'owner' LIMIT 1`,
		testWorkspaceID).Scan(&ownerID); err != nil {
		t.Fatalf("test owner member missing: %v", err)
	}
	_, _ = testPool.Exec(ctx, `
		INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, status, started_by)
		VALUES ($1::uuid, $2, $3, 'running', $4::uuid)`,
		testWorkspaceID, PipelineIDs.FeatureWriteback, crID, ownerID)

	// Fixed real-looking SHA, matching the deterministic dedup file name
	// archive-<CR>-<sha>.json that crctl writes.
	const sha = "aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44"
	svc := NewSyncService(testPool, nil)
	e := ev(crID, "archive", "writing-back", "archived", "cr-archive", sha, "archive.json")

	// First ingest: known kind, legal transition, terminal projection.
	resp1 := postEvents(t, svc, testWorkspaceID, []OutboxEvent{e})
	if len(resp1.Accepted) != 1 || len(resp1.Rejected) != 0 {
		t.Fatalf("archive event must be accepted, got %+v", resp1)
	}
	status, reconcile, projected := crRow(t, crID)
	if status != "archived" || reconcile || projected != sha {
		t.Fatalf("archive projection wrong: status=%s reconcile=%v projected=%s", status, reconcile, projected)
	}
	if runStatus(t, crID, PipelineIDs.FeatureWriteback) != "completed" {
		t.Fatalf("feature-writeback run must complete on archive")
	}
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM cr_sync_event WHERE cr_id = $1 AND commit_sha = $2 AND event_kind = $3`,
		crID, sha, "archive").Scan(&n); err != nil || n != 1 {
		t.Fatalf("want exactly 1 ledger row for archive key, got %d (err=%v)", n, err)
	}

	// Replay the same event (redelivery window after journal re-mark): the
	// unique ledger key dedups it and neither projection nor run state moves.
	resp2 := postEvents(t, svc, testWorkspaceID, []OutboxEvent{e})
	if len(resp2.Accepted) != 1 || len(resp2.Rejected) != 0 {
		t.Fatalf("replayed archive event must be accepted (dedup on key), got %+v", resp2)
	}
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM cr_sync_event WHERE cr_id = $1 AND commit_sha = $2 AND event_kind = $3`,
		crID, sha, "archive").Scan(&n); err != nil || n != 1 {
		t.Fatalf("replay must not add ledger rows, got %d (err=%v)", n, err)
	}
	status, reconcile, projected = crRow(t, crID)
	if status != "archived" || reconcile || projected != sha {
		t.Fatalf("replay changed projection: status=%s reconcile=%v projected=%s", status, reconcile, projected)
	}
	if runStatus(t, crID, PipelineIDs.FeatureWriteback) != "completed" {
		t.Fatal("replay must not reopen the writeback run")
	}
}
