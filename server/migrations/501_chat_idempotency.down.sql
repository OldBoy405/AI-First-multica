-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.6). Lossy by design: dropping
-- the table drops the idempotency history (replay protection for in-flight
-- 24h windows); accepted as part of the rollback.
DROP TABLE chat_idempotency;
