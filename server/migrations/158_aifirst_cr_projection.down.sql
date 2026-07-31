-- AIFIRST: roll back the CR governance projection tables (CR-2026-002 TASK-04).
-- Projections are safe to drop: git is the authority; replaying events rebuilds everything.
DROP TABLE IF EXISTS approval_record;
DROP TABLE IF EXISTS cr_sync_event;
DROP TABLE IF EXISTS cr;
