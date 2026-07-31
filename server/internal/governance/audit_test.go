package governance

// AIFIRST: audit event ingestion tests (CR-2026-002 TASK-10, AC-6① / AC-7③
// evidence half). Reuses the crsync_test harness (TestMain, postEvents).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

func fetchAuditRows(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT details FROM activity_log
		WHERE workspace_id = $1::uuid AND action = $2 AND issue_id IS NULL
		ORDER BY created_at`, testWorkspaceID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func clearAuditRows(t *testing.T) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM activity_log WHERE workspace_id = $1::uuid AND action LIKE 'aifirst.%'`, testWorkspaceID)
}

func auditEvent(crID string, payload map[string]any) OutboxEvent {
	raw, _ := json.Marshal(payload)
	return OutboxEvent{
		V: 1, File: "t10-" + crID + "-audit.json", EventKind: "audit",
		CRID: crID, Actor: "test", Payload: raw, OccurredAt: time.Now(),
	}
}

// AC-6①: a gitguard denial reaches activity_log with only countable facts —
// caller, subcommand, error code — and never argument bodies.
func TestAuditGitguardDenied(t *testing.T) {
	clearAuditRows(t)
	svc := NewSyncService(testPool, nil)
	ev := auditEvent("", map[string]any{
		"action": ActionGitguardDenied, "caller": "agent-42", "sub": "push", "code": "FORBIDDEN_SUBCOMMAND",
	})
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Accepted) != 1 {
		t.Fatalf("want accepted, got %+v", resp)
	}
	rows := fetchAuditRows(t, ActionGitguardDenied)
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(rows))
	}
	d := rows[0]
	if d["caller"] != "agent-42" || d["sub"] != "push" || d["code"] != "FORBIDDEN_SUBCOMMAND" {
		t.Fatalf("details mismatch: %v", d)
	}
	if _, has := d["args"]; has {
		t.Fatal("details must never carry argument bodies")
	}
	// No cr row, no ledger row: audit bypasses the projection entirely.
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cr_sync_event WHERE event_kind = 'audit'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("audit events must not enter cr_sync_event, found %d", n)
	}
}

// AC-7③ evidence half: post-approval drift detected by crctl lands as an
// evidence_drift row carrying digests only, plus the CR binding.
func TestAuditEvidenceDrift(t *testing.T) {
	clearAuditRows(t)
	svc := NewSyncService(testPool, nil)
	ev := auditEvent("CR-9102-001", map[string]any{
		"action": ActionEvidenceDrift, "stage": "tech-design",
		"expected_digest": strings.Repeat("a", 16), "actual_digest": strings.Repeat("b", 16),
		"detected_at": "2026-07-31T12:00:00+08:00",
	})
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Accepted) != 1 {
		t.Fatalf("want accepted, got %+v", resp)
	}
	rows := fetchAuditRows(t, ActionEvidenceDrift)
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(rows))
	}
	d := rows[0]
	if d["cr_id"] != "CR-9102-001" || d["stage"] != "tech-design" {
		t.Fatalf("details mismatch: %v", d)
	}
	if d["expected_digest"] == d["actual_digest"] {
		t.Fatalf("digests should differ in a drift record: %v", d)
	}
}

// A forged outbox file cannot mint arbitrary activity rows: unknown actions
// are rejected (daemon dead-letters after 3 strikes).
func TestAuditUnknownActionRejected(t *testing.T) {
	clearAuditRows(t)
	svc := NewSyncService(testPool, nil)
	ev := auditEvent("", map[string]any{"action": "issue_deleted", "sneaky": true})
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Rejected) != 1 || resp.Rejected[0].Code != "INGEST_FAILED" {
		t.Fatalf("want INGEST_FAILED rejection, got %+v", resp)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'issue_deleted'`,
		testWorkspaceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("forged action must not be written")
	}
}

// Non-audit kinds still require a well-formed CR id (the relaxation is scoped).
func TestAuditCRIDRelaxationScoped(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	ev := OutboxEvent{V: 1, File: "t10-bad.json", EventKind: "status", ToStatus: "drafting", OccurredAt: time.Now()}
	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{ev})
	if len(resp.Rejected) != 1 || resp.Rejected[0].Code != "BAD_EVENT" {
		t.Fatalf("status event without cr_id must stay BAD_EVENT, got %+v", resp)
	}
}

// The Go spool constant and the governance action constant must never drift —
// gitguard writes the string, this package gates on it.
func TestAuditActionConstantsAligned(t *testing.T) {
	if gitguard.ActionDenied != ActionGitguardDenied {
		t.Fatalf("gitguard.ActionDenied %q != governance.ActionGitguardDenied %q",
			gitguard.ActionDenied, ActionGitguardDenied)
	}
}
