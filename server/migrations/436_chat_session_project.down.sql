DROP INDEX IF EXISTS chat_session_project_creator_active_unique;
DROP INDEX IF EXISTS idx_chat_session_project_creator;
ALTER TABLE chat_session DROP CONSTRAINT IF EXISTS chat_session_project_fk;
-- The project_id column itself is upstream's (214_chat_session_project);
-- reverting this fork migration leaves it in place.
