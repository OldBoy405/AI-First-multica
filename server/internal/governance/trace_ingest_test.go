package governance

// AIFIRST: CR-2026-049 TASK-06 — trace event ingest tests (SDD §3.1, TD-B3).
// Live-PG suite (same harness as crsync_test.go): accepted trace → one committed
// processed row and no cr.status change; same-key replay is idempotent; same-key
// conflicting payload is rejected with EVENT_IDEMPOTENCY_CONFLICT; bad/oversized
// payloads are rejected with their codes and the file id is never acked.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/middleware"
)

func validTracePayload(crID string) json.RawMessage {
	return json.RawMessage(`{"spec_id":"test-spec","traceability":{"spec-id":"test-spec","cr-ref":"` + crID + `","cr-history":["CR-2026-001","` + crID + `"],"target-version":"0.23","baseline-since":"0.10","generated-at":"2026-08-20T20:00:00+08:00","milestones":[{"cr":"CR-2026-001","milestone":"M0","frs":[]},{"cr":"` + crID + `","milestone":"M3","frs":[{"fr":"FR-1"}],"evidence":{"test":{"status":"pass"}}}]}}`)
}

func traceEvent(crID, sha, file string, payload json.RawMessage) OutboxEvent {
	return OutboxEvent{
		V: 1, File: file, EventKind: "trace", CRID: crID,
		CommitSHA: sha, Actor: "tester", OccurredAt: time.Now(), Payload: payload,
	}
}

func postEventsRaw(t *testing.T, svc *SyncService, workspaceID string, evs []OutboxEvent) crEventsResponse {
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

func TestTraceIngestAcceptedProcessedAndLedgerOnly(t *testing.T) {
	crID := "CR-9999-001"
	resetCR(t, crID)
	defer resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	// Seed a cr row so "status unchanged" is meaningful.
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID); err != nil {
		t.Fatal(err)
	}
	resp := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-1", "trace-1.json", validTracePayload(crID))})
	if len(resp.Accepted) != 1 || len(resp.Rejected) != 0 {
		t.Fatalf("want 1 accepted / 0 rejected, got %+v", resp)
	}
	var n int
	var processedAt *time.Time
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*), min(processed_at) FROM cr_sync_event
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND event_kind = 'trace'`,
		testWorkspaceID, crID).Scan(&n, &processedAt); err != nil {
		t.Fatal(err)
	}
	if n != 1 || processedAt == nil {
		t.Fatalf("trace row: n=%d processed=%v", n, processedAt)
	}
	status, _, _ := crRow(t, crID)
	if status != "developing" {
		t.Fatalf("trace must be ledger-only; cr.status = %s", status)
	}
}

func TestTraceIngestIdempotentReplayAndConflictReject(t *testing.T) {
	crID := "CR-9999-002"
	resetCR(t, crID)
	defer resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	payload := validTracePayload(crID)
	first := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-2", "trace-2.json", payload)})
	if len(first.Accepted) != 1 {
		t.Fatalf("first ingest: %+v", first)
	}
	// Same key, same payload → idempotent accepted, still one row.
	second := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-2", "trace-2.json", payload)})
	if len(second.Accepted) != 1 || len(second.Rejected) != 0 {
		t.Fatalf("idempotent replay must be accepted: %+v", second)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM cr_sync_event
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND event_kind = 'trace' AND commit_sha = 'trace-sha-2'`,
		testWorkspaceID, crID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("trace rows after replay = %d, want 1", n)
	}
	// Same key, different payload → rejected with the conflict code, file id kept out of Accepted.
	conflict := json.RawMessage(strings.Replace(string(payload), `"M3"`, `"M3-conflict"`, 1))
	third := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-2", "trace-2.json", conflict)})
	if len(third.Accepted) != 0 || len(third.Rejected) != 1 || third.Rejected[0].Code != "EVENT_IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict replay must be rejected with EVENT_IDEMPOTENCY_CONFLICT: %+v", third)
	}
	if third.Rejected[0].File != "trace-2.json" {
		t.Fatalf("rejected file id = %q", third.Rejected[0].File)
	}
}

func TestTraceIngestRejectsBadAndOversizedPayload(t *testing.T) {
	crID := "CR-9999-003"
	resetCR(t, crID)
	defer resetCR(t, crID)
	svc := NewSyncService(testPool, nil)

	// BAD_TRACE_PAYLOAD: traceability cr-ref mismatch (milestone CR appears twice).
	bad := json.RawMessage(`{"spec_id":"test-spec","traceability":{"spec-id":"test-spec","cr-ref":"` + crID + `","milestones":[{"cr":"CR-2026-001"},{"cr":"CR-2026-001"}]}}`)
	resp := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-3", "bad.json", bad)})
	if len(resp.Accepted) != 0 || len(resp.Rejected) != 1 || resp.Rejected[0].Code != "BAD_TRACE_PAYLOAD" {
		t.Fatalf("bad payload: %+v", resp)
	}
	// Oversized: 2 MiB + 1 → validateEvent rejects before ingest.
	big := json.RawMessage(`{"spec_id":"x","traceability":{},"pad":"` + strings.Repeat("a", MaxTracePayloadBytes) + `"}`)
	resp = postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-sha-4", "big.json", big)})
	if len(resp.Accepted) != 0 || len(resp.Rejected) != 1 || resp.Rejected[0].Code != "TRACE_PAYLOAD_TOO_LARGE" {
		t.Fatalf("oversized payload: %+v", resp)
	}
	// Nothing landed for either rejected file.
	var n int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM cr_sync_event
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND event_kind = 'trace'`,
		testWorkspaceID, crID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected payloads must not land: rows=%d", n)
	}
}
