-- AIFIRST: CR-2026-061 TASK-01 rollback (SDD §13.1): no data dependency.
-- Downside semantics: losing the uniqueness guard, the application-level
-- project advisory lock still converges concurrent promotions; the
-- recognition-key lookup falls back to a scan.
DROP INDEX CONCURRENTLY IF EXISTS idx_pipeline_run_promotion_active_issue;
