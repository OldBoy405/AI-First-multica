-- AIFIRST: CR-2026-049 TASK-05 rollback (runs after 391-397 down have dropped
-- the workspace-scoped indexes).
ALTER TABLE cr_sync_event DROP COLUMN workspace_id;
