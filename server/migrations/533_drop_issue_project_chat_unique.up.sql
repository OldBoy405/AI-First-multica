-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.1/§2.6, AC-22): retire the per-project
-- uniqueness of project_chat container issues (migration 435). Sessions can
-- rebind to new containers, so a project may hold several project_chat rows
-- across history; migration 480 replaces the uniqueness on the issue side by
-- (workspace_id, origin_id). GetProjectChatIssue must ORDER BY
-- created_at ASC, id ASC LIMIT 1 (SDD §4.13, BLOCK-017) — see TASK-07.
DROP INDEX CONCURRENTLY IF EXISTS issue_project_chat_unique;
