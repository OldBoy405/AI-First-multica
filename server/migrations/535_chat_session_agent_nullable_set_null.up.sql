-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.1, FR-7/FR-21): the project-shared
-- Discussion session may have no Coordinator bound, so chat_session.agent_id
-- becomes nullable, and the legacy inline FK is converted from ON DELETE
-- CASCADE to ON DELETE SET NULL so a hard-deleted Coordinator Agent keeps the
-- session/message rows (AC-31/AC-32). This is the single PRD-authorized
-- conversion of an EXISTING foreign key (PRD FR-21), not a new FK.
ALTER TABLE chat_session ALTER COLUMN agent_id DROP NOT NULL;
ALTER TABLE chat_session DROP CONSTRAINT chat_session_agent_id_fkey;
ALTER TABLE chat_session ADD CONSTRAINT chat_session_agent_id_fkey
    FOREIGN KEY (agent_id) REFERENCES agent(id) ON DELETE SET NULL;
