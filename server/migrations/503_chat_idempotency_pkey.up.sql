-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.6, FR-24): attach the PK with USING
-- INDEX (488's unique, non-partial index satisfies all preconditions).
-- PostgreSQL renames the index to chat_idempotency_pkey; ON CONFLICT
-- arbitration targets the CONSTRAINT name so later index renames cannot
-- drift. ACCESS EXCLUSIVE on a table that is still empty (487 just created
-- it, code not deployed) is instantaneous.
ALTER TABLE chat_idempotency
    ADD CONSTRAINT chat_idempotency_pkey
    PRIMARY KEY USING INDEX chat_idempotency_scope_key_uidx;
