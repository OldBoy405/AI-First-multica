// AIFIRST: CR projection sync worker (CR-2026-002 TASK-05, SDD §1.2/§4.4).
//
// Consumes crctl outbox events reported by daemons (POST /api/daemon/cr-events),
// records them idempotently in cr_sync_event, and projects legal status changes
// onto the cr table. Git stays the authority: on any doubt (out-of-order events,
// unknown transitions) the row is flagged needs_reconcile instead of guessing,
// and the reconcile job heals it from the knowledge-base repo.
//
// This package deliberately uses the pgx pool directly instead of sqlc: the
// projection tables are fork-only and keeping their queries here avoids
// touching the upstream-owned sqlc query files (CONTRIBUTING.AIFIRST.md rule 1).
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// MaxEventsPerBatch bounds one report call (PRD FR-2).
const MaxEventsPerBatch = 100

// EventCRUpdated is the WS event type broadcast to workspace rooms after a
// projection change (board refresh signal).
const EventCRUpdated = protocol.EventCRUpdated

var crIDRe = regexp.MustCompile(`^CR-\d{4}-\d{3}$`)

var knownEventKinds = map[string]bool{
	"status": true, "owners": true, "checkpoint": true,
	"merge": true, "archive": true, "inbox": true,
	"audit":    true, // TASK-10: activity_log rows, bypasses the cr ledger
	"snapshot": true, // TASK-07: daemon-mode reconcile, bypasses the cr ledger
	"review":   true, // CR-2026-011 TASK-03: review-verdict visibility (blocked/passed), not a status transition
}

// ledgerlessKinds carry no commit sha (the ledger's idempotency key) and may
// lack a CR binding; they are handled by dedicated ingestors instead of the
// projection ledger.
var ledgerlessKinds = map[string]bool{"audit": true, "snapshot": true}

// pendingShaPrefix marks an embedded-mode placeholder commit sha — crctl's
// pendingCommitSha() emits "pending:{ms}:{pid}:{seq}" (CR-2026-003 FR-1, the
// cross-language contract literal locked by tests on both sides). Placeholders
// exist solely to keep the idempotency key unique; they must never be
// projected as a commit pointer.
const pendingShaPrefix = "pending:"

// projectableSha returns the sha usable as cr.projected_commit: placeholders
// degrade to "" so the existing "empty keeps the current pointer" semantics
// apply (the next checkpoint event carries the real sha and catches up).
func projectableSha(sha string) string {
	if strings.HasPrefix(sha, pendingShaPrefix) {
		return ""
	}
	return sha
}

// OutboxEvent mirrors the crctl outbox event schema v1. File is injected by the
// daemon alongside the event so the server can ack exactly which outbox files
// may be deleted.
type OutboxEvent struct {
	V          int               `json:"v"`
	File       string            `json:"file"`
	EventKind  string            `json:"event_kind"`
	CRID       string            `json:"cr_id"`
	FromStatus string            `json:"from_status"`
	ToStatus   string            `json:"to_status"`
	Trigger    string            `json:"trigger"`
	CommitSHA  string            `json:"commit_sha"`
	Actor      string            `json:"actor"`
	Evidence   map[string]string `json:"evidence"`
	Payload    json.RawMessage   `json:"payload"`
	OccurredAt time.Time         `json:"occurred_at"`
}

type crEventsRequest struct {
	WorkspaceRootHash string        `json:"workspace_root_hash"`
	Events            []OutboxEvent `json:"events"`
}

type rejectedEvent struct {
	File string `json:"file"`
	Code string `json:"code"`
}

type crEventsResponse struct {
	Accepted []string        `json:"accepted"`
	Rejected []rejectedEvent `json:"rejected"`
}

// SyncService projects CR events onto the cr table.
type SyncService struct {
	pool *pgxpool.Pool
	bus  *events.Bus
	crMu sync.Map // cr_id -> *sync.Mutex; per-CR serialization (single node; PG advisory lock when multi-node)
}

func NewSyncService(pool *pgxpool.Pool, bus *events.Bus) *SyncService {
	return &SyncService{pool: pool, bus: bus}
}

