-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.6). Plain ALTER; the
-- underlying index (chat_idempotency_pkey after 489's rename) is removed with
-- the constraint. 488.down is therefore a no-op after this.
ALTER TABLE chat_idempotency DROP CONSTRAINT IF EXISTS chat_idempotency_pkey;
