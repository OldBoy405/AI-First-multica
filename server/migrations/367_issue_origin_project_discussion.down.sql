-- Reverting the CHECK constraint to a value list that excludes
-- 'project_discussion' would fail validation (ADD CONSTRAINT re-checks all
-- existing rows) if any Discussion container issues still exist. Delete them
-- first — comment.issue_id is ON DELETE CASCADE (001_init.up.sql), so their
-- comment stream goes with them; there is nothing else keyed off this
-- container (it is never subscribed, never assigned, never referenced by
-- agent_task_queue).
DELETE FROM issue WHERE origin_type = 'project_discussion';

DROP INDEX IF EXISTS issue_project_discussion_unique;

ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'project_chat'));
