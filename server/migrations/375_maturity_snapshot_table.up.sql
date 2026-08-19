-- AIFIRST: maturity snapshot projection table (CR-2026-047 TASK-02, SDD §2.1).
-- Rebuildable projection of the AI maturity dashboard; every row is one
-- (workspace, local bucket date, scope) fact frozen at rollup time.
-- Deliberately carries NO inline indexes: 376 builds the unique index
-- CONCURRENTLY and 377 promotes it to the primary key (CLAUDE.md migration
-- rules forbid inline indexes on hot paths and mixing CONCURRENTLY with other
-- statements in one file).
CREATE TABLE maturity_snapshot (
    workspace_id UUID        NOT NULL,
    bucket_date  DATE        NOT NULL,
    scope        TEXT        NOT NULL CHECK (scope IN ('org','user','project')),
    scope_id     TEXT        NOT NULL,
    metrics      JSONB       NOT NULL DEFAULT '{}',
    scores       JSONB       NOT NULL DEFAULT '{}',
    config_rev   TEXT        NOT NULL CHECK (config_rev ~ '^[0-9a-f]{40}$'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
      (scope = 'org' AND scope_id = '·') OR
      (scope IN ('user','project') AND scope_id <> '·')
    )
);
