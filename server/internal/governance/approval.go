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
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// GrantAckEvent carries the fields an ACK callback (FR-10) needs. WorkspaceID
// is the authenticated daemon workspace; RecordID is approval_record.id in
// its text form (parity with the pending endpoint); Stage/Decision are the
// approval_record values. It is passed to BOTH hooks so each can decide
// independently (TD-BL-12).
type GrantAckEvent struct {
	WorkspaceID string // daemon workspace (authenticated)
	CrID        string
	RecordID    string // approval_record.id text form
	Stage       string // requirement | tech-design | dev-start | code
	Decision    string // approve | reject
}

// approvalContinuationEnqueuer is the TaskService surface HandleGrantsAck needs.
// Declared as a local interface (same convention as Runner's
// pipelineTaskEnqueuer) so governance stays mockable and the import direction
// stays governance → service (no cycle).
type approvalContinuationEnqueuer interface {
	EnqueueApprovalContinuation(ctx context.Context, qtx *db.Queries, spec service.ApprovalContinuationSpec) (db.AgentTaskQueue, service.EnqueueOutcome, error)
	NotifyContinuationTaskEnqueued(ctx context.Context, task db.AgentTaskQueue) error
}

// ApprovalService issues signed grants and serves the daemon delivery queue.
type ApprovalService struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	tasks   approvalContinuationEnqueuer
	key     ed25519.PrivateKey
	keyID   string
	// onGrantAck is the FR-10 canonical callback: a PRE-COMMIT pure
	// validation hook. Its error rolls back the whole ACK batch and yields
	// HTTP 5xx. It MUST have zero external side effects — no table writes, no
	// event enqueue, no locks intersecting the ACK row locks, and it must not
	// depend on this transaction's uncommitted writes (TD-BL-12). The real
	// wake is onGrantAckCommitted below.
	onGrantAck func(context.Context, GrantAckEvent) error
	// onGrantAckCommitted is the POST-COMMIT wake. Its error is logged only;
	// HTTP stays 2xx because delivered_at is already committed and the daemon
	// does not redeliver an ACKed record (TD-BL-12 / SDD §3.2).
	onGrantAckCommitted func(context.Context, GrantAckEvent) error
}

// SetGrantAckHandler wires the PRE-COMMIT FR-10 callback (pure validation,
// error → rollback/5xx). Retains the onGrantAck / SetGrantAckHandler name to
// match PRD FR-10 mechanically; the name is NOT reused for the committed wake
// (TD-BL-12).
func (a *ApprovalService) SetGrantAckHandler(fn func(context.Context, GrantAckEvent) error) {
	if a != nil {
		a.onGrantAck = fn
	}
}

// SetGrantAckCommittedHandler wires the POST-COMMIT wake (Reconcile). Its
// error is logged only; HTTP stays 2xx (TD-BL-12 / SDD §3.2).
func (a *ApprovalService) SetGrantAckCommittedHandler(fn func(context.Context, GrantAckEvent) error) {
	if a != nil {
		a.onGrantAckCommitted = fn
	}
}

func NewApprovalService(pool *pgxpool.Pool, key ed25519.PrivateKey, keyID string, queries *db.Queries, tasks approvalContinuationEnqueuer) *ApprovalService {
	if queries == nil {
		queries = db.New(pool) // nil-pool callers (e.g. signGrant-only) get a nil-DBTX handle
	}
	return &ApprovalService{pool: pool, queries: queries, tasks: tasks, key: key, keyID: keyID}
}

