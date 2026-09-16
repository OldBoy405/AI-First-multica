-- AIFIRST: Runner Core uniqueness guard (CR-2026-045 TASK-04; SDD §3.3).
-- At most one non-terminal architecture run per (workspace, pipeline, cr). The Runner
-- Start upsert and the gate-node projector find/create both race to create the first
-- run; this partial unique index makes the loser path a DB unique violation that is
-- re-read instead of producing a second run.
--
-- Single-statement migration: CREATE INDEX CONCURRENTLY cannot run inside a transaction
-- or multi-command string (repo convention, see 080/114).
--
-- This migration is AI-First fork custom code (tracked in CUSTOM.md); keep the number
-- sequence intact when rebasing onto upstream.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_pipeline_run_architecture_active_cr
    ON pipeline_run (workspace_id, pipeline_id, cr_id)
    WHERE cr_id IS NOT NULL AND status IN ('running', 'waiting_approval');
