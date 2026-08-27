-- AIFIRST: CR-2026-052 TASK-01 (FR-6/TD-BL-10/11, SDD §2.3): workspace-qualified
-- pending-successor uniqueness. Within one authenticated daemon workspace, at
-- most one approval_continuation task whose prompt has NOT yet been snapshotted
-- (status queued/deferred) may exist per cr_id. Two workspaces with the same CR
-- name have distinct (approval_workspace_id, cr_id) keys and never collide or
-- leak across tenants. dispatched/waiting_local_directory/running are excluded:
-- their prompt is already snapshotted at claim time, so a new approval must
-- create an independent queued/deferred successor (TD-BL-11) rather than merge.
-- Single statement: CREATE INDEX CONCURRENTLY cannot run in a transaction.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_approval_continuation_workspace_cr_pending
    ON agent_task_queue (approval_workspace_id, cr_id)
    WHERE trigger_evidence_kind = 'approval_continuation'
      AND status IN ('queued', 'deferred');
