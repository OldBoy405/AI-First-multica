// AIFIRST: gate-node projector (CR-2026-011 TASK-02, SDD §4.1/DD-1).
//
// Projects the crctl status event stream onto pipeline_run/pipeline_node_run —
// the read-side data pipeline UI (D7's approval cards, blocked lists, CR
// badges) consumes so it doesn't need to reach into git or wait for the full
// Pipeline Runner (CR-H, unregistered as of this CR) to exist. Only the four
// approval-stage human_approval nodes are projected here; review-node blocked/
// passed projection is TASK-03's applyReview, sharing the same upsert helpers.
//
// Authority rule unchanged from cr/cr_sync_event: git is authoritative (the
// review-loop attempt count lives in review-loop.yml), this is a replayable
// projection. Node identity is never derived — gate_nodes_gen.go is the single
// generated source both this projector and any future Runner read (TSUG-002).
package governance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pipelineForStatus returns the pipeline template a CR status belongs to, or
// "" for statuses with no active pipeline (drafting, rejected, withdrawn).
func pipelineForStatus(status string) string {
	switch status {
	case "requirement-reviewing", "requirement-approved":
		return PipelineIDs.RequirementAuthoring
	case "tech-designing", "tech-design-review-pending", "tech-design-reviewed":
		return PipelineIDs.ArchitectureDesign
	case "task-breakdown", "developing", "code-reviewing", "code-approved":
		return PipelineIDs.CodeImplementation
	case "merging", "writing-back", "archived":
		return PipelineIDs.FeatureWriteback
	default:
		return ""
	}
}

// gateNodeAction is what a status transition does to its approval gate node,
// keyed by the exact status being entered (SDD §4.1's table, expanded 1:1).
type gateNodeAction struct {
	Stage  string // key into ApprovalGateNodes
	Status string // "running" | "passed"
}

var statusGateAction = map[string]gateNodeAction{
	"requirement-reviewing":      {"requirement", "running"},
	"requirement-approved":       {"requirement", "passed"},
	"tech-designing":             {"tech-design", "running"},
	"tech-design-review-pending": {"tech-design", "running"},
	"tech-design-reviewed":       {"tech-design", "passed"},
	"task-breakdown":             {"dev-start", "running"},
	"developing":                 {"dev-start", "passed"},
	"code-reviewing":             {"code", "running"},
	"code-approved":              {"code", "passed"},
}

// projectGateTransition applies one legal (fromStatus -> toStatus) CR
// transition to the pipeline_run/pipeline_node_run projection. Called from
// applyStatus after the cr row itself has been updated, inside the same
// per-CR lock (crsync.go's lockCR) — callers do not need their own locking.
//
// Failures are logged-and-swallowed rather than propagated: gate-node
// projection is a read-side enhancement (SDD DD-2 — approval card visibility
// depends only on cr.status, never on this table), so a projection error must
// never fail the cr-events ingest path that callers depend on for the
// authoritative status update.
func (s *SyncService) projectGateTransition(ctx context.Context, workspaceID, crID string, fromStatus, toStatus string) {
	if err := s.projectGateTransitionErr(ctx, workspaceID, crID, fromStatus, toStatus); err != nil {
		// Intentionally best-effort: see doc comment above.
		_ = err
	}
}

func (s *SyncService) projectGateTransitionErr(ctx context.Context, workspaceID, crID, fromStatus, toStatus string) error {
	if toStatus == "rejected" || toStatus == "withdrawn" {
		return s.cancelActiveRuns(ctx, workspaceID, crID)
	}

	newPID := pipelineForStatus(toStatus)
	oldPID := pipelineForStatus(fromStatus)
	if oldPID != "" && oldPID != newPID {
		if err := s.completeRun(ctx, workspaceID, crID, oldPID); err != nil {
			return err
		}
	}

	if newPID == "" {
		return nil
	}

	if toStatus == "archived" {
		return s.completeRun(ctx, workspaceID, crID, newPID)
	}

	runID, err := s.findOrCreateRun(ctx, workspaceID, crID, newPID)
	if err != nil {
		return err
	}

	action, ok := statusGateAction[toStatus]
	if !ok {
		// "merging"/"writing-back": pipeline is open (run exists) but no node
		// action for this specific status — nothing further to do.
		return nil
	}
	node, ok := ApprovalGateNodes[action.Stage]
	if !ok {
		return nil
	}
	switch action.Status {
	case "running":
		return s.upsertNodeRunning(ctx, runID, node)
	case "passed":
		return s.markNodePassed(ctx, runID, node, crID, action.Stage)
	}
	return nil
}

