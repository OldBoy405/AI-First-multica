-- AIFIRST: CR-2026-061 TASK-01 (SDD §2.5/D-8): at most one non-terminal
-- requirement-authoring run per (workspace, issue) — the promotion
-- recognition key and uniqueness guard that covers BOTH phases of the
-- pre-built run: before bind (cr_id IS NULL) and after bind (cr_id = CR-ID,
-- same row). issue_id IS NULL projection rows (plain CR registration) are
-- unaffected: PostgreSQL unique indexes never conflict on NULLs.
--
-- Single-statement migration: CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction or share a migration (repo convention, see 080/192/456).
-- Registered in cmd/migrate concurrentIndexCleanups.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_pipeline_run_promotion_active_issue
    ON pipeline_run (workspace_id, issue_id)
    WHERE pipeline_id = 'requirement-authoring' AND status IN ('running', 'waiting_approval');
