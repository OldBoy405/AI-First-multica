-- AIFIRST: pipeline runner state tables (CR-2026-011 TASK-01; P0 data-model mapping §3.4
-- + SDD §3). These are populated by the governance gate-node projector (crsync.go) from
-- the crctl event stream — there is no orchestrator/runner yet (that's CR-H, unregistered
-- at the time of this migration). Only human_approval and review nodes get projected rows
-- in this CR; full per-node coverage arrives with the Runner, which becomes a second
-- writer against this same schema.
--
-- Authority rule: same as `cr` (158) — git is the source of truth for review-loop attempt
-- counts (review-loop.yml); these rows are a read-side projection and can be replayed.
--
-- This migration is AI-First fork custom code (tracked in CUSTOM.md); keep the number
-- sequence intact when rebasing onto upstream.

CREATE TABLE pipeline_run (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id       UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    pipeline_id        TEXT NOT NULL,              -- e.g. 'requirement-authoring', one of 8 templates
    cr_id              TEXT,                        -- NULL for planning-only pipelines with no CR
    issue_id           UUID REFERENCES issue(id) ON DELETE SET NULL,
    status             TEXT NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'waiting_approval', 'completed', 'failed', 'cancelled')),
    inputs             JSONB NOT NULL DEFAULT '{}',
    execution_context  JSONB NOT NULL DEFAULT '{}', -- mirrors the node-N.md trailing YAML block (cr_id/branch/worktrees)
    started_by         UUID NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at       TIMESTAMPTZ
);

CREATE INDEX idx_pipeline_run_cr ON pipeline_run(cr_id) WHERE cr_id IS NOT NULL;
CREATE INDEX idx_pipeline_run_workspace_status ON pipeline_run(workspace_id, status);

CREATE TABLE pipeline_node_run (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES pipeline_run(id) ON DELETE CASCADE,
    node_id       UUID NOT NULL,              -- deterministic id, see governance.ResolveNodeID
    ref           TEXT,                        -- skill id; NULL for human_approval nodes
    kind          TEXT NOT NULL CHECK (kind IN ('skill', 'human_approval', 'code_generation')),
    seq           INT NOT NULL,                -- position in the pipeline template's nodes[] array
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'running', 'passed', 'blocked', 'skipped', 'failed')),
    attempt       INT NOT NULL DEFAULT 1,       -- reviewLoop round; authoritative copy lives in git review-loop.yml
    approval_id   UUID REFERENCES approval_record(id),
    output_note   TEXT,                         -- worktree-relative path to node-N.md
    detail        JSONB NOT NULL DEFAULT '{}',  -- review event payload: {verdict, blockers[], reviewer, reviewed_at}
                                                 -- (superset of P0 §3.4 — added here because P0's schema had nowhere
                                                 -- to put per-node blocker detail; see SDD §3 note)
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    UNIQUE (run_id, node_id, attempt)
);

CREATE INDEX idx_pipeline_node_run_run ON pipeline_node_run(run_id);
CREATE INDEX idx_pipeline_node_run_status ON pipeline_node_run(status) WHERE status IN ('running', 'blocked');

-- agent_task_queue attribution columns (P0 §2.2, "B4"). Both nullable and populated
-- out-of-band (post-hoc, from the StartTask daemon callback — see SDD DD-4): no enqueue
-- path has CR context at insert time, so ClaimAgentTask's serialization predicate and the
-- "all four FKs NULL" quick-create mutual-exclusion class are intentionally left untouched.
ALTER TABLE agent_task_queue
    ADD COLUMN cr_id TEXT,
    ADD COLUMN pipeline_node_run_id UUID REFERENCES pipeline_node_run(id) ON DELETE SET NULL;

CREATE INDEX idx_agent_task_queue_cr_id ON agent_task_queue(cr_id) WHERE cr_id IS NOT NULL;
CREATE INDEX idx_agent_task_queue_pipeline_node_run ON agent_task_queue(pipeline_node_run_id) WHERE pipeline_node_run_id IS NOT NULL;