func (s *SyncService) lockCR(workspaceID, crID string) func() {
	// CR-2026-049 TASK-05: mutex key includes the workspace so same-named CRs
	// in two tenants never serialize against each other or interleave applies.
	key := workspaceID + "\x00" + crID
	v, _ := s.crMu.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// HandleCREvents is POST /api/daemon/cr-events (DaemonAuth group). The trusted
// workspace binding comes exclusively from the daemon auth context — the
// request body's workspace_root_hash is log-only, never a trust input
// (SDD-SUG-002).
func (s *SyncService) HandleCREvents(w http.ResponseWriter, r *http.Request) {
	workspaceID, denyReason := resolveDaemonWorkspace(r, s.pool)
	if workspaceID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": denyReason})
		return
	}
	var req crEventsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(req.Events) > MaxEventsPerBatch {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many events in one batch (max 100)"})
		return
	}
	resp := crEventsResponse{Accepted: []string{}, Rejected: []rejectedEvent{}}
	for _, ev := range req.Events {
		if code := validateEvent(ev); code != "" {
			resp.Rejected = append(resp.Rejected, rejectedEvent{File: ev.File, Code: code})
			continue
		}
		if err := s.ingest(r.Context(), workspaceID, ev); err != nil {
			resp.Rejected = append(resp.Rejected, rejectedEvent{File: ev.File, Code: "INGEST_FAILED"})
			continue
		}
		resp.Accepted = append(resp.Accepted, ev.File)
	}
	writeJSON(w, http.StatusOK, resp)
}

func validateEvent(ev OutboxEvent) string {
	if ev.V != 1 {
		return "BAD_EVENT"
	}
	// audit/snapshot events may lack a CR binding (a gitguard denial outside
	// any CR context; a whole-workspace snapshot); every other kind projects
	// onto a cr row and needs the id.
	if !crIDRe.MatchString(ev.CRID) && !(ledgerlessKinds[ev.EventKind] && ev.CRID == "") {
		return "BAD_EVENT"
	}
	if !knownEventKinds[ev.EventKind] {
		return "UNKNOWN_KIND"
	}
	if ev.OccurredAt.IsZero() {
		return "BAD_EVENT"
	}
	return ""
}

