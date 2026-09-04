-- AIFIRST: CR-2026-059 TASK-01 rollback (SDD §2.1). Reverse 481 in order:
-- restore ON DELETE CASCADE, then re-impose NOT NULL.
-- DATA-DEPENDENT rollback: the final SET NOT NULL FAILS when NULL agent_id
-- rows exist (rows whose Coordinator Agent was hard-deleted after 481).
-- Operators MUST clear those NULL rows first; the failure is intentional and
-- must not be swallowed.
ALTER TABLE chat_session DROP CONSTRAINT chat_session_agent_id_fkey;
ALTER TABLE chat_session ADD CONSTRAINT chat_session_agent_id_fkey
    FOREIGN KEY (agent_id) REFERENCES agent(id) ON DELETE CASCADE;
ALTER TABLE chat_session ALTER COLUMN agent_id SET NOT NULL;
