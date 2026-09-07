-- AIFIRST: CR-2026-049 TASK-05: drop the pre-workspace global idempotency key;
-- the workspace-scoped unique index from 391 is already in place.
ALTER TABLE cr_sync_event DROP CONSTRAINT cr_sync_event_cr_id_commit_sha_event_kind_key;
