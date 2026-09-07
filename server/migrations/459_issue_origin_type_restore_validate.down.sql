-- Restore migration 267's NOT VALID widened constraint when rolling back only
-- migration 268. Migration 267 down then narrows and validates it.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN (
        'autopilot',
        'quick_create',
        'lark_chat',
        'slack_chat',
        'agent_create',
        'project_chat',
        'project_discussion',
        'dingtalk_chat',
        'wecom_chat'
    )) NOT VALID;
