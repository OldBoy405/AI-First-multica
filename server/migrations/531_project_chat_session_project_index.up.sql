-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.6): plain (workspace, project) index
-- covering closed history rows too (COUNT/listing paths).
CREATE INDEX CONCURRENTLY project_chat_session_project_index
    ON project_chat_session (workspace_id, project_id);
