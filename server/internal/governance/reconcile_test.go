package governance

// AIFIRST: reconcile tests (CR-2026-002 TASK-07, AC-3). Reuses the crsync_test
// harness (TestMain, postEvents).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// crRow is shared with crsync_test.go.

func seedCR(t *testing.T, crID, status string, needsReconcile bool) {
	t.Helper()
	resetCR(t, crID)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO cr (workspace_id, cr_id, status, projected_commit, needs_reconcile)
		VALUES ($1::uuid, $2, $3, 'seed-sha', $4)`,
		testWorkspaceID, crID, status, needsReconcile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetCR(t, crID) })
}

func TestParseBacklog(t *testing.T) {
	lf := "change-requests:\n  - id: CR-9200-001\n    status: developing\n  - id: CR-9200-002\n    status: drafting\n"
	crlf := "change-requests:\r\n  - id: CR-9200-001\r\n    status: developing\r\n"
	for name, raw := range map[string]string{"lf": lf, "crlf": crlf} {
		m, err := ParseBacklog([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m["CR-9200-001"] != "developing" {
			t.Fatalf("%s: got %v", name, m)
		}
	}
	if _, err := ParseBacklog([]byte("{{not yaml")); err == nil {
		t.Fatal("garbage must be an error, never an empty map (parse-failure hard-fail discipline)")
	}
	if m, err := ParseBacklog([]byte("change-requests: []\n")); err != nil || len(m) != 0 {
		t.Fatalf("empty backlog is valid: %v %v", m, err)
	}
}

// AC-3①/③: a tampered row and a needs_reconcile row both heal to the
// authority; a row matching the authority is untouched (idempotency).
func TestApplySnapshotHeals(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	seedCR(t, "CR-9201-001", "drafting", false)             // tampered: authority says developing
	seedCR(t, "CR-9201-002", "developing", true)            // flagged: status right, flag must clear
	seedCR(t, "CR-9201-003", "requirement-approved", false) // in sync: untouched
	resetCR(t, "CR-9201-004")                               // missing: authority has it
	t.Cleanup(func() { resetCR(t, "CR-9201-004") })

	snap := AuthoritySnapshot{HeadSHA: "auth-sha", Statuses: map[string]string{
		"CR-9201-001": "developing",
		"CR-9201-002": "developing",
		"CR-9201-003": "requirement-approved",
		"CR-9201-004": "drafting",
		"CR-9201-005": "not-a-real-status", // enum guard: never projected
	}}
	healed, err := svc.ApplySnapshot(context.Background(), testWorkspaceID, snap)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 3 {
		t.Fatalf("want 3 healed (tampered + flagged + inserted), got %d", healed)
	}
	if st, nr, pc := crRow(t, "CR-9201-001"); st != "developing" || nr || pc != "auth-sha" {
		t.Fatalf("tampered row not healed: %s %v %s", st, nr, pc)
	}
	if st, nr, _ := crRow(t, "CR-9201-002"); st != "developing" || nr {
		t.Fatalf("flag not cleared: %s %v", st, nr)
	}
	if st, _, pc := crRow(t, "CR-9201-003"); st != "requirement-approved" || pc != "seed-sha" {
		t.Fatalf("in-sync row must be untouched: %s %s", st, pc)
	}
	if st, nr, _ := crRow(t, "CR-9201-004"); st != "drafting" || nr {
		t.Fatalf("missing row not inserted clean: %s %v", st, nr)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr WHERE cr_id = 'CR-9201-005'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unknown status must never be projected (n=%d, err=%v)", n, err)
	}

	// Second application of the same snapshot: nothing left to heal.
	healed, err = svc.ApplySnapshot(context.Background(), testWorkspaceID, snap)
	if err != nil || healed != 0 {
		t.Fatalf("idempotency: want 0 healed on replay, got %d (%v)", healed, err)
	}
}

func TestApplySnapshotSkipsActiveArchitecturePipeline(t *testing.T) {
	const crID = "CR-9201-006"
	seedCR(t, crID, "tech-design-reviewed", false)
	userID := testUserID(t)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, status, started_by)
		VALUES ($1::uuid, $2, $3, 'running', $4::uuid)`,
		testWorkspaceID, PipelineIDs.ArchitectureDesign, crID, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM pipeline_run WHERE cr_id = $1`, crID)
	})

	svc := NewSyncService(testPool, nil)
	stale := AuthoritySnapshot{HeadSHA: "root-stale", Statuses: map[string]string{crID: "requirement-approved"}}
	healed, err := svc.ApplySnapshot(context.Background(), testWorkspaceID, stale)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 0 {
		t.Fatalf("active pipeline snapshot must not heal live CR, healed=%d", healed)
	}
	if status, _, _ := crRow(t, crID); status != "tech-design-reviewed" {
		t.Fatalf("stale snapshot overwrote live projection: %s", status)
	}

	if _, err := testPool.Exec(context.Background(), `
		UPDATE pipeline_run SET status = 'completed', completed_at = now() WHERE cr_id = $1`, crID); err != nil {
		t.Fatal(err)
	}
	healed, err = svc.ApplySnapshot(context.Background(), testWorkspaceID, stale)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 1 {
		t.Fatalf("completed pipeline should allow snapshot healing, healed=%d", healed)
	}
	if status, _, _ := crRow(t, crID); status != "requirement-approved" {
		t.Fatalf("completed pipeline snapshot did not heal: %s", status)
	}
}

// Rows absent from the snapshot (archived CRs moved to _history) stay as-is.
func TestApplySnapshotLeavesAbsentRows(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	seedCR(t, "CR-9202-001", "archived", false)
	healed, err := svc.ApplySnapshot(context.Background(), testWorkspaceID,
		AuthoritySnapshot{HeadSHA: "h", Statuses: map[string]string{}})
	if err != nil || healed != 0 {
		t.Fatalf("want 0 healed, got %d (%v)", healed, err)
	}
	if st, _, _ := crRow(t, "CR-9202-001"); st != "archived" {
		t.Fatalf("absent-from-snapshot row must be untouched, got %s", st)
	}
}

// Daemon mode end to end at the server boundary: a snapshot event through
// POST /cr-events heals a tampered row (AC-3② daemon half).
func TestSnapshotEventHeals(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	seedCR(t, "CR-9203-001", "drafting", false)
	backlog := "change-requests:\n  - id: CR-9203-001\n    status: developing\n"
	payload, _ := json.Marshal(map[string]string{"head_sha": "snap-sha", "backlog": backlog})
	ev := OutboxEvent{
		V: 1, File: "snapshot:snap-sha", EventKind: "snapshot",
		Actor: "daemon-reconcile", Payload: payload, OccurredAt: time.Now(),
	}
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Accepted) != 1 {
		t.Fatalf("want accepted, got %+v", resp)
	}
	if st, nr, pc := crRow(t, "CR-9203-001"); st != "developing" || nr || pc != "snap-sha" {
		t.Fatalf("snapshot event did not heal: %s %v %s", st, nr, pc)
	}
	// The ledger stays clean — snapshots bypass cr_sync_event.
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr_sync_event WHERE event_kind = 'snapshot'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("snapshot must not enter the ledger (n=%d, err=%v)", n, err)
	}
	// A corrupt snapshot payload is rejected, not half-applied.
	bad := OutboxEvent{V: 1, File: "snapshot:bad", EventKind: "snapshot",
		Payload: json.RawMessage(`{"backlog": "{{not yaml"}`), OccurredAt: time.Now()}
	resp = postEvents(t, svc, testWorkspaceID, []OutboxEvent{bad})
	if len(resp.Rejected) != 1 || resp.Rejected[0].Code != "INGEST_FAILED" {
		t.Fatalf("corrupt snapshot must reject, got %+v", resp)
	}
}

// ── CR-2026-003 tests ────────────────────────────────────────────────────────
// "pending:" is the cross-language contract literal with crctl pendingCommitSha()
// (tools crctl.test.mjs locks the same literal on the JS side).

// AC-1 (FR-1): two embedded status events for the same CR must both land in the
// ledger and both advance the projection — and the placeholder must never leak
// into cr.projected_commit.
func TestEmbeddedPlaceholderEventsDoNotCollide(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	resetCR(t, "CR-9301-100")
	t.Cleanup(func() { resetCR(t, "CR-9301-100") })

	// Seed via a real-sha registration, then two placeholder transitions
	// (mirrors writing-back -> archived, the sequence that was silently lost).
	evs := []OutboxEvent{
		{V: 1, File: "e0.json", EventKind: "status", CRID: "CR-9301-100",
			FromStatus: "", ToStatus: "drafting", Trigger: "requirement-register",
			CommitSHA: "realsha0", OccurredAt: time.Now()},
		{V: 1, File: "e1.json", EventKind: "status", CRID: "CR-9301-100",
			FromStatus: "drafting", ToStatus: "requirement-reviewing", Trigger: "review-requirement",
			CommitSHA: "pending:1753900000000:11111:1", OccurredAt: time.Now()},
		{V: 1, File: "e2.json", EventKind: "status", CRID: "CR-9301-100",
			FromStatus: "requirement-reviewing", ToStatus: "requirement-approved", Trigger: "approve-requirement",
			CommitSHA: "pending:1753900000000:11111:2", OccurredAt: time.Now()},
	}
	resp := postEvents(t, svc, testWorkspaceID, evs)
	if len(resp.Accepted) != 3 {
		t.Fatalf("want 3 accepted, got %+v", resp)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr_sync_event WHERE cr_id = 'CR-9301-100' AND event_kind = 'status'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("idempotency collision: want 3 ledger rows, got %d (the pre-fix bug collapsed placeholders)", n)
	}
	st, nr, pc := crRow(t, "CR-9301-100")
	if st != "requirement-approved" || nr {
		t.Fatalf("second placeholder transition lost: status=%s needs_reconcile=%v", st, nr)
	}
	if strings.HasPrefix(pc, "pending:") {
		t.Fatalf("placeholder leaked into projected_commit: %q", pc)
	}
	if pc != "realsha0" {
		t.Fatalf("projected_commit should keep the last real sha, got %q", pc)
	}
	// NFR-1: real-sha dedup behavior unchanged — replaying e0 is still a no-op.
	resp = postEvents(t, svc, testWorkspaceID, evs[:1])
	if len(resp.Accepted) != 1 {
		t.Fatalf("replay must still be acked: %+v", resp)
	}
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr_sync_event WHERE cr_id = 'CR-9301-100' AND event_kind = 'status'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("real-sha replay must dedup (NFR-1), got %d rows", n)
	}
}

func TestParseHistory(t *testing.T) {
	lf := "history:\n  - id: CR-9300-001\n    final-status: archived\n  - id: CR-9300-002\n    final-status: rejected\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	for name, raw := range map[string]string{"lf": lf, "crlf": crlf} {
		m, err := ParseHistory([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m["CR-9300-001"] != "archived" || m["CR-9300-002"] != "rejected" {
			t.Fatalf("%s: got %v", name, m)
		}
	}
	if m, err := ParseHistory(nil); err != nil || len(m) != 0 {
		t.Fatalf("empty history is valid (never archived): %v %v", m, err)
	}
	if _, err := ParseHistory([]byte("{{not yaml")); err == nil {
		t.Fatal("garbage must hard-fail, never silently empty (工程纪律 1)")
	}
}

func TestMergeAuthorityBacklogWins(t *testing.T) {
	m := mergeAuthority(map[string]string{"CR-A": "developing"}, map[string]string{"CR-A": "archived", "CR-B": "archived"})
	if m["CR-A"] != "developing" || m["CR-B"] != "archived" || len(m) != 2 {
		t.Fatalf("backlog must win on overlap: %v", m)
	}
}

// AC-2 (FR-2): an archived CR whose projection is stuck heals from the merged
// snapshot — the exact shape CR-2026-001/002 were stuck in.
func TestArchivedCRHealsFromHistorySnapshot(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	seedCR(t, "CR-9302-001", "writing-back", true) // the real-world stuck shape
	backlog := "change-requests:\n"
	history := "history:\n  - id: CR-9302-001\n    final-status: archived\n"
	payload, _ := json.Marshal(map[string]string{"head_sha": "hist-sha", "backlog": backlog, "history": history})
	ev := OutboxEvent{V: 1, File: "snapshot:hist", EventKind: "snapshot",
		Actor: "daemon-reconcile", Payload: payload, OccurredAt: time.Now()}
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Accepted) != 1 {
		t.Fatalf("want accepted, got %+v", resp)
	}
	if st, nr, _ := crRow(t, "CR-9302-001"); st != "archived" || nr {
		t.Fatalf("archived CR did not heal from history: %s/%v", st, nr)
	}
	// Backward compat: a snapshot without the history field behaves pre-fix.
	seedCR(t, "CR-9302-002", "writing-back", true)
	payload2, _ := json.Marshal(map[string]string{"head_sha": "h2", "backlog": backlog})
	ev2 := OutboxEvent{V: 1, File: "snapshot:nohist", EventKind: "snapshot",
		Actor: "daemon-reconcile", Payload: payload2, OccurredAt: time.Now()}
	if resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev2}); len(resp.Accepted) != 1 {
		t.Fatalf("old-daemon snapshot must still be accepted: %+v", resp)
	}
	if st, _, _ := crRow(t, "CR-9302-002"); st != "writing-back" {
		t.Fatalf("no-history snapshot must not touch the row (compat): %s", st)
	}
}
