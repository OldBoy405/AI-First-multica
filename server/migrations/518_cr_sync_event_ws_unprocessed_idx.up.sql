-- AIFIRST: CR-2026-049 TASK-05: workspace-scoped unprocessed scan index,
-- replacing idx_cr_sync_event_unprocessed (dropped in 395).
CREATE INDEX CONCURRENTLY cr_sync_event_ws_unprocessed_idx
    ON cr_sync_event (workspace_id, cr_id, received_at)
    WHERE processed_at IS NULL;
