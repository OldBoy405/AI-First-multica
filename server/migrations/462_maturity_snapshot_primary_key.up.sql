-- AIFIRST: promote the concurrent unique index to the snapshot primary key
-- (CR-2026-047 TASK-02, SDD §2.1). Single statement: ALTER CONSTRAINT takes
-- ACCESS EXCLUSIVE on the new table only.
ALTER TABLE maturity_snapshot
    ADD CONSTRAINT maturity_snapshot_pkey
    PRIMARY KEY USING INDEX maturity_snapshot_identity_uidx;
