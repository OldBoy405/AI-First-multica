-- AIFIRST: CR-2026-049 TASK-04: backing unique index for drift_finding's primary key,
-- attached in 387 via PRIMARY KEY USING INDEX. Own single-statement migration so
-- CONCURRENTLY runs outside an implicit transaction (repo convention).
CREATE UNIQUE INDEX CONCURRENTLY drift_finding_id_uidx
    ON drift_finding (id);