// ingest records the event idempotently and, when it is new, applies it to the
// projection. A duplicate (same idempotency key via both channels) is still
// acked as accepted so the daemon deletes its outbox file — duplication is the
// normal case, not an error.
func (s *SyncService) ingest(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	if ev.EventKind == "audit" {
		return s.ingestAudit(ctx, workspaceID, ev)
	}
	if ev.EventKind == "snapshot" {
		return s.ingestSnapshot(ctx, workspaceID, ev)
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	evidence := ev.Evidence
	if evidence == nil {
		evidence = map[string]string{}
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cr_sync_event (workspace_id, cr_id, commit_sha, event_kind, payload, evidence, actor, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id, cr_id, commit_sha, event_kind) DO NOTHING`,
		workspaceID, ev.CRID, ev.CommitSHA, ev.EventKind, payload, evidence, ev.Actor, ev.OccurredAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already ingested via the other channel
	}
	unlock := s.lockCR(workspaceID, ev.CRID)
	defer unlock()
	if err := s.apply(ctx, workspaceID, ev); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE cr_sync_event SET processed_at = now()
		WHERE workspace_id = $1 AND cr_id = $2 AND commit_sha = $3 AND event_kind = $4`,
		workspaceID, ev.CRID, ev.CommitSHA, ev.EventKind)
	return err
}

func (s *SyncService) apply(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	switch ev.EventKind {
	case "status", "archive":
		return s.applyStatus(ctx, workspaceID, ev)
	case "review":
		return s.applyReview(ctx, workspaceID, ev)
	case "checkpoint":
		// Fills projected_commit; also the completion channel for --embedded
		// status events that carried an empty commit_sha (source design §A.5 —
		// no delayed processing needed because status application never gates
		// on the sha; the checkpoint simply catches the pointer up).
		_, err := s.pool.Exec(ctx, `
			UPDATE cr SET projected_commit = $3, updated_at = now()
			WHERE workspace_id = $1 AND cr_id = $2 AND $3 <> ''`,
			workspaceID, ev.CRID, ev.CommitSHA)
		if err != nil {
			return err
		}
		s.publish(ctx, workspaceID, ev.CRID)
		return nil
	default:
		// owners/merge/inbox: not emitted by crctl yet; ledger keeps them for replay.
		return nil
	}
}

func (s *SyncService) applyStatus(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	var curStatus string
	found := true
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM cr WHERE workspace_id = $1 AND cr_id = $2`,
		workspaceID, ev.CRID).Scan(&curStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
		} else {
			return err
		}
	}
	if !found {
		// First event for this CR. A clean registration (from "" and legal) is
		// trusted; anything mid-flight means we missed history — project the
		// reported status best-effort but flag for reconcile.
		legalFresh := ev.FromStatus == "" && IsLegalTransition("", ev.ToStatus, ev.Trigger)
		if !KnownStatuses[ev.ToStatus] {
			return s.flagUnknownCR(ctx, workspaceID, ev.CRID)
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO cr (workspace_id, cr_id, status, projected_commit, needs_reconcile)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (workspace_id, cr_id) DO NOTHING`,
			workspaceID, ev.CRID, ev.ToStatus, projectableSha(ev.CommitSHA), !legalFresh)
		if err != nil {
			return err
		}
		// Gate-node projection (CR-2026-011 TASK-02) is read-side only — only
		// project when the transition is trusted (legalFresh); an untrusted
		// first sighting is flagged needs_reconcile above and must not seed a
		// pipeline_run off a from/to pair we don't believe.
		if legalFresh {
			s.projectGateTransition(ctx, workspaceID, ev.CRID, ev.FromStatus, ev.ToStatus)
		}
		s.publish(ctx, workspaceID, ev.CRID)
		return nil
	}
	if curStatus == ev.FromStatus && KnownStatuses[ev.ToStatus] && IsLegalTransition(ev.FromStatus, ev.ToStatus, ev.Trigger) {
		_, err := s.pool.Exec(ctx, `
			UPDATE cr SET status = $3,
			              projected_commit = CASE WHEN $4 <> '' THEN $4 ELSE projected_commit END,
			              updated_at = now()
			WHERE workspace_id = $1 AND cr_id = $2`,
			workspaceID, ev.CRID, ev.ToStatus, projectableSha(ev.CommitSHA))
		if err != nil {
			return err
		}
		s.projectGateTransition(ctx, workspaceID, ev.CRID, ev.FromStatus, ev.ToStatus)
	} else {
		// Out-of-order or illegal: never force the projection — flag and let
		// reconcile replay from the authority.
		if _, err := s.pool.Exec(ctx, `
			UPDATE cr SET needs_reconcile = TRUE, updated_at = now()
			WHERE workspace_id = $1 AND cr_id = $2`,
			workspaceID, ev.CRID); err != nil {
			return err
		}
	}
	s.publish(ctx, workspaceID, ev.CRID)
	return nil
}

func (s *SyncService) flagUnknownCR(ctx context.Context, workspaceID, crID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, status, needs_reconcile)
		VALUES ($1, $2, 'drafting', TRUE)
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET needs_reconcile = TRUE, updated_at = now()`,
		workspaceID, crID)
	return err
}

func (s *SyncService) publish(ctx context.Context, workspaceID, crID string) {
	if s.bus == nil {
		return
	}
	var status string
	var needsReconcile bool
	err := s.pool.QueryRow(ctx,
		`SELECT status, needs_reconcile FROM cr WHERE workspace_id = $1 AND cr_id = $2`,
		workspaceID, crID).Scan(&status, &needsReconcile)
	if err != nil {
		return
	}
	s.bus.Publish(events.Event{
		Type:        EventCRUpdated,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"cr_id":           crID,
			"status":          status,
			"needs_reconcile": needsReconcile,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
