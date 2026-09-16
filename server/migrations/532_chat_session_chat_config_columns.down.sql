-- AIFIRST: CR-2026-056 TASK-02 rollback (SDD §2.6).
ALTER TABLE chat_session
    DROP COLUMN thinking_level_override,
    DROP COLUMN model_override,
    DROP COLUMN base_thinking_level,
    DROP COLUMN base_model;