// findOrCreateRun returns the id of the current non-terminal pipeline_run for
// (workspace, cr, pipeline), creating one if none exists yet.
func (s *SyncService) findOrCreateRun(ctx context.Context, workspaceID, crID, pipelineID string) (string, error) {
	var runID string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM pipeline_run
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND pipeline_id = $3
		  AND status IN ('running', 'waiting_approval')
		ORDER BY created_at DESC LIMIT 1`,
		workspaceID, crID, pipelineID).Scan(&runID)
	if err == nil {
		return runID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// started_by is NOT NULL (P0 §3.4 assumes a Runner-initiated run with a
	// real actor). The projector runs off a crctl event, not a user request,
	// so there is no real actor to attribute — fall back to the workspace's
	// earliest owner rather than leaving the column unattributable.
	var startedBy string
	if err := s.pool.QueryRow(ctx, `
		SELECT user_id::text FROM member WHERE workspace_id = $1::uuid AND role = 'owner'
		ORDER BY created_at ASC LIMIT 1`, workspaceID).Scan(&startedBy); err != nil {
		return "", err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, status, started_by)
		VALUES ($1::uuid, $2, $3, 'running', $4::uuid)
		RETURNING id::text`,
		workspaceID, pipelineID, crID, startedBy).Scan(&runID)
	if err == nil {
		return runID, nil
	}
	// CR-2026-045: Runner Start and projector may race to open the same
	// architecture run. The partial unique index chooses one winner; the
	// projector loser must re-read it instead of dropping the projection.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return "", err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT id::text FROM pipeline_run
		WHERE workspace_id=$1::uuid AND cr_id=$2 AND pipeline_id=$3
		  AND status IN ('running','waiting_approval')
		ORDER BY created_at DESC LIMIT 1`, workspaceID, crID, pipelineID).Scan(&runID)
	return runID, err
}

// upsertNodeRunning marks a node running (attempt 1 — approval nodes are not
// subject to reviewLoop retry) and, if the run has no other running node,
// promotes the run to waiting_approval (human_approval semantics).
func (s *SyncService) upsertNodeRunning(ctx context.Context, runID string, node GateNode) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pipeline_node_run (run_id, node_id, ref, kind, seq, status, started_at)
		VALUES ($1::uuid, $2::uuid, NULL, $3, $4, 'running', now())
		ON CONFLICT (run_id, node_id, attempt) DO UPDATE
		  SET started_at = COALESCE(pipeline_node_run.started_at, now())
		  WHERE pipeline_node_run.status IN ('pending','running')`,
		runID, node.NodeID, node.Kind, node.Seq)
	if err != nil {
		return err
	}
	if node.Kind == "human_approval" {
		_, err = s.pool.Exec(ctx, `
			UPDATE pipeline_run SET status = 'waiting_approval' WHERE id = $1::uuid AND status = 'running'`,
			runID)
	}
	return err
}

