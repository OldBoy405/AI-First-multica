package service

// AIFIRST: CR-2026-052 TASK-03 (SDD §1.3/§4.1/§4.3) — approval-continuation
// enqueue on TaskService. Two methods:
//   - EnqueueApprovalContinuation: transaction-scoped pure DB write implementing
//     the four-rung idempotency ladder (successor-enqueued → already-queued →
//     merged → slot-deferred; never silently degrades — discipline 1).
//   - NotifyContinuationTaskEnqueued: post-commit event broadcast for the
//     newly-created rows only (merged/already-queued rows were not created by
//     this ACK and must not be re-broadcast — TD-SUG-1, parity with
//     EnqueuePipelineTask's tail at task.go:415-416).
//
// The methods deliberately do NOT read or write any controlled ledger
// (_index.yml etc.); those are crctl-owned. All DB access is workspace-scoped
// via the spec's WorkspaceID (TD-BL-10).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// EnqueueOutcome names the rung of the idempotency ladder that handled an ACK.
// Only successor-enqueued and slot-deferred produce a newly-created row that
// the caller must broadcast post-commit; already-queued and merged reuse an
// existing row (SDD §7.3 reason codes).
type EnqueueOutcome string

const (
	OutcomeAlreadyQueued     EnqueueOutcome = "already-queued"
	OutcomeMerged            EnqueueOutcome = "merged"
	OutcomeSuccessorEnqueued EnqueueOutcome = "successor-enqueued"
	OutcomeSlotDeferred      EnqueueOutcome = "slot-deferred"
)

// ApprovalContinuationSpec carries the resolved continuation target plus the
// approval fields, all scoped to the authenticated daemon workspace. CrID is
// the CR identifier verbatim (e.g. "CR-2026-052") — it is never re-prefixed
// (TD-SUG-3). Priority is computed by the caller from the locked issue's
// priority (priorityToInt), so this method stays a pure DB writer.
type ApprovalContinuationSpec struct {
	WorkspaceID pgtype.UUID // authenticated daemon workspace → approval_workspace_id carrier
	AgentID     pgtype.UUID // resolved CR leader agent
	RuntimeID   pgtype.UUID // leader agent's runtime (informational; INSERT SELECTs a.runtime_id)
	IssueID     pgtype.UUID // cr.shell_issue_id
	SquadID     pgtype.UUID // issue.assignee_id (squad)
	ProjectID   pgtype.UUID // cr.project_id (nullable)
	CrID        string      // CR identifier verbatim
	RecordID    string      // approval_record.id (text form, as returned by AckApprovalGrants)
	Stage       string      // requirement | tech-design | dev-start | code
	Decision    string      // approve | reject
	ApproverID  pgtype.UUID // approver_user_id → originator + accountable (DD-7)
	Priority    int32       // priorityToInt(issue.Priority)
}

