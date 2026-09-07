-- AIFIRST: CR-2026-049 TASK-04: finding keyset pagination index (SDD §2.3/§3.6):
-- (status_rank ASC, found_at DESC, id DESC) with status_rank derived in the query.
CREATE INDEX CONCURRENTLY drift_finding_keyset_idx
    ON drift_finding (workspace_id, status, found_at DESC, id DESC);
