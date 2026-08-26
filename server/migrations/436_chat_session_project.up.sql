-- CR-2026-008 (P2 three-mode chat CR-C): the project_id column itself
-- arrives with upstream migration 214_chat_session_project (soft reference).
-- This fork migration adds the constraints the Private Ask pane relies on:
-- the FK cascade (fork keeps the hard reference upstream deliberately avoids),
-- the (project, creator) lookup index (renamed from the pre-upstream-215 name
-- to dodge upstream's idx_chat_session_project), and the one-active-session
-- unique index that makes concurrent get-or-create collapse to a single row.
ALTER TABLE chat_session
    ADD CONSTRAINT chat_session_project_fk
    FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_chat_session_project_creator
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL;

-- At most one *active* Private Ask session per (project, creator): concurrent
-- get-or-create (two tabs opening the pane at once) collapses to a single row
-- via insert-conflict + reselect. Archived sessions fall out of the predicate,
-- so archiving and re-entering the pane starts a fresh session.
CREATE UNIQUE INDEX IF NOT EXISTS chat_session_project_creator_active_unique
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL AND status = 'active';
