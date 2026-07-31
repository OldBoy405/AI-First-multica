package governance

// AIFIRST: tests for signed approvals (CR-2026-002 TASK-08, AC-4). The digest
// parity test runs standalone; the HTTP tests need the shared test database
// (same convention as crsync_test.go — TestMain there skips when unreachable).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestCanonicalDigestParityWithSharedVectors locks the Go implementation to
// the exact bytes of crctl's canonicalEvidenceDigest (AC-4②/AC-7⑤ cross-
// language half). Source of truth: tools .../test/fixtures/digest-vectors.
func TestCanonicalDigestParityWithSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "digest-vectors", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Files         []string `json:"files"`
		PerFileSha256 []string `json:"perFileSha256"`
		Digest        string   `json:"digest"`
	}
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	// Recompute per-file hashes from the fixture contents (EOL-normalized),
	// proving the whole chain — not just the final concat step.
	evidence := map[string]string{}
	for i, f := range expected.Files {
		content, err := os.ReadFile(filepath.Join("testdata", "digest-vectors", f))
		if err != nil {
			t.Fatal(err)
		}
		normalized := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
		sum := sha256.Sum256(normalized)
		if got := hex.EncodeToString(sum[:]); got != expected.PerFileSha256[i] {
			t.Fatalf("per-file hash mismatch for %s: got %s want %s", f, got, expected.PerFileSha256[i])
		}
		evidence[f] = "sha256:" + expected.PerFileSha256[i]
	}
	if got := CanonicalDigestFromEvidence(evidence); got != expected.Digest {
		t.Fatalf("canonical digest mismatch: got %s want %s", got, expected.Digest)
	}
}

func TestSigningKeyLoading(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	for name, encoded := range map[string]string{
		"base64-pem": base64.StdEncoding.EncodeToString(pemBytes),
		"raw-pem":    string(pemBytes),
		"base64-der": base64.StdEncoding.EncodeToString(der),
	} {
		key, err := parseSigningKey(encoded)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !key.Equal(priv) {
			t.Errorf("%s: parsed key differs", name)
		}
	}
	if _, err := parseSigningKey("not-a-key"); err == nil {
		t.Error("garbage input must fail")
	}
	// Configured-but-invalid env must error (caller refuses to start).
	t.Setenv("APPROVAL_SIGNING_KEY", "garbage")
	t.Setenv("APPROVAL_SIGNING_KEY_ID", "k1")
	if _, err := NewApprovalServiceFromEnv(nil); err == nil {
		t.Error("invalid configured key must be a startup error")
	}
	// Unset env = feature off, no error.
	t.Setenv("APPROVAL_SIGNING_KEY", "")
	if svc, err := NewApprovalServiceFromEnv(nil); err != nil || svc != nil {
		t.Errorf("unset key must mean (nil, nil), got (%v, %v)", svc, err)
	}
}

// ── HTTP-level tests (need the shared test DB from crsync_test.go TestMain) ──

func newTestApprovalService(t *testing.T) (*ApprovalService, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewApprovalService(testPool, priv, "approval-test"), pub
}

func testUserID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO "user" (email, name) VALUES ('governance-approver@test', 'Approver')
		ON CONFLICT (email) DO UPDATE SET updated_at = now()
		RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("test user fixture: %v", err)
	}
	return id
}