// EnqueueApprovalContinuation inserts/merges an approval-continuation task
// inside the caller's transaction (qtx is the tx-bound *db.Queries). It is a
// pure DB write — no event broadcast; the caller broadcasts post-commit via
// NotifyContinuationTaskEnqueued for newly-created rows only.
func (s *TaskService) EnqueueApprovalContinuation(ctx context.Context, qtx *db.Queries, spec ApprovalContinuationSpec) (db.AgentTaskQueue, EnqueueOutcome, error) {
	ws := spec.WorkspaceID
	recordUUID, err := util.ParseUUID(spec.RecordID)
	if err != nil {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: invalid record id %q: %w", spec.RecordID, err)
	}

	handoff := fmt.Sprintf(
		"%s 的 %s 审批已 %s（approval_record_id=%s）。请读取 .crctl/grants/ 与 crctl status/next 确定下一步；本提示不携带任何状态→下一步映射。",
		spec.CrID, spec.Stage, spec.Decision, spec.RecordID,
	)
	summary := fmt.Sprintf("%s approval %s: %s", spec.CrID, spec.Stage, spec.Decision)

	entryJSON, err := json.Marshal(map[string]string{
		"cr_id":              spec.CrID,
		"stage":              spec.Stage,
		"decision":           spec.Decision,
		"approval_record_id": spec.RecordID,
	})
	if err != nil {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: marshal entry: %w", err)
	}
	contextJSON, err := json.Marshal(map[string]any{
		"type":      "approval_continuation",
		"schema":    "ai-first.approval-continuation/v1",
		"approvals": []json.RawMessage{entryJSON},
	})
	if err != nil {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: marshal context: %w", err)
	}

	crText := pgtype.Text{String: spec.CrID, Valid: true}
	summaryText := pgtype.Text{String: summary, Valid: true}
	handoffText := pgtype.Text{String: handoff, Valid: true}

	// Rung 0: guarded INSERT as a queued successor. ON CONFLICT DO NOTHING →
	// 469/471 losers and guard-failure both yield pgx.ErrNoRows.
	task, err := qtx.CreateApprovalContinuationTask(ctx, db.CreateApprovalContinuationTaskParams{
		WorkspaceID:    ws,
		IssueID:        spec.IssueID,
		Status:         "queued",
		Priority:       spec.Priority,
		FireAt:         pgtype.Timestamptz{}, // NULL for queued
		TriggerSummary: summaryText,
		HandoffNote:    handoffText,
		Context:        contextJSON,
		ApproverUserID: spec.ApproverID,
		RecordID:       recordUUID,
		CrID:           crText,
		ProjectID:      spec.ProjectID,
		AgentID:        spec.AgentID,
	})
	if err == nil {
		return task, OutcomeSuccessorEnqueued, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: create queued task: %w", err)
	}

	// Rung 1: idempotent re-read by record id (469 index), workspace-scoped.
	existing, err := qtx.GetApprovalContinuationTaskByRecord(ctx, db.GetApprovalContinuationTaskByRecordParams{
		WorkspaceID: ws,
		RecordID:     recordUUID,
	})
	if err == nil {
		return existing, OutcomeAlreadyQueued, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: read by record: %w", err)
	}

	// Rung 2: merge into the single prompt-not-yet-snapshotted (queued/deferred)
	// successor for this (workspace, cr), FOR UPDATE-locked.
	succ, err := qtx.GetMergeableApprovalContinuationTaskByWorkspaceAndCrForUpdate(ctx,
		db.GetMergeableApprovalContinuationTaskByWorkspaceAndCrForUpdateParams{
			WorkspaceID: ws,
			CrID:        crText,
		})
	if err == nil {
		merged, mErr := qtx.AppendApprovalContinuationEvidence(ctx, db.AppendApprovalContinuationEvidenceParams{
			NewEntry:    entryJSON,
			NewLine:     handoffText,
			SuccessorID: succ.ID,
			WorkspaceID: ws,
			RecordID:    spec.RecordID,
		})
		if mErr == nil {
			return merged, OutcomeMerged, nil
		}
		if errors.Is(mErr, pgx.ErrNoRows) {
			// 0 rows ⟺ the record was already in approvals[] (NOT EXISTS guard
			// fired). The locked successor is the carrier; treat as already-queued.
			return succ, OutcomeAlreadyQueued, nil
		}
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: append evidence: %w", mErr)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: read mergeable successor: %w", err)
	}

	// Rung 3: no mergeable successor (none yet, or only an in-flight predecessor
	// whose prompt is already snapshotted). Fall back to a deferred row outside
	// the 257 predicate so it coexists with an occupying ordinary task and is
	// promoted to queued once the slot frees (PromoteDueDeferredTasksForRuntime).
	task, err = qtx.CreateApprovalContinuationTask(ctx, db.CreateApprovalContinuationTaskParams{
		WorkspaceID:    ws,
		IssueID:        spec.IssueID,
		Status:         "deferred",
		Priority:       spec.Priority,
		FireAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		TriggerSummary: summaryText,
		HandoffNote:    handoffText,
		Context:        contextJSON,
		ApproverUserID: spec.ApproverID,
		RecordID:       recordUUID,
		CrID:           crText,
		ProjectID:      spec.ProjectID,
		AgentID:        spec.AgentID,
	})
	if err == nil {
		return task, OutcomeSlotDeferred, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// All rungs exhausted and the deferred insert still conflicted (e.g. a
		// concurrent winner under 471 between rung 2's 0-row read and here).
		// Fail hard — never silently degrade (discipline 1).
		return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: idempotency ladder exhausted (record %s, cr %s)", spec.RecordID, spec.CrID)
	}
	return db.AgentTaskQueue{}, "", fmt.Errorf("approval continuation: create deferred task: %w", err)
}

// NotifyContinuationTaskEnqueued broadcasts the task:queued event + daemon
// availability kick for a newly-created approval-continuation row. Call only
// for OutcomeSuccessorEnqueued / OutcomeSlotDeferred rows, post-commit, so the
// event publishes only after the row is durably visible (DD-5/§3.2). Parity
// with EnqueuePipelineTask's broadcast tail (task.go:415-416).
func (s *TaskService) NotifyContinuationTaskEnqueued(ctx context.Context, task db.AgentTaskQueue) error {
	if s == nil {
		return nil
	}
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
	return nil
}
