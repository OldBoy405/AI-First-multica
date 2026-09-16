-- Roll back to the validated constraint state that preceded migration 267.
-- If project_chat/project_discussion rows remain, validation fails closed.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN (
        'autopilot',
        'quick_create',
        'lark_chat',
        'slack_chat',
        'agent_create',
        'dingtalk_chat',
        'wecom_chat'
    )) NOT VALID;
ALTER TABLE issue VALIDATE CONSTRAINT issue_origin_type_check;
