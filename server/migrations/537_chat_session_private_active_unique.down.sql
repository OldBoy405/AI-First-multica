-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.3). CONCURRENTLY DROP (not a
-- build) -> no concurrentDownIndexCleanups registration. Rollback order runs
-- 484.down (rebuild of the old wide index) BEFORE this file, so both indexes
-- coexist during the window (over-constrained but safe).
DROP INDEX CONCURRENTLY IF EXISTS chat_session_private_creator_active_unique;
