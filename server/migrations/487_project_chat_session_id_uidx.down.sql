-- AIFIRST: CR-2026-056 TASK-02 rollback (SDD §2.6). After 474's down drops
-- the PK constraint, the constraint-owned index is gone too; IF EXISTS keeps
-- this a no-op in that order.
DROP INDEX CONCURRENTLY IF EXISTS project_chat_session_id_uidx;
