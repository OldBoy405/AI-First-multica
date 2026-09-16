-- AIFIRST: CR-2026-049 TASK-05: workspace-scoped approve idempotency index,
-- replacing approval_record_approve_uniq (dropped in 397).
CREATE UNIQUE INDEX CONCURRENTLY approval_record_approve_ws_uniq
    ON approval_record (workspace_id, cr_id, stage, evidence_digest)
    WHERE decision = 'approve';
