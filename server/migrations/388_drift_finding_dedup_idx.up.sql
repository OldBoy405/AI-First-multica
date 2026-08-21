-- AIFIRST: CR-2026-049 TASK-04: finding dedup key (SDD §2.3/§4.1).
-- Same workspace/repository/kind/commit is one row across rescans; E5 rows carry
-- spec_id/cr_id = NULL (COALESCE'd) and a non-empty evidence commit_sha (DB CHECK).
CREATE UNIQUE INDEX CONCURRENTLY drift_finding_dedup_idx
    ON drift_finding (workspace_id, repository_id, kind, COALESCE(spec_id,''), (evidence->>'commit_sha'));
