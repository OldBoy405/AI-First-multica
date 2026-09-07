-- AIFIRST: CR-2026-049 TASK-04: drift_finding table (P3 org intelligence E5 drift detection).
-- Table file holds no inline PK/FK/index: the id unique index (386) + PRIMARY KEY USING INDEX
-- (387), the dedup unique index (388) and the keyset index (389) each own a single-statement
-- CONCURRENTLY migration (repo convention). No FK on workspace_id by design (SDD §2.1):
-- findings are append-only governance projections, not tenant-owned resources.
CREATE TABLE drift_finding (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    repository_id TEXT NOT NULL,
    spec_id TEXT,
    cr_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('alignment-drift','impact-stale','bypass-commit','wip-on-trunk')),
    severity TEXT NOT NULL CHECK (severity IN ('info','warn','block')),
    summary TEXT NOT NULL,
    evidence JSONB NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','acknowledged','resolved','wontfix')),
    found_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    -- E5 scan findings (bypass-commit/wip-on-trunk) must carry complete commit evidence;
    -- spec-level findings (alignment-drift/impact-stale) are exempt. Last line of defense —
    -- the application layer validates the same shape before insert (SDD §2.1).
    CHECK (
        kind NOT IN ('bypass-commit','wip-on-trunk') OR
        (COALESCE(evidence->>'repository_id','') <> '' AND
         COALESCE(evidence->>'trunk','') <> '' AND
         COALESCE(evidence->>'commit_sha','') <> '' AND
         COALESCE(evidence->>'commit_subject','') <> '' AND
         COALESCE(evidence->>'scanned_at','') <> '')
    )
);
