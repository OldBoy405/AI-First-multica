-- AIFIRST: CR-2026-048 TASK-01 rollback.
ALTER TABLE skill
    DROP COLUMN visibility,
    DROP COLUMN version,
    DROP COLUMN owner_actor;
