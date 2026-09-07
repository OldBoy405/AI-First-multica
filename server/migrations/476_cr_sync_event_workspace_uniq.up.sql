-- AIFIRST: CR-2026-049 TASK-05: workspace-scoped event idempotency key
-- (workspace_id, cr_id, commit_sha, event_kind). Built before the old global
-- key is dropped in 394 (new-index-before-old-drop, SDD §2.5).
CREATE UNIQUE INDEX CONCURRENTLY cr_sync_event_workspace_dedup_idx
    ON cr_sync_event (workspace_id, cr_id, commit_sha, event_kind);