// NewApprovalServiceFromEnv builds the service from APPROVAL_SIGNING_KEY /
// APPROVAL_SIGNING_KEY_ID. Returns (nil, nil) when the key is not configured
// (feature off) and an error when it is configured but unusable (caller must
// refuse to start — §B.5).
func NewApprovalServiceFromEnv(pool *pgxpool.Pool, queries *db.Queries, tasks approvalContinuationEnqueuer) (*ApprovalService, error) {
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
	return NewApprovalService(pool, key, keyID, queries, tasks), nil
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
// confirms grants were written to the workspace's .crctl/grants/. Per CR-2026-052
// (SDD §4.1) this is a single pgx transaction: mark delivered_at AND enqueue
// the approval-continuation task atomically, all-or-nothing. The only 5xx
// paths are pre-commit (tx error or onGrantAck handler error) — committed wake
// errors are logged but keep HTTP 2xx (TD-BL-12).
// AIFIRST: CR-2026-052 TASK-04.
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
	ws, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid daemon workspace"})
		return
	}

	ctx := r.Context()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack failed"})
		return
	}
	qtx := a.queries.WithTx(tx)

	rows, err := qtx.AckApprovalGrants(ctx, db.AckApprovalGrantsParams{WorkspaceID: ws, Ids: req.IDs})
	if err != nil {
		rollbackTx(tx)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack failed"})
		return
	}

	type ackReason struct {
		CrID   string `json:"cr_id"`
		Stage  string `json:"stage"`
		Reason string `json:"reason"`
	}
	reasons := []ackReason{}
	ackEvents := make([]GrantAckEvent, 0, len(rows))
	type newTask struct {
		task    db.AgentTaskQueue
		outcome service.EnqueueOutcome
	}
	newTasks := []newTask{}

	for _, row := range rows {
		target, reason := resolveContinuationTarget(ctx, qtx, ws, row)
		if target == nil {
			reasons = append(reasons, ackReason{CrID: row.CrID, Stage: row.Stage, Reason: reason})
			continue
		}
		approver, _ := util.ParseUUID(row.ApproverUserID)
		task, outcome, enqErr := a.tasks.EnqueueApprovalContinuation(ctx, qtx, service.ApprovalContinuationSpec{
			WorkspaceID: ws,
			AgentID:     target.agentID,
			RuntimeID:   target.runtimeID,
			IssueID:     target.issueID,
			SquadID:     target.squadID,
			ProjectID:   target.projectID,
			CrID:        row.CrID,
			RecordID:    row.ID,
			Stage:       row.Stage,
			Decision:    row.Decision,
			ApproverID:  approver,
			Priority:    target.priority,
		})
		if enqErr != nil {
			rollbackTx(tx)
			slog.Warn("approval continuation enqueue failed", "cr_id", row.CrID, "stage", row.Stage, "error", enqErr)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "approval continuation failed",
				"reasons": append(reasons, ackReason{CrID: row.CrID, Stage: row.Stage, Reason: "tx-failure"}),
			})
			return
		}
		// Only newly-created rows are broadcast post-commit; merged/already-queued
		// reuse an existing row and must not be re-announced (TD-SUG-1).
		if outcome == service.OutcomeSuccessorEnqueued || outcome == service.OutcomeSlotDeferred {
			newTasks = append(newTasks, newTask{task: task, outcome: outcome})
		}
		ackEvents = append(ackEvents, GrantAckEvent{
			WorkspaceID: workspaceID,
			CrID:        row.CrID,
			RecordID:    row.ID,
			Stage:       row.Stage,
			Decision:    row.Decision,
		})
	}

	if len(reasons) > 0 {
		// FR-7 fail-closed: any unresolved target rolls back the whole batch.
		rollbackTx(tx)
		slog.Warn("approval continuation fail-closed", "reasons", reasons)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "approval continuation failed",
			"reasons": reasons,
		})
		return
	}

	// Pre-commit FR-10 canonical callback: pure validation, zero side effects.
	// An error here rolls back the batch and yields 5xx so the daemon retries.
	for _, ev := range ackEvents {
		if a.onGrantAck != nil {
			if err := a.onGrantAck(ctx, ev); err != nil {
				rollbackTx(tx)
				slog.Warn("approval continuation onGrantAck rejected", "cr_id", ev.CrID, "stage", ev.Stage, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":   "approval continuation failed",
					"reasons": []ackReason{{CrID: ev.CrID, Stage: ev.Stage, Reason: "ack-handler-failed"}},
				})
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Warn("approval continuation commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack failed"})
		return
	}

	// Post-commit: broadcast the newly-created tasks (FR-11) and wake.
	for _, nt := range newTasks {
		if err := a.tasks.NotifyContinuationTaskEnqueued(ctx, nt.task); err != nil {
			slog.Warn("approval continuation broadcast failed", "cr_id", nt.task.CrID, "error", err)
		}
	}
	for _, ev := range ackEvents {
		if a.onGrantAckCommitted != nil {
			if err := a.onGrantAckCommitted(ctx, ev); err != nil {
				// Committed: delivered_at is set, daemon will not redeliver. Log only.
				slog.Error("approval continuation ack-wake failed", "cr_id", ev.CrID, "stage", ev.Stage, "reason", "ack-wake-failed", "error", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// continuationTarget is the resolved FR-7 authority chain for one approval row.
type continuationTarget struct {
	agentID   pgtype.UUID
	runtimeID pgtype.UUID
	issueID   pgtype.UUID
	squadID   pgtype.UUID
	projectID pgtype.UUID
	priority  int32
}

// resolveContinuationTarget walks the authority chain cr → issue → squad →
// agent, locking each level before reading (FOR SHARE → FOR UPDATE), all
// scoped to the authenticated workspace. Any missing level returns (nil,
// reason) so the caller rolls back the whole ACK batch (FR-7 fail-closed —
// never falls back to an arbitrary agent). Reasons are NFR-10 observability
// codes (SDD §7.3). AIFIRST: CR-2026-052 TASK-04 (SDD §4.2, TD-BL-5/8/10).
func resolveContinuationTarget(ctx context.Context, qtx *db.Queries, ws pgtype.UUID, row db.AckApprovalGrantsRow) (*continuationTarget, string) {
	cr, err := qtx.GetCrShellIssueInWorkspaceForShare(ctx, db.GetCrShellIssueInWorkspaceForShareParams{
		WorkspaceID: ws,
		CrID:        row.CrID,
	})
	if err != nil {
		return nil, "workspace-mismatch"
	}
	if !cr.ShellIssueID.Valid {
		return nil, "issue-missing"
	}
	issue, err := qtx.LockIssueInWorkspaceForShare(ctx, db.LockIssueInWorkspaceForShareParams{
		ID:          cr.ShellIssueID,
		WorkspaceID: ws,
	})
	if err != nil {
		return nil, "issue-missing"
	}
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "squad" || !issue.AssigneeID.Valid {
		return nil, "leader-missing"
	}
	squad, err := qtx.LockSquadForAutopilotAssignment(ctx, db.LockSquadForAutopilotAssignmentParams{
		ID:          issue.AssigneeID,
		WorkspaceID: ws,
	})
	if err != nil || squad.ArchivedAt.Valid {
		return nil, "leader-missing"
	}
	leader, err := qtx.GetAgentForUpdate(ctx, squad.LeaderID)
	if err != nil {
		return nil, "leader-missing"
	}
	if !leader.WorkspaceID.Valid || leader.WorkspaceID != ws || leader.ArchivedAt.Valid || !leader.RuntimeID.Valid || leader.Kind != "user" {
		return nil, "leader-missing"
	}
	return &continuationTarget{
		agentID:   leader.ID,
		runtimeID: leader.RuntimeID,
		issueID:   issue.ID,
		squadID:   squad.ID,
		projectID: issue.ProjectID,
		priority:  priorityToIntIssue(issue.Priority),
	}, ""
}

// priorityToIntIssue maps an issue.Priority text value to the agent_task_queue
// priority int (parity with service.priorityToInt; duplicated here to avoid a
// governance → service method dependency for a pure mapping).
func priorityToIntIssue(p string) int32 {
	switch p {
	case "urgent":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func rollbackTx(tx pgx.Tx) {
	if tx != nil {
		_ = tx.Rollback(context.Background())
	}
}
