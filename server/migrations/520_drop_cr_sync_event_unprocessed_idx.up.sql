-- AIFIRST: CR-2026-049 TASK-05: drop the pre-workspace unprocessed index;
-- cr_sync_event_ws_unprocessed_idx (393) covers it with workspace scoping.
DROP INDEX CONCURRENTLY idx_cr_sync_event_unprocessed;
