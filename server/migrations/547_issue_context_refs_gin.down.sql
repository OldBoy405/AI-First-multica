-- AIFIRST: CR-2026-061 TASK-01 rollback (SDD §13.1): no data dependency.
-- Downside semantics: the dedupe_key containment query falls back to a
-- sequential scan; correctness is unchanged.
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_context_refs_gin;
