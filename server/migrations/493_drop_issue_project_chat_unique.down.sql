-- AIFIRST: CR-2026-056 TASK-02 rollback (SDD §2.6): restore the pre-479
-- per-project uniqueness exactly as migration 435 created it.
CREATE UNIQUE INDEX CONCURRENTLY issue_project_chat_unique
    ON issue (project_id)
    WHERE origin_type = 'project_chat';
