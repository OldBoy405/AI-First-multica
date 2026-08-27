-- AIFIRST: CR-2026-052 TASK-01 rollback (TD-BL-10). Drop the CHECK first, then
-- the carrier column (471's index on this column must already be gone).
ALTER TABLE agent_task_queue
  DROP CONSTRAINT IF EXISTS agent_task_queue_approval_workspace_ck,
  DROP COLUMN IF EXISTS approval_workspace_id;
