-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.2). Plain ALTER, no hook
-- registration.
-- DATA-DEPENDENT (lossy) rollback: PostgreSQL performs this unconditionally.
-- If any 'project_shared' rows still exist, their kind distinction is erased
-- and those rows fall back to private semantics under the old code. Operators
-- MUST archive/clean shared sessions BEFORE this rollback; the loss is
-- recorded here and must not be swallowed silently.
ALTER TABLE chat_session DROP COLUMN kind;
