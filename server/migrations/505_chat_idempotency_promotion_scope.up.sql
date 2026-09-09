-- AIFIRST: CR-2026-061 TASK-01 (SDD §2.2/§2.6/§13.2): extend the
-- chat_idempotency scope_type CHECK with 'discussion_promotion'. scope_id
-- semantics for the new scope = project_id (SDD §2.2).
--
-- One ALTER with DROP + ADD is atomic in PostgreSQL, so there is no window
-- where the constraint is missing (SDD §13.2). The constraint name is the
-- PG default name for an inline column CHECK ({table}_{column}_check);
-- migration 501 created it inline, so this name is stable.
--
-- Rollback precondition: no 'discussion_promotion' rows (SDD §13.1). The
-- 24h sweeper removes them within one retention window.
ALTER TABLE chat_idempotency
    DROP CONSTRAINT chat_idempotency_scope_type_check,
    ADD CONSTRAINT chat_idempotency_scope_type_check
        CHECK (scope_type IN ('discussion_message', 'merge_forward_messages', 'discussion_promotion'));
