-- AIFIRST: CR-2026-056 TASK-02 (PRD FR-19, SDD §2.1): new per-project Team
-- Agent chat session table. One row = one session identity (PATCH/send
-- credential). At most one active row per (workspace, project) is enforced by
-- migration 475's partial unique index. No PK/UNIQUE/FK here (repo hard rule:
-- no REFERENCES, no cascades); the PK is built in 473/474 from a
-- concurrently-created unique index. workspace/project/agent membership is
-- validated at the application layer.
CREATE TABLE project_chat_session (
    id UUID,
    workspace_id UUID NOT NULL,
    project_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    issue_id UUID,
    base_model TEXT,
    base_thinking_level TEXT,
    model_override TEXT,
    thinking_level_override TEXT,
    status TEXT NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT project_chat_session_status_ck CHECK (status IN ('active', 'closed'))
);
