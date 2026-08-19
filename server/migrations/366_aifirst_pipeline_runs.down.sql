-- AIFIRST: roll back pipeline runner state tables (CR-2026-011 TASK-01).
-- Projections are safe to drop: git is the authority; replaying events rebuilds everything.
ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS pipeline_node_run_id,
    DROP COLUMN IF EXISTS cr_id;

DROP TABLE IF EXISTS pipeline_node_run;
DROP TABLE IF EXISTS pipeline_run;
