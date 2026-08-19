-- CR-2026-009 (P2 三模式聊天 CR-D): introduce a second hidden per-project
-- "container" issue, this time anchoring the Discussion tab's pure-human
-- message stream. Same shape as 160's 'project_chat' container: origin_id
-- stays NULL, and the container is never created by an agent_task_queue row.
--
-- Two changes:
--  1. Extend the origin_type CHECK to allow 'project_discussion' (same
--     DROP+ADD pattern as 060/111/131/149/160).
--  2. A partial unique index guarantees at most one Discussion container per
--     project, so concurrent lazy-creation (two browser tabs opening the
--     Discussion tab at once) collapses to a single row via
--     INSERT ... ON CONFLICT DO NOTHING + reselect.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'project_chat', 'project_discussion'));

CREATE UNIQUE INDEX IF NOT EXISTS issue_project_discussion_unique
    ON issue (project_id)
    WHERE origin_type = 'project_discussion';
