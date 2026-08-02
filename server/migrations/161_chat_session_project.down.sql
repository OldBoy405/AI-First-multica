DROP INDEX IF EXISTS chat_session_project_creator_active_unique;
DROP INDEX IF EXISTS idx_chat_session_project;
ALTER TABLE chat_session DROP COLUMN IF EXISTS project_id;
