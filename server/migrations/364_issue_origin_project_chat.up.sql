-- CR-2026-006 (P2 三模式聊天 CR-A): introduce a hidden per-project "container"
-- issue that anchors the Team Agent chat message stream. Each project's group
-- chat reuses the existing comment / timeline / websocket infrastructure by
-- hanging its messages off one issue stamped origin_type='project_chat'.
--
-- Two changes:
--  1. Extend the origin_type CHECK to allow 'project_chat' (same DROP+ADD
--     pattern as 060/111/131/149/259/263/259/263). origin_id stays NULL — unlike the other
--     origin types this container is not created *by* an agent_task_queue row.
--  2. A partial unique index guarantees at most one container per project, so
--     concurrent lazy-creation (two browser tabs opening the Chat tab at once)
--     collapses to a single row via INSERT ... ON CONFLICT DO NOTHING + reselect.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'project_chat'));

CREATE UNIQUE INDEX IF NOT EXISTS issue_project_chat_unique
    ON issue (project_id)
    WHERE origin_type = 'project_chat';
