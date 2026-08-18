-- CR-2026-045 TASK-15: restore the complete issue origin set lost when
-- migrations 259/263 rebuilt issue_origin_type_check without the project
-- container values. Widening is NOT VALID first so validation can run under a
-- weaker lock in migration 268.
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
