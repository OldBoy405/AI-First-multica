-- AIFIRST: CR-2026-056 TASK-02 (SDD §2.1/§2.6): one container issue may back
-- at most one session (AC-18); rows without a bound issue are excluded.
CREATE UNIQUE INDEX CONCURRENTLY project_chat_session_issue_uidx
    ON project_chat_session (issue_id)
    WHERE issue_id IS NOT NULL;
