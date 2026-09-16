-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.6): promote the concurrently-built
-- unique index from migration 473 into the table's primary key.
ALTER TABLE project_chat_session
    ADD CONSTRAINT project_chat_session_pkey PRIMARY KEY USING INDEX project_chat_session_id_uidx;
