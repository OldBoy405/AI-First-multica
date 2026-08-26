-- AIFIRST: snapshot scope/date query index for trend reads
-- (CR-2026-047 TASK-02, SDD §2.1): GET /api/maturity/overall and token-trend
-- read (workspace, scope, scope_id) ordered by bucket_date DESC.
CREATE INDEX CONCURRENTLY maturity_snapshot_scope_date_idx
    ON maturity_snapshot (workspace_id, scope, scope_id, bucket_date DESC);