// markNodePassed marks a node passed and, for human_approval nodes, links the
// most recent approving approval_record for (cr_id, stage) — the same
// evidence_digest that unblocked it. Also demotes the run back to running
// (the approval gate is clear; the pipeline's next skill node takes over,
// which this projector does not track — only gate nodes are projected).
func (s *SyncService) markNodePassed(ctx context.Context, runID string, node GateNode, crID, stage string) error {
	var approvalID *string
	if node.Kind == "human_approval" {
		var id string
		err := s.pool.QueryRow(ctx, `
			SELECT id::text FROM approval_record
			WHERE cr_id = $1 AND stage = $2 AND decision = 'approve'
			ORDER BY created_at DESC LIMIT 1`, crID, stage).Scan(&id)
		if err == nil {
			approvalID = &id
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pipeline_node_run (run_id, node_id, ref, kind, seq, status, started_at, completed_at, approval_id)
		VALUES ($1::uuid, $2::uuid, NULL, $3, $4, 'passed', now(), now(), $5::uuid)
		ON CONFLICT (run_id, node_id, attempt) DO UPDATE
		  SET status = 'passed', completed_at = now(),
		      approval_id = COALESCE($5::uuid, pipeline_node_run.approval_id)`,
		runID, node.NodeID, node.Kind, node.Seq, approvalID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE pipeline_run SET status = 'running' WHERE id = $1::uuid AND status = 'waiting_approval'`,
		runID)
	return err
}

// completeRun marks the current non-terminal run for (cr, pipeline) completed.
func (s *SyncService) completeRun(ctx context.Context, workspaceID, crID, pipelineID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE pipeline_run SET status = 'completed', completed_at = now()
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND pipeline_id = $3
		  AND status IN ('running', 'waiting_approval')`,
		workspaceID, crID, pipelineID)
	return err
}

// cancelActiveRuns handles rejected/withdrawn: every non-terminal run for this
// CR is cancelled, and any node still running inside them fails.
func (s *SyncService) cancelActiveRuns(ctx context.Context, workspaceID, crID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE pipeline_node_run SET status = 'failed', completed_at = now()
		WHERE status = 'running' AND run_id IN (
			SELECT id FROM pipeline_run
			WHERE workspace_id = $1::uuid AND cr_id = $2 AND status IN ('running', 'waiting_approval')
		)`, workspaceID, crID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE pipeline_run SET status = 'cancelled', completed_at = now()
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND status IN ('running', 'waiting_approval')`,
		workspaceID, crID)
	return err
}

// reviewEventPayload mirrors the JSON built by the daemon's
// buildReviewPayload (crevents.go, TASK-03).
type reviewEventPayload struct {
	Stage   string `json:"stage"`
	Verdict string `json:"verdict"`
	Attempt int    `json:"attempt"`
}

// applyReview projects a review-verdict event (TASK-03's fifth commit-scan
// contract) onto the review skill node's pipeline_node_run row. Unlike
// approval-node projection, each reviewLoop round gets its OWN row —
// attempt is part of the table's uniqueness key (P0 §3.4) — so a blocked
// round 1 followed by a passing round 2 leaves both rows queryable, matching
// "reviewLoop attempt N/3" (D7 FR-5) needing the whole history, not just the
// latest verdict.
//
// This does not touch cr.status or pipeline_run.status: a blocked review
// does not advance (or regress) the CR — the status machine's own
// review-requirement:block -> write-requirement-prd transition is a
// separate "[cr] status" event on a different commit. Publishing cr:updated
// here (SDD DD-6) is purely a UI refresh signal for the blocked-list card.
func (s *SyncService) applyReview(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	var p reviewEventPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	node, ok := ReviewGateNodes[p.Stage]
	if !ok {
		return nil // dev-start has no review node; unknown stage: nothing to project
	}
	runID, err := s.findOrCreateRun(ctx, workspaceID, ev.CRID, node.PipelineID)
	if err != nil {
		return err
	}
	status := "passed"
	if p.Verdict == "block" {
		status = "blocked"
	}
	attempt := p.Attempt
	if attempt < 1 {
		attempt = 1
	}
	detail := ev.Payload
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var payloadKeys map[string]any
	if err := json.Unmarshal(detail, &payloadKeys); err != nil {
		return err
	}
	if _, reserved := payloadKeys["runner"]; reserved {
		return errors.New("review payload contains reserved runner key")
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO pipeline_node_run (run_id, node_id, ref, kind, seq, status, attempt, started_at, completed_at, detail)
		VALUES ($1::uuid, $2::uuid, NULL, $3, $4, $5, $6, now(), now(), $7)
		ON CONFLICT (run_id, node_id, attempt) DO UPDATE
		  SET status = $5, completed_at = now(),
		      detail = COALESCE(pipeline_node_run.detail,'{}') || $7::jsonb`,
		runID, node.NodeID, node.Kind, node.Seq, status, attempt, detail); err != nil {
		return err
	}
	s.publish(ctx, workspaceID, ev.CRID)
	return nil
}
