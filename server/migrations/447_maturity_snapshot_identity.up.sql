-- AIFIRST: maturity_snapshot identity index (CR-2026-047 TASK-02, SDD §2.1).
-- Physical key includes workspace_id so org rows cannot collide across
-- tenants. CONCURRENTLY because CREATE INDEX takes ACCESS EXCLUSIVE.
CREATE UNIQUE INDEX CONCURRENTLY maturity_snapshot_identity_uidx
    ON maturity_snapshot (workspace_id, bucket_date, scope, scope_id);
