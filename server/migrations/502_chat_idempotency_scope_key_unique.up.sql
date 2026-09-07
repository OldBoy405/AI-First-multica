-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.6, FR-24): unique index carrying the
-- PK for chat_idempotency, built CONCURRENTLY (single statement, own file).
-- Registered in cmd/migrate concurrentIndexCleanups.
CREATE UNIQUE INDEX CONCURRENTLY chat_idempotency_scope_key_uidx
    ON chat_idempotency (workspace_id, user_id, scope_type, scope_id, key);
