-- AIFIRST: CR-2026-049 TASK-05 rollback.
CREATE UNIQUE INDEX CONCURRENTLY approval_record_approve_uniq
    ON approval_record (cr_id, stage, evidence_digest)
    WHERE decision = 'approve';
