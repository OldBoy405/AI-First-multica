-- AIFIRST: CR-2026-056 TASK-02 rollback (SDD §2.6). Dropping the PK
-- constraint also drops the index it owns.
ALTER TABLE project_chat_session
    DROP CONSTRAINT project_chat_session_pkey;
