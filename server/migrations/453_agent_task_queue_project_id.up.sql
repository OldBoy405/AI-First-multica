-- CR-2026-010 (P2 三模式聊天 CR-E): stamp a redundant project_id onto
-- agent_task_queue so the claim path can serialize project-shared (Team
-- Agent) tasks without joining issue on every claim attempt (hot path, see
-- 067/125 for the existing claim-candidate index precedent).
--
-- Backfill is scoped to non-terminal rows only: claim only ever reads
-- project_id for rows in ('dispatched','running','waiting_local_directory')
-- (see migration 162's partial index), so terminal rows never need it and
-- backfilling them would just add lock time / write volume on a hot table
-- for no read benefit.
ALTER TABLE agent_task_queue
    ADD COLUMN project_id UUID REFERENCES project(id) ON DELETE SET NULL;

UPDATE agent_task_queue atq
SET project_id = i.project_id
FROM issue i
WHERE atq.issue_id = i.id
  AND i.project_id IS NOT NULL
  AND atq.status NOT IN ('completed', 'failed', 'cancelled');
