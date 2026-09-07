-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.6). Plain DROP; the
-- idempotency table is small, no CONCURRENTLY needed.
DROP INDEX IF EXISTS idx_chat_idempotency_created;
