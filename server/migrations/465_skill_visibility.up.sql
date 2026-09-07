-- AIFIRST: CR-2026-048 TASK-01 (Skill Market): skill visibility / version / owner columns.
-- visibility has only two values on purpose: builtin skills have no rows in
-- this table (they are synthesized at claim time), so a 'builtin' enum value
-- would be dead code (P3 design doc R-14).
ALTER TABLE skill
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','org')),
    ADD COLUMN version TEXT NOT NULL DEFAULT '0.1.0',
    ADD COLUMN owner_actor TEXT;
