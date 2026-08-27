-- AIFIRST: CR-2026-052 TASK-01 (TD-BL-10, SDD §2.3): agent_task_queue gains a
-- nullable approval_workspace_id carrier that records the authenticated daemon
-- workspace which enqueued an approval_continuation task. It is NOT a global
-- workspace column for all tasks (ordinary tasks stay NULL) and carries no FK
-- (repo hard rule) — the CHECK below forces it to be non-NULL exactly for
-- approval_continuation rows, and the guarded INSERT (approval.sql) re-verifies
-- the full authority chain (agent/issue/squad/cr) all belong to this same $ws.
-- migration 471 then builds the (approval_workspace_id, cr_id) partial unique
-- index on top of it. ADD COLUMN is nullable → no table rewrite / long lock.
ALTER TABLE agent_task_queue
  ADD COLUMN approval_workspace_id UUID,
  ADD CONSTRAINT agent_task_queue_approval_workspace_ck
  CHECK (trigger_evidence_kind IS DISTINCT FROM 'approval_continuation'
         OR approval_workspace_id IS NOT NULL);
