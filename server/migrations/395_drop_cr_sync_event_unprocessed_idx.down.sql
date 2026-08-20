-- AIFIRST: CR-2026-049 TASK-05 rollback.
CREATE INDEX CONCURRENTLY idx_cr_sync_event_unprocessed
    ON cr_sync_event (cr_id, received_at) WHERE processed_at IS NULL;
