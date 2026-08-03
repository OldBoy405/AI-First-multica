-- CR-2026-010: supports the project-scoped NOT EXISTS branch added to
-- ClaimAgentTask (agent.sql) — probing "is there an active task for this
-- project" must not degrade into a sequential scan on agent_task_queue.
--
-- CONCURRENTLY because agent_task_queue is hot (see 080's note: a plain
-- CREATE INDEX takes ACCESS EXCLUSIVE and blocks the dispatch path); the
-- migration runner cannot mix CONCURRENTLY with other statements in the
-- same file (068), so this is its own single-statement migration.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_atq_project_active
    ON agent_task_queue (project_id)
    WHERE status IN ('dispatched', 'running', 'waiting_local_directory')
      AND project_id IS NOT NULL;
