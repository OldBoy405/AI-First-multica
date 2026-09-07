-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.4, FR-3): at most one ACTIVE shared
-- Discussion session per (workspace, project). project_id is a soft reference
-- (migration 214, no FK); project deletion clears it via
-- ClearChatSessionProjectByProject and the row falls out of the predicate.
CREATE UNIQUE INDEX CONCURRENTLY chat_session_project_shared_active_unique
    ON chat_session (workspace_id, project_id)
    WHERE kind = 'project_shared' AND status = 'active';
