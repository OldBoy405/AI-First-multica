-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.4). CONCURRENTLY DROP (not a
-- build) -> no concurrentDownIndexCleanups registration.
-- DATA-DEPENDENT rollback: the one-active-shared-session guarantee disappears.
-- The server-side kind routing code MUST be rolled back BEFORE this down, or
-- concurrent first-opens can create duplicate active shared rows.
DROP INDEX CONCURRENTLY IF EXISTS chat_session_project_shared_active_unique;
