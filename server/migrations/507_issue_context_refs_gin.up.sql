-- AIFIRST: CR-2026-061 TASK-01 (SDD §2.5/§4.2/§7.2): GIN containment index
-- backing the dedupe_key lookup
-- (context_refs @> jsonb_build_array(jsonb_build_object('dedupe_key', ...))).
-- Same single-statement CONCURRENTLY pattern as 192 (issue properties GIN).
-- Registered in cmd/migrate concurrentIndexCleanups.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_context_refs_gin
    ON issue USING GIN (context_refs jsonb_path_ops);
