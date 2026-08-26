-- AIFIRST: CR-2026-049 TASK-04: attach the primary key using the concurrently built
-- unique index from 386 (no table rewrite).
ALTER TABLE drift_finding ADD CONSTRAINT drift_finding_pkey PRIMARY KEY USING INDEX drift_finding_id_uidx;
