-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.3, FR-6): drop the pre-kind wide
-- predicate index AFTER 483 built its narrowed replacement. Between 483 and
-- this drop both indexes enforce (no unconstrained window); the old index is
-- strictly stronger for private rows, so no extra insert failures occur.
DROP INDEX CONCURRENTLY IF EXISTS chat_session_project_creator_active_unique;
