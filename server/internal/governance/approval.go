// AIFIRST: server-side signed approvals (CR-2026-002 TASK-08, P1 design §B).
//
// Replaces the crctl TTY prompt for approvals without weakening the guarantee:
// the human check moves to the server (RequireHumanActor rejects task tokens),
// the approval is bound to one exact evidence version (canonical digest), and
// the resulting grant file is Ed25519-signed so crctl can verify it offline
// against the public key committed into the knowledge-base repo.
//
// Signing happens in exactly one place (signGrant); the private key never
// leaves this file. Key material comes from APPROVAL_SIGNING_KEY (base64 of a
// PKCS#8 PEM/DER, injected by the orchestrator) — when unset the approval
// endpoints are simply not mounted (self-host without signed approvals still
// boots); when set but invalid the server refuses to start (fail closed).
package governance

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/middleware"
)

// Grant mirrors the crctl grant file schema v1 (source design §B.2). Field
// names must match crctl's approveWithGrant exactly.
type Grant struct {
	V              int    `json:"v"`
	CRID           string `json:"cr_id"`
	Stage          string `json:"stage"`
	Decision       string `json:"decision"`
	Approver       string `json:"approver"`
	ApprovedAt     string `json:"approved_at"`
	EvidenceDigest string `json:"evidence_digest"`
	KeyID          string `json:"key_id"`
	Signature      string `json:"signature"`
}

func grantCanonical(g Grant) string {
	return fmt.Sprintf("v1|%s|%s|%s|%s|%s|%s", g.CRID, g.Stage, g.Decision, g.Approver, g.ApprovedAt, g.EvidenceDigest)
}

var approvalStages = map[string]bool{"requirement": true, "tech-design": true, "dev-start": true, "code": true}

// CanonicalDigestFromEvidence recomputes the canonical evidence digest from a
// {path: "sha256:<hex>"} snapshot: per-file hashes concatenated in path order,
// hashed again. Must stay byte-identical with crctl's canonicalEvidenceDigest —
// parity is locked by the shared test vectors (testdata/digest-vectors, source
// of truth: tools skills/shared/crctl/scripts/test/fixtures/digest-vectors).
// Empty snapshot returns "".
func CanonicalDigestFromEvidence(evidence map[string]string) string {
	if len(evidence) == 0 {
		return ""
	}
	paths := make([]string, 0, len(evidence))
	for p := range evidence {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var concat strings.Builder
	for _, p := range paths {
		concat.WriteString(strings.TrimPrefix(evidence[p], "sha256:"))
	}
	sum := sha256.Sum256([]byte(concat.String()))
	return hex.EncodeToString(sum[:])
}

// ApprovalService issues signed grants and serves the daemon delivery queue.
type ApprovalService struct {
	pool       *pgxpool.Pool
	key        ed25519.PrivateKey
	keyID      string
	onGrantAck func(context.Context, string, string)
}

// SetGrantAckHandler wires the optional Runner wake callback. ApprovalService
// remains the sole writer of delivered_at; the callback is only a wake signal.
func (a *ApprovalService) SetGrantAckHandler(fn func(context.Context, string, string)) {
	if a != nil {
		a.onGrantAck = fn
	}
}

func NewApprovalService(pool *pgxpool.Pool, key ed25519.PrivateKey, keyID string) *ApprovalService {
	return &ApprovalService{pool: pool, key: key, keyID: keyID}
}

// NewApprovalServiceFromEnv builds the service from APPROVAL_SIGNING_KEY /
// APPROVAL_SIGNING_KEY_ID. Returns (nil, nil) when the key is not configured
// (feature off) and an error when it is configured but unusable (caller must
// refuse to start — §B.5).
func NewApprovalServiceFromEnv(pool *pgxpool.Pool) (*ApprovalService, error) {
	raw := strings.TrimSpace(os.Getenv("APPROVAL_SIGNING_KEY"))
	if raw == "" {
		return nil, nil
	}
	keyID := strings.TrimSpace(os.Getenv("APPROVAL_SIGNING_KEY_ID"))
	if keyID == "" {
		return nil, fmt.Errorf("APPROVAL_SIGNING_KEY is set but APPROVAL_SIGNING_KEY_ID is empty")
	}
	key, err := parseSigningKey(raw)
	if err != nil {
		return nil, fmt.Errorf("APPROVAL_SIGNING_KEY invalid: %w", err)
	}
	// Startup smoke test: a sign/verify roundtrip catches wrong key types or
	// corrupted material before the first real approval (§B.5). Only key_id is
	// ever logged, never key bytes.
	msg := []byte("aifirst-approval-smoke")
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), msg, ed25519.Sign(key, msg)) {
		return nil, fmt.Errorf("signing key smoke test failed for key_id=%s", keyID)
	}
	return NewApprovalService(pool, key, keyID), nil
}

