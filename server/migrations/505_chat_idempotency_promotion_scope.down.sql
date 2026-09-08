-- AIFIRST: CR-2026-061 TASK-01 rollback (SDD §2.6/§13.1): restore the
-- pre-promotion scope_type enum. DATA-DEPENDENT: fails (and rolls back
-- safely, never silently dropping data) if any 'discussion_promotion' rows
-- remain — operators must wait out the 24h retention window (or clean up
-- with explicit approval) before rolling back this far.
ALTER TABLE chat_idempotency
    DROP CONSTRAINT chat_idempotency_scope_type_check,
    ADD CONSTRAINT chat_idempotency_scope_type_check
        CHECK (scope_type IN ('discussion_message', 'merge_forward_messages'));
