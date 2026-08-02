-- CR-2026-008 (P2 三模式聊天 CR-C): give chat_session an optional project
-- dimension for the Private Ask pane. A project-bound session is a per-member
-- private Q&A sandbox scoped to one project; the existing global 1:1 chat
-- keeps project_id NULL and is untouched by this migration (zero row rewrites,
-- nullable column + partial indexes only).
ALTER TABLE chat_session
    ADD COLUMN project_id UUID REFERENCES project(id) ON DELETE CASCADE;

-- Lookup path for the pane's get-or-create: latest active session per
-- (project, creator).
CREATE INDEX idx_chat_session_project
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL;

-- At most one *active* Private Ask session per (project, creator): concurrent
-- get-or-create (two tabs opening the pane at once) collapses to a single row
-- via insert-conflict + reselect. Archived sessions fall out of the predicate,
-- so archiving and re-entering the pane starts a fresh session.
CREATE UNIQUE INDEX chat_session_project_creator_active_unique
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL AND status = 'active';