// parseSigningKey accepts base64(PEM PKCS#8), raw PEM PKCS#8, or base64(DER PKCS#8).
func parseSigningKey(raw string) (ed25519.PrivateKey, error) {
	data := []byte(raw)
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		data = decoded
	}
	der := data
	if block, _ := pem.Decode(data); block != nil {
		der = block.Bytes
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an Ed25519 key (%T)", parsed)
	}
	return key, nil
}

// PublicKeyPEM returns the SPKI PEM of the signing key's public half — the
// exact content to commit as {workspace}/.crctl/keys/{key_id}.pub.
func (a *ApprovalService) PublicKeyPEM() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(a.key.Public())
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func (a *ApprovalService) signGrant(g *Grant) {
	g.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(a.key, []byte(grantCanonical(*g))))
}

// requireHumanActor rejects any request authenticated by an agent task token
// (mat_): middleware.Auth marks those with X-Actor-Source=task_token after
// stripping the client-supplied value, so this header is trustworthy.
func requireHumanActor(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Actor-Source") == "task_token" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approvals require a human actor; task tokens are rejected"})
		return false
	}
	if r.Header.Get("X-User-ID") == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no authenticated user"})
		return false
	}
	return true
}

// latestEvidence returns the newest non-empty evidence snapshot for a CR.
// cr_sync_event carries workspace_id (CR-2026-049 TASK-05, migration 390) and
// the query scopes on it directly — a same-named CR in another workspace can
// never leak its evidence (file paths + sha256 digests) through this
// workspace's approval card.
func (a *ApprovalService) latestEvidence(r *http.Request, crID string) (map[string]string, error) {
	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	var evidence map[string]string
	err := a.pool.QueryRow(r.Context(), `
		SELECT cse.evidence FROM cr_sync_event cse
		WHERE cse.workspace_id = $1::uuid AND cse.cr_id = $2 AND cse.evidence <> '{}'::jsonb
		ORDER BY cse.id DESC LIMIT 1`, workspaceID, crID).Scan(&evidence)
	if err != nil {
		if err.Error() == "no rows in result set" || strings.Contains(err.Error(), "no rows") {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return evidence, nil
}

type approveRequest struct {
	Stage        string `json:"stage"`
	Decision     string `json:"decision"`
	RejectReason string `json:"reject_reason"`
	// EvidenceDigest is what the approval card showed the human. When set it
	// must equal the server's current digest — otherwise the evidence changed
	// under the approver's feet and the request is a 409 (§B.1 ③).
	EvidenceDigest string `json:"evidence_digest"`
}

// HandleApprove is POST /api/workspaces/{workspaceID}/crs/{crID}/approve
// (session auth + workspace membership enforced by router middleware).
func (a *ApprovalService) HandleApprove(w http.ResponseWriter, r *http.Request) {
	if !requireHumanActor(w, r) {
		return
	}
	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	crID := chi.URLParam(r, "crID")
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !approvalStages[req.Stage] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stage must be requirement | tech-design | dev-start | code"})
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decision must be approve or reject"})
		return
	}
	// SDD DD-5 (CR-2026-011 TASK-04): this endpoint had no role check before
	// this task, only requireHumanActor. See canApprove's doc comment
	// (project_gates.go) for why it checks workspace owner/admin only.
	if allowed, err := canApprove(r.Context(), a.pool, workspaceID, r.Header.Get("X-User-ID")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approver check failed"})
		return
	} else if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "FORBIDDEN_APPROVER", "detail": "only workspace owners/admins may approve or reject",
		})
		return
	}
	evidence, err := a.latestEvidence(r, crID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "evidence lookup failed"})
		return
	}
	digest := CanonicalDigestFromEvidence(evidence)
	if req.EvidenceDigest != "" && req.EvidenceDigest != digest {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "EVIDENCE_DRIFT", "expected": req.EvidenceDigest, "current": digest,
			"detail": "the evidence changed after the approval card was rendered; reload and re-review",
		})
		return
	}
	grant := Grant{
		V: 1, CRID: crID, Stage: req.Stage, Decision: req.Decision,
		Approver: r.Header.Get("X-User-ID"), ApprovedAt: time.Now().Format(time.RFC3339),
		EvidenceDigest: digest, KeyID: a.keyID,
	}
	a.signGrant(&grant)
	grantJSON, _ := json.Marshal(grant)
	tag, err := a.pool.Exec(r.Context(), `
		INSERT INTO approval_record (workspace_id, cr_id, stage, decision, approver_user_id, evidence_digest, key_id, signature, reject_reason, grant_json)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10)
		ON CONFLICT (workspace_id, cr_id, stage, evidence_digest) WHERE decision = 'approve' DO NOTHING`,
		workspaceID, crID, req.Stage, req.Decision, grant.Approver, digest, a.keyID, grant.Signature, req.RejectReason, grantJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval persistence failed"})
		return
	}
	if req.Decision == "approve" && tag.RowsAffected() == 0 {
		// Idempotent re-approve of the same evidence version: return the grant
		// signed the first time (its approved_at is part of the signature).
		var existing json.RawMessage
		if err := a.pool.QueryRow(r.Context(), `
			SELECT grant_json FROM approval_record
			WHERE workspace_id = $1::uuid AND cr_id = $2 AND stage = $3 AND evidence_digest = $4 AND decision = 'approve'`,
			workspaceID, crID, req.Stage, digest).Scan(&existing); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"grant": existing, "idempotent": true})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant": grant})
}

