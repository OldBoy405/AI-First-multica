-- AIFIRST: CR-2026-049 TASK-05 rollback: restore the old global key (391 down
-- drops the workspace-scoped replacement, so the table is free of duplicates).
ALTER TABLE cr_sync_event ADD CONSTRAINT cr_sync_event_cr_id_commit_sha_event_kind_key
    UNIQUE (cr_id, commit_sha, event_kind);
