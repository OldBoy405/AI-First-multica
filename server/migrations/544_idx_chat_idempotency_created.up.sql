-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.6, FR-24): created_at index for the
-- 24h sweeper range delete. Registered in cmd/migrate concurrentIndexCleanups.
CREATE INDEX CONCURRENTLY idx_chat_idempotency_created ON chat_idempotency (created_at);