// HandleApprovalCard is GET /api/workspaces/{workspaceID}/crs/{crID}/approval —
// the evidence summary a human reviews before deciding.
func (a *ApprovalService) HandleApprovalCard(w http.ResponseWriter, r *http.Request) {
	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	crID := chi.URLParam(r, "crID")
	evidence, err := a.latestEvidence(r, crID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "evidence lookup failed"})
		return
	}
	var status string
	var needsReconcile bool
	_ = a.pool.QueryRow(r.Context(),
		`SELECT status, needs_reconcile FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`,
		workspaceID, crID).Scan(&status, &needsReconcile)
	writeJSON(w, http.StatusOK, map[string]any{
		"cr_id": crID, "status": status, "needs_reconcile": needsReconcile,
		"evidence": evidence, "evidence_digest": CanonicalDigestFromEvidence(evidence),
		"key_id": a.keyID,
	})
}

// HandleListCRs is GET /api/workspaces/{workspaceID}/crs — the board list.
func (a *ApprovalService) HandleListCRs(w http.ResponseWriter, r *http.Request) {
	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	rows, err := a.pool.Query(r.Context(), `
		SELECT cr_id, title, status, owners, target_version, projected_commit, needs_reconcile, updated_at
		FROM cr WHERE workspace_id = $1::uuid ORDER BY cr_id`, workspaceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	type crRow struct {
		CRID            string          `json:"cr_id"`
		Title           string          `json:"title"`
		Status          string          `json:"status"`
		Owners          json.RawMessage `json:"owners"`
		TargetVersion   string          `json:"target_version"`
		ProjectedCommit string          `json:"projected_commit"`
		NeedsReconcile  bool            `json:"needs_reconcile"`
		UpdatedAt       time.Time       `json:"updated_at"`
	}
	list := []crRow{}
	for rows.Next() {
		var c crRow
		if err := rows.Scan(&c.CRID, &c.Title, &c.Status, &c.Owners, &c.TargetVersion, &c.ProjectedCommit, &c.NeedsReconcile, &c.UpdatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "scan failed"})
			return
		}
		list = append(list, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"crs": list})
}

// HandleGrantsPending is GET /api/daemon/approvals/pending (DaemonAuth group):
// undelivered grants for the daemon's workspace.
func (a *ApprovalService) HandleGrantsPending(w http.ResponseWriter, r *http.Request) {
	workspaceID, denyReason := resolveDaemonWorkspace(r, a.pool)
	if workspaceID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": denyReason})
		return
	}
	rows, err := a.pool.Query(r.Context(), `
		SELECT id::text, cr_id, stage, grant_json FROM approval_record
		WHERE workspace_id = $1::uuid AND delivered_at IS NULL
		ORDER BY created_at`, workspaceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()
	type pending struct {
		ID    string          `json:"id"`
		CRID  string          `json:"cr_id"`
		Stage string          `json:"stage"`
		Grant json.RawMessage `json:"grant"`
	}
	list := []pending{}
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ID, &p.CRID, &p.Stage, &p.Grant); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "scan failed"})
			return
		}
		list = append(list, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": list})
}

// HandleGrantsAck is POST /api/daemon/approvals/ack {ids: [...]} — the daemon
// confirms grants were written to the workspace's .crctl/grants/.
func (a *ApprovalService) HandleGrantsAck(w http.ResponseWriter, r *http.Request) {
	workspaceID, denyReason := resolveDaemonWorkspace(r, a.pool)
	if workspaceID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": denyReason})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
		return
	}
	rows, err := a.pool.Query(r.Context(), `
		UPDATE approval_record SET delivered_at = now()
		WHERE workspace_id = $1::uuid AND id::text = ANY($2) AND delivered_at IS NULL
		RETURNING cr_id`, workspaceID, req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack failed"})
		return
	}
	crIDs := map[string]struct{}{}
	for rows.Next() {
		var crID string
		if err := rows.Scan(&crID); err != nil {
			rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack scan failed"})
			return
		}
		crIDs[crID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack scan failed"})
		return
	}
	rows.Close()
	if a.onGrantAck != nil {
		for crID := range crIDs {
			a.onGrantAck(r.Context(), workspaceID, crID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