func seedEvidenceEvent(t *testing.T, crID string, evidence map[string]string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO cr_sync_event (cr_id, commit_sha, event_kind, payload, evidence, occurred_at)
		VALUES ($1, $2, 'status', '{}', $3, now())
		ON CONFLICT DO NOTHING`, crID, "ev-"+crID, evidence)
	if err != nil {
		t.Fatal(err)
	}
}

func approveHTTP(t *testing.T, svc *ApprovalService, userID, crID string, body approveRequest, taskToken bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/crs/"+crID+"/approve", bytes.NewReader(raw))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceID", testWorkspaceID)
	rctx.URLParams.Add("crID", crID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = middleware.SetMemberContext(ctx, testWorkspaceID, db.Member{})
	req = req.WithContext(ctx)
	req.Header.Set("X-User-ID", userID)
	if taskToken {
		req.Header.Set("X-Actor-Source", "task_token")
	}
	rec := httptest.NewRecorder()
	svc.HandleApprove(rec, req)
	return rec
}

func TestApproveRejectsTaskTokens(t *testing.T) {
	svc, _ := newTestApprovalService(t)
	rec := approveHTTP(t, svc, testUserID(t), "CR-9002-001", approveRequest{Stage: "requirement", Decision: "approve"}, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mat_ (task token) must be 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestApproveEvidenceDrift409(t *testing.T) {
	crID := "CR-9002-002"
	resetCR(t, crID)
	seedEvidenceEvent(t, crID, map[string]string{"a.yml": "sha256:" + hex.EncodeToString(make([]byte, 32))})
	svc, _ := newTestApprovalService(t)
	rec := approveHTTP(t, svc, testUserID(t), crID, approveRequest{
		Stage: "requirement", Decision: "approve",
		EvidenceDigest: "1111111111111111111111111111111111111111111111111111111111111111",
	}, false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale evidence_digest must be 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestApproveIssuesVerifiableGrantAndPersistsRecord(t *testing.T) {
	crID := "CR-9002-003"
	resetCR(t, crID)
	evidence := map[string]string{
		"change-requests/CR-9002-003/review-annotations/requirement.yml": "sha256:" + hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32)),
	}
	seedEvidenceEvent(t, crID, evidence)
	svc, pub := newTestApprovalService(t)
	userID := testUserID(t)

	rec := approveHTTP(t, svc, userID, crID, approveRequest{Stage: "requirement", Decision: "approve"}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Grant Grant `json:"grant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	g := resp.Grant
	if g.V != 1 || g.CRID != crID || g.Stage != "requirement" || g.KeyID != "approval-test" {
		t.Fatalf("grant fields wrong: %+v", g)
	}
	if g.EvidenceDigest != CanonicalDigestFromEvidence(evidence) {
		t.Fatal("grant must bind the server-computed digest")
	}
	sig, err := base64.StdEncoding.DecodeString(g.Signature)
	if err != nil || !ed25519.Verify(pub, []byte(grantCanonical(g)), sig) {
		t.Fatal("grant signature must verify against the service public key")
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM approval_record WHERE cr_id = $1 AND stage = 'requirement' AND decision = 'approve'`,
		crID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("approval_record row missing: n=%d err=%v", n, err)
	}

	// Idempotent re-approve of the same evidence: no second row, same grant back.
	rec2 := approveHTTP(t, svc, userID, crID, approveRequest{Stage: "requirement", Decision: "approve"}, false)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-approve must be 200, got %d", rec2.Code)
	}
	_ = testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM approval_record WHERE cr_id = $1 AND decision = 'approve'`, crID).Scan(&n)
	if n != 1 {
		t.Fatalf("re-approve must not create a second row, got %d", n)
	}

	// Reject of the same evidence coexists (partial unique index, SDD-SUG-001).
	rec3 := approveHTTP(t, svc, userID, crID, approveRequest{Stage: "requirement", Decision: "reject", RejectReason: "changed my mind"}, false)
	if rec3.Code != http.StatusOK {
		t.Fatalf("reject must be 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestGrantDeliveryQueue(t *testing.T) {
	crID := "CR-9002-004"
	resetCR(t, crID)
	seedEvidenceEvent(t, crID, map[string]string{"x.yml": "sha256:" + hex.EncodeToString(bytes.Repeat([]byte{0x01}, 32))})
	svc, _ := newTestApprovalService(t)
	rec := approveHTTP(t, svc, testUserID(t), crID, approveRequest{Stage: "dev-start", Decision: "approve"}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}

	pendingReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/daemon/approvals/pending", nil)
		req = req.WithContext(middleware.WithDaemonContext(req.Context(), testWorkspaceID, "daemon-test"))
		r := httptest.NewRecorder()
		svc.HandleGrantsPending(r, req)
		return r
	}
	r1 := pendingReq()
	var pending struct {
		Grants []struct {
			ID   string `json:"id"`
			CRID string `json:"cr_id"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(r1.Body.Bytes(), &pending); err != nil || len(pending.Grants) == 0 {
		t.Fatalf("pending must include the new grant: %s (err=%v)", r1.Body.String(), err)
	}
	// Ack → queue drains.
	ids := []string{}
	for _, g := range pending.Grants {
		if g.CRID == crID {
			ids = append(ids, g.ID)
		}
	}
	body, _ := json.Marshal(map[string]any{"ids": ids})
	ackReq := httptest.NewRequest(http.MethodPost, "/api/daemon/approvals/ack", bytes.NewReader(body))
	ackReq = ackReq.WithContext(middleware.WithDaemonContext(ackReq.Context(), testWorkspaceID, "daemon-test"))
	ackRec := httptest.NewRecorder()
	svc.HandleGrantsAck(ackRec, ackReq)
	if ackRec.Code != http.StatusOK {
		t.Fatalf("ack: %d", ackRec.Code)
	}
	r2 := pendingReq()
	var after struct {
		Grants []struct {
			CRID string `json:"cr_id"`
		} `json:"grants"`
	}
	_ = json.Unmarshal(r2.Body.Bytes(), &after)
	for _, g := range after.Grants {
		if g.CRID == crID {
			t.Fatal("acked grant must leave the pending queue")
		}
	}
	// No daemon context → 403.
	noCtx := httptest.NewRecorder()
	svc.HandleGrantsPending(noCtx, httptest.NewRequest(http.MethodGet, "/api/daemon/approvals/pending", nil))
	if noCtx.Code != http.StatusForbidden {
		t.Fatalf("pending without daemon binding must be 403, got %d", noCtx.Code)
	}
	_ = time.Now()
}
