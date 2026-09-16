-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.1/§2.6): container issues are now keyed
-- by their session origin. New containers always write origin_id = session.id
-- (AC-18); legacy project_chat rows (origin_id NULL, migration 435 era) are
-- excluded by the partial predicate, so this index builds with zero conflicts.
CREATE UNIQUE INDEX CONCURRENTLY issue_project_chat_session_origin_uidx
    ON issue (workspace_id, origin_id)
    WHERE origin_type = 'project_chat' AND origin_id IS NOT NULL;
