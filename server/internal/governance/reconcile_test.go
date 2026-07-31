package governance

// AIFIRST: reconcile tests (CR-2026-002 TASK-07, AC-3). Reuses the crsync_test
// harness (TestMain, postEvents).

import (
	"context"
	"encoding/json"
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
