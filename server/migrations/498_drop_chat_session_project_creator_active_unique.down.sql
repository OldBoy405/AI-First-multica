-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.3). CONCURRENTLY BUILD of
-- the old wide-predicate index -> the ONLY down direction registered in
-- concurrentDownIndexCleanups.
-- DATA-DEPENDENT rollback: if active 'project_shared' rows coexist with an
-- active Private Ask row on the same (project, creator), this build FAILS
-- with a unique violation (intentional, never swallowed). Operators must
-- archive/clean shared sessions before rolling back this far.
CREATE UNIQUE INDEX CONCURRENTLY chat_session_project_creator_active_unique
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL AND status = 'active';
