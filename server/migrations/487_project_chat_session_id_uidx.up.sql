-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.6): unique index on session id;
-- converted into the table PK by migration 474.
CREATE UNIQUE INDEX CONCURRENTLY project_chat_session_id_uidx
    ON project_chat_session (id);
