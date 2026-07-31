package governance

// AIFIRST: PAT-fallback workspace binding tests (CR-2026-002 TASK-11 defect
// fix): mdt_ minting is not live upstream, so real daemons hit the governance
// endpoints via the PAT path — binding must come from the member table.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seedMemberUser(t *testing.T, email string) string {
	t.Helper()
	var userID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO "user" (email, name) VALUES ($1, 'governance-test-user')
		ON CONFLICT (email) DO UPDATE SET updated_at = now()
		RETURNING id::text`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1::uuid, $2::uuid, 'member')
		ON CONFLICT (workspace_id, user_id) DO NOTHING`, testWorkspaceID, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1::uuid`, userID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1::uuid`, userID)
	})
	return userID
}

func postEventsAsPAT(t *testing.T, svc *SyncService, userID, workspaceHeader string, evs []OutboxEvent) (int, crEventsResponse) {
	t.Helper()
	body, _ := json.Marshal(crEventsRequest{Events: evs})
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/cr-events", bytes.NewReader(body))
	if userID != "" {
		req.Header.Set("X-User-ID", userID) // server-set by DaemonAuth PAT path in production
	}
	if workspaceHeader != "" {
		req.Header.Set("X-Workspace-ID", workspaceHeader)
	}
	rec := httptest.NewRecorder()
	svc.HandleCREvents(rec, req)
	var resp crEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestPATFallbackBindsSingleMembership(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	userID := seedMemberUser(t, "governance-pat@test.local")
	resetCR(t, "CR-9301-001")
	t.Cleanup(func() { resetCR(t, "CR-9301-001") })

	ev := OutboxEvent{V: 1, File: "t11-pat.json", EventKind: "status", CRID: "CR-9301-001",
		ToStatus: "drafting", Trigger: "requirement-register", CommitSHA: "patsha", OccurredAt: time.Now()}
	code, resp := postEventsAsPAT(t, svc, userID, "", []OutboxEvent{ev})
	if code != http.StatusOK || len(resp.Accepted) != 1 {
		t.Fatalf("single-membership PAT daemon must bind implicitly: code=%d resp=%+v", code, resp)
	}
	if st, _, _ := crRow(t, "CR-9301-001"); st != "drafting" {
		t.Fatalf("event not projected under the member workspace: %s", st)
	}
}

func TestPATFallbackRejectsOutsiders(t *testing.T) {
	svc := NewSyncService(testPool, nil)
	// No X-User-ID at all (e.g. auth path that sets nothing): reject.
	code, _ := postEventsAsPAT(t, svc, "", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("no identity must stay 403, got %d", code)
	}
	// A member asking for a workspace they do not belong to: reject.
	userID := seedMemberUser(t, "governance-pat2@test.local")
	code, _ = postEventsAsPAT(t, svc, userID, "00000000-0000-0000-0000-000000000001", nil)
	if code != http.StatusForbidden {
		t.Fatalf("non-membership workspace selection must be 403, got %d", code)
	}
}
