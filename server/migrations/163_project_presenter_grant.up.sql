-- CR-2026-010 (P2 三模式聊天 CR-E): presenter (single-writer control) for the
-- Team Agent chat. A grant row's status IS its history — rows are never
-- deleted, only transitioned to a terminal status, so the table doubles as
-- the audit trail (no separate history table / JSONB array, no precedent for
-- either in this schema).
--
-- status values:
--   pending     - a member requested presenter access, awaiting owner decision
--   active      - the current presenter (at most one per project, enforced below)
--   rejected    - an owner rejected the pending request
--   released    - the presenter released control themselves
--   revoked     - an owner revoked an active presenter
--   transferred - the presenter handed control to another member (this row's
--                 terminal state; a new 'active' row is inserted for the target)
CREATE TABLE project_presenter_grant (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    project_id   UUID NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    status       TEXT NOT NULL CHECK (status IN
        ('pending', 'active', 'rejected', 'released', 'revoked', 'transferred')),
    granted_by   UUID,
    resolved_by  UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ
);

-- At most one active presenter per project (the single-writer guarantee's
-- DB-level backstop, alongside the advisory lock taken before writes).
CREATE UNIQUE INDEX ppg_active_uniq
    ON project_presenter_grant (project_id) WHERE status = 'active';

-- At most one pending request per (project, user) — a member cannot stack
-- duplicate requests.
CREATE UNIQUE INDEX ppg_pending_uniq
    ON project_presenter_grant (project_id, user_id) WHERE status = 'pending';

CREATE INDEX ppg_project_idx
    ON project_presenter_grant (project_id, created_at DESC);
