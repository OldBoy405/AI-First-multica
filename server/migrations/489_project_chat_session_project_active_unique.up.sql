-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.1/§2.6): at most one active session
-- per (workspace, project); closed history rows are excluded so rebinds can
-- create a new active row.
CREATE UNIQUE INDEX CONCURRENTLY project_chat_session_project_active_unique
    ON project_chat_session (workspace_id, project_id)
    WHERE status = 'active';
