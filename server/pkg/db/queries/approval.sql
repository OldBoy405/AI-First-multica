-- AIFIRST: CR-2026-052 TASK-02 (SDD §3.3/§4.2/§4.3) — approval-continuation
-- sqlc queries. All reads/writes are workspace-qualified by the authenticated
-- daemon workspace ($ws / @workspace_id); there is NO workspace-less fallback
-- path, so two tenants with the same CR name can never collide or leak (TD-BL-10).

-- name: AckApprovalGrants :many
-- AIFIRST: CR-2026-052 (FR-3, SDD §4.1). First step of HandleGrantsAck: mark
-- the daemon-delivered grants as delivered and return the five fields the
-- continuation needs. Idempotent — a replayed ACK matches 0 rows (delivered_at
-- already set) and the caller returns 200.
UPDATE approval_record
SET delivered_at = now()
WHERE workspace_id = @workspace_id::uuid
  AND id::text = ANY(@ids::text[])
  AND delivered_at IS NULL
RETURNING id::text, cr_id, stage, decision, approver_user_id::text;

-- name: GetCrShellIssueInWorkspaceForShare :one
-- AIFIRST: CR-2026-052 (TD-BL-5/8/10, SDD §4.2). Locks the CR authority row for
-- the authenticated workspace with FOR SHARE — the weakest lock that conflicts
-- with an ordinary non-key UPDATE (FOR NO KEY UPDATE) and with row DELETE
-- (FOR UPDATE), so concurrent crsync shell_issue_id/status projection writes or
-- a shell_issue_id upsert either commit before we read (we see the new value)
-- or block until our tx commits (we dispatched the ACK-time authority). 0 rows
-- → workspace-mismatch (FR-7 fail-closed).
SELECT * FROM cr
WHERE workspace_id = @workspace_id::uuid AND cr_id = @cr_id
FOR SHARE;

-- name: CreateApprovalContinuationTask :one
-- AIFIRST: CR-2026-052 (FR-1/FR-4/FR-6/FR-7, SDD §3.3/§4.3). Guarded INSERT:
-- the row is written only if the FULL authority chain (cr → issue → squad →
-- agent) all belong to the same authenticated workspace and the squad's leader
-- is the resolved agent. Any guard failure inserts 0 rows (pgx.ErrNoRows) and
-- the caller escalates to the merge/re-read/defer ladders — never silently
-- degrades (discipline 1). status ('queued'/'deferred') and fire_at are
-- parameterized so the same query serves the normal insert and the 257-slot
-- deferred fallback. ON CONFLICT DO NOTHING lets 469/471 losers re-read.
-- Approval_workspace_id is the tenant carrier (migration 470); it never enters
-- prompt/context (FR-9). is_leader_task=true matches the daemon squad-brief
-- injection contract (migration 127). originator_source='direct_human' (DD-7).
INSERT INTO agent_task_queue (
    agent_id, runtime_id, approval_workspace_id, issue_id, status, priority, fire_at,
    trigger_summary, squad_id, is_leader_task, handoff_note, context,
    originator_user_id, accountable_user_id, originator_source,
    trigger_evidence_kind, trigger_evidence_ref_id, cr_id, project_id
)
SELECT
    a.id, a.runtime_id,
    @workspace_id::uuid,
    @issue_id::uuid,
    @status,
    @priority,
    sqlc.narg('fire_at'),
    @trigger_summary,
    s.id,
    TRUE,
    @handoff_note,
    @context,
    @approver_user_id::uuid,
    @approver_user_id::uuid,
    'direct_human',
    'approval_continuation',
    @record_id::uuid,
    @cr_id,
    sqlc.narg('project_id')
FROM cr c
JOIN issue i ON i.id = @issue_id::uuid AND i.workspace_id = @workspace_id::uuid
JOIN squad s ON s.id = i.assignee_id AND s.workspace_id = @workspace_id::uuid
             AND s.leader_id = a.id AND s.archived_at IS NULL
JOIN agent a ON a.id = @agent_id::uuid AND a.workspace_id = @workspace_id::uuid
             AND a.archived_at IS NULL AND a.runtime_id IS NOT NULL AND a.kind = 'user'
WHERE c.workspace_id = @workspace_id::uuid AND c.cr_id = @cr_id AND c.shell_issue_id = i.id
ON CONFLICT DO NOTHING
RETURNING *;

-- name: AppendApprovalContinuationEvidence :one
-- AIFIRST: CR-2026-052 (TD-BL-9/10/11, SDD §4.3 ladder 2). Atomically append one
-- approval's four fields to a locked (queued/deferred) successor row. The row is
-- already FOR UPDATE-locked by GetMergeableApprovalContinuationTaskByWorkspaceAndCrForUpdate,
-- so no status predicate is needed here; the NOT EXISTS guard makes a same-record
-- replay a 0-row no-op (→ caller maps to already-queued, never double-appends).
-- approvals[] is the machine-readable idempotency/audit key; handoff_note is the
-- prompt carrier (migration 122). Both workspace_id and kind re-verified to
-- defend against cross-tenant writes.
UPDATE agent_task_queue
SET context = jsonb_set(
        COALESCE(context, '{}'::jsonb), '{approvals}',
        COALESCE(context -> 'approvals', '[]'::jsonb) || @new_entry::jsonb
      ),
    handoff_note = COALESCE(handoff_note, '') || E'\n' || @new_line,
    updated_at = now()
WHERE id = @successor_id::uuid
  AND approval_workspace_id = @workspace_id::uuid
  AND trigger_evidence_kind = 'approval_continuation'
  AND NOT (
        COALESCE(context -> 'approvals', '[]'::jsonb) @>
        jsonb_build_array(jsonb_build_object('approval_record_id', @record_id::text))
      )
RETURNING *;

-- name: GetApprovalContinuationTaskByRecord :one
-- AIFIRST: CR-2026-052 (FR-4, SDD §4.3 ladder 1). Idempotent re-read keyed by the
-- 469 record-id index, defensively workspace-scoped so a cross-tenant ACK can
-- never read back another tenant's task. Covers all five active states.
SELECT * FROM agent_task_queue
WHERE approval_workspace_id = @workspace_id::uuid
  AND trigger_evidence_kind = 'approval_continuation'
  AND trigger_evidence_ref_id = @record_id::uuid
  AND status IN ('queued', 'deferred', 'dispatched', 'waiting_local_directory', 'running');

-- name: GetMergeableApprovalContinuationTaskByWorkspaceAndCrForUpdate :one
-- AIFIRST: CR-2026-052 (TD-BL-9/10/11, SDD §4.3 ladder 2). Selects the single
-- prompt-not-yet-snapshotted successor for this (workspace, cr) and locks it
-- FOR UPDATE so a concurrent claim cannot dispatch it between select and merge.
-- Predicate is ONLY queued/deferred: dispatched/waiting_local_directory/running
-- already snapshotted handoff at claim time and must NOT be merged — a new
-- approval creates an independent successor instead (TD-BL-11). 0 rows means no
-- mergeable successor exists (none yet, or only an in-flight predecessor).
SELECT * FROM agent_task_queue
WHERE approval_workspace_id = @workspace_id::uuid
  AND trigger_evidence_kind = 'approval_continuation'
  AND cr_id = @cr_id
  AND status IN ('queued', 'deferred')
FOR UPDATE;
