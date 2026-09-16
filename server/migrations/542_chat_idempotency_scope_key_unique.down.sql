-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.6). CONCURRENTLY DROP (not a
-- build) -> no concurrentDownIndexCleanups registration. No-op when 489 has
-- already rolled back (it removed the index with the constraint); effective
-- only for the partial state "488 applied, 489 not applied".
DROP INDEX CONCURRENTLY IF EXISTS chat_idempotency_scope_key_uidx;
