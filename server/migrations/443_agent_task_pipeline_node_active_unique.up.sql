-- AIFIRST: Runner Core task uniqueness guard (CR-2026-045 TASK-04; SDD §3.3).
-- At most one active task per pipeline node run. The Runner enqueues at most one task
-- per reconcile, but event re-delivery / restart races can double-enqueue; this partial
-- unique index makes that a DB unique violation (loser path re-reads) instead of two
-- effective tasks. Terminal parent tasks are not covered by the WHERE clause, so the
-- existing retry path can still clone a child after the parent completes.
--
-- Single-statement migration: CREATE INDEX CONCURRENTLY cannot run inside a transaction
-- or multi-command string (repo convention).
--
-- This migration is AI-First fork custom code (tracked in CUSTOM.md); keep the number
-- sequence intact when rebasing onto upstream.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_pipeline_node_active
    ON agent_task_queue (pipeline_node_run_id)
    WHERE pipeline_node_run_id IS NOT NULL
      AND status IN ('queued', 'deferred', 'dispatched', 'waiting_local_directory', 'running');
