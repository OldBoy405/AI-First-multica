-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.6, FR-24): idempotency record table
-- for shared Discussion sends and merge-forward. Deliberately NO inline PK /
-- constraint index: 488 builds the unique index CONCURRENTLY and 489 attaches
-- the PK with USING INDEX. No FOREIGN KEY / REFERENCES (invariant 6).
CREATE TABLE chat_idempotency (
    workspace_id UUID NOT NULL,
    user_id      UUID NOT NULL,
    scope_type   TEXT NOT NULL CHECK (scope_type IN ('discussion_message', 'merge_forward_messages')),
    scope_id     UUID NOT NULL,      -- discussion_message: session_id; merge_forward_messages: project_id
    key          TEXT NOT NULL,      -- Idempotency-Key verbatim (<=255B, entry-length-capped)
    fingerprint  TEXT NOT NULL,
    response_status INT NOT NULL,
    response_body   JSONB,           -- placeholder until the owning transaction commits
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
