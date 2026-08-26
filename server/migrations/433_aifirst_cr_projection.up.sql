-- AIFIRST: CR governance projection tables (CR-2026-002 TASK-04; P0 data-model mapping + SDD §2.1).
--
-- Authority rule: the source of truth for CRs is ALWAYS the knowledge-base git repo
-- (_backlog.yml / cr.md / approval.yml). These tables are projections consumed by the
-- board and approval UI — they can be truncated and replayed with zero data loss.
-- Events flow from crctl outbox via daemon (POST /api/daemon/cr-events) and are
-- consumed by internal/governance/crsync.go.
--
-- This migration is AI-First fork custom code (tracked in CUSTOM.md); keep the number
-- sequence intact when rebasing onto upstream.

-- CR projection row: the platform-side mirror of an in-flight/archived CR
CREATE TABLE cr (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    cr_id TEXT NOT NULL,                              -- "CR-2026-002"
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,                             -- one of the 15 named states (validated by governance.IsLegalTransition before write)
    owners JSONB NOT NULL DEFAULT '{}',
    target_version TEXT NOT NULL DEFAULT '',
    projected_commit TEXT NOT NULL DEFAULT '',        -- knowledge-base commit SHA the projection has caught up to
    needs_reconcile BOOLEAN NOT NULL DEFAULT FALSE,   -- set on out-of-order/missing events; healed by the reconcile job
    shell_issue_id UUID REFERENCES issue(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, cr_id)
);

CREATE INDEX idx_cr_workspace_status ON cr(workspace_id, status);
CREATE INDEX idx_cr_needs_reconcile ON cr(needs_reconcile) WHERE needs_reconcile;

-- Sync event ledger: idempotent dedup layer for at-least-once reporting
CREATE TABLE cr_sync_event (
    id BIGSERIAL PRIMARY KEY,
    cr_id TEXT NOT NULL,
    commit_sha TEXT NOT NULL DEFAULT '',              -- empty until the --embedded completion event arrives
    event_kind TEXT NOT NULL,                         -- status|owners|checkpoint|merge|archive|inbox
    payload JSONB NOT NULL,
    evidence JSONB NOT NULL DEFAULT '{}',             -- {path: "sha256:<hex>"} snapshot attached by crctl when entering an approval-pending status; grant issuance reads the latest one
    actor TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    UNIQUE (cr_id, commit_sha, event_kind)            -- idempotency key: same event via both channels (outbox + commit scan) lands once
);

CREATE INDEX idx_cr_sync_event_unprocessed ON cr_sync_event(cr_id, received_at) WHERE processed_at IS NULL;

-- Approval records: persistence side of server-signed (grant) approvals
CREATE TABLE approval_record (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    cr_id TEXT NOT NULL,
    stage TEXT NOT NULL,                              -- one of the four gates.json#approvalStages keys
    decision TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    approver_user_id UUID NOT NULL REFERENCES "user"(id),
    evidence_digest TEXT NOT NULL,                    -- canonical digest (same formula as crctl; locked by shared test vectors)
    key_id TEXT NOT NULL,
    signature TEXT NOT NULL,                          -- base64(ed25519)
    reject_reason TEXT NOT NULL DEFAULT '',
    grant_json JSONB NOT NULL DEFAULT '{}',           -- the exact grant file signed at issuance (delivered verbatim to the daemon; avoids reconstruction drift)
    delivered_at TIMESTAMPTZ,                         -- set when the daemon acks pickup
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Approve is idempotent, reject keeps every attempt: a reject followed by an approve
-- of the SAME evidence version must not hit a unique violation (SDD-SUG-001).
CREATE UNIQUE INDEX approval_record_approve_uniq
    ON approval_record (cr_id, stage, evidence_digest) WHERE decision = 'approve';

CREATE INDEX idx_approval_record_cr ON approval_record(cr_id, stage);
