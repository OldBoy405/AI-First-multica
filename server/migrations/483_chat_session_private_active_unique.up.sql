-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.3, FR-6): build the kind-narrowed
-- Private Ask unique index under a NEW name while the old wide-predicate
-- index (chat_session_project_creator_active_unique, dropped by 484) is still
-- enforcing. No unconstrained window exists at any point (see 484).
CREATE UNIQUE INDEX CONCURRENTLY chat_session_private_creator_active_unique
    ON chat_session (project_id, creator_id)
    WHERE project_id IS NOT NULL AND status = 'active' AND kind = 'private';
