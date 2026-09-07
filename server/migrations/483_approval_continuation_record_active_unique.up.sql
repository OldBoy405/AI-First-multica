-- AIFIRST: CR-2026-052 TASK-01 (FR-4, SDD §2.2): approval_continuation record-id
-- idempotency. At most one active (queued/deferred/dispatched/waiting_local_directory/
-- running) agent_task_queue row may reference a given approval_record.id as its
-- trigger_evidence_ref_id. A concurrent ACK loser hits ON CONFLICT DO NOTHING and
-- re-reads the existing row (already-queued); no second task, no 5xx (FR-1/FR-4/AC-1).
-- Single statement: CREATE INDEX CONCURRENTLY cannot run in a transaction.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_approval_continuation_record_active
    ON agent_task_queue (trigger_evidence_ref_id)
    WHERE trigger_evidence_kind = 'approval_continuation'
      AND status IN ('queued', 'deferred', 'dispatched', 'waiting_local_directory', 'running');
