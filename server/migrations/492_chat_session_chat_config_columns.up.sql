-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.2/§2.6): Private Ask chat_session gains
-- four nullable chat-config columns. Historical rows stay NULL; compatibility
-- rules per SDD FR-11 (agent_default is resolved live and never written to
-- these columns). ADD COLUMN nullable => no table rewrite.
ALTER TABLE chat_session
    ADD COLUMN base_model TEXT,
    ADD COLUMN base_thinking_level TEXT,
    ADD COLUMN model_override TEXT,
    ADD COLUMN thinking_level_override TEXT;
