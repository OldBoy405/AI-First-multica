-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.5). Plain ALTER, no hook
-- registration.
-- DATA-DEPENDENT (lossy) rollback: author attribution already written on
-- shared messages is irreversibly erased; rendering falls back to the NULL
-- degradation (role literal). Private/legacy rows are unaffected (their
-- columns are NULL anyway).
ALTER TABLE chat_message DROP COLUMN author_type, DROP COLUMN author_id;
