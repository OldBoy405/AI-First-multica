-- maturity.sql — AI maturity dashboard queries (CR-2026-047).
-- Section 1 (TASK-04): token/agent metrics (metrics 1/2/7, headline, model detail).
-- Section 2 (TASK-05): CR/pipeline metrics and governance guardrails.
-- Every query must bound tenant access first: tables without workspace_id
-- (agent_task_queue, cr_sync_event, task_usage) join through a workspace-scoped
-- parent (agent, cr). All windows are half-open [from_utc, to_utc).

-- Section 1: token/agent metrics (TASK-04)

-- name: MaturityMemberCount :one
SELECT count(*)::bigint AS n FROM member WHERE workspace_id = $1;

-- name: MaturityTaskTokenRows :many
-- Per-usage-row token detail for one local day (metrics 1/2 attribution and
-- headline). Bucketed by tu.created_at, NOT task creation: a task created on
-- D1 that emits usage on D2 belongs to D2. project_key falls back
-- q.project_id -> issue.project_id; NULL rows count org-only.
SELECT
    tu.task_id,
    q.initiator_user_id,
    COALESCE(q.project_id, issue.project_id) AS project_key,
    LOWER(tu.provider) AS provider,
    tu.model,
    tu.input_tokens,
    tu.output_tokens,
    tu.cache_read_tokens,
    tu.cache_write_tokens,
    tu.cost_usd_ticks
FROM task_usage tu
JOIN agent_task_queue q ON q.id = tu.task_id
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
LEFT JOIN issue ON issue.id = q.issue_id
WHERE tu.created_at >= sqlc.arg('from_utc')::timestamptz
  AND tu.created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityBusinessProjects :many
SELECT id FROM project
WHERE workspace_id = $1
  AND status != 'cancelled'
  AND settings->>'system_key' IS NULL
ORDER BY id;

-- name: MaturityInitiatorDistinct :one
SELECT count(DISTINCT q.initiator_user_id)::bigint AS n
FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
WHERE q.created_at >= sqlc.arg('from_utc')::timestamptz
  AND q.created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityAttributionCounts :one
SELECT
    count(*) FILTER (WHERE q.initiator_user_id IS NOT NULL)::bigint AS attributed,
    count(*) FILTER (WHERE q.initiator_user_id IS NULL)::bigint     AS unattributed
FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
WHERE q.created_at >= sqlc.arg('from_utc')::timestamptz
  AND q.created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityTeamAgentCounts :one
-- "Deep" usage = task carries a CR or issue binding (SDD §4.2 metric 7).
-- pipeline_node_run_id deliberately does not count.
SELECT
    count(*) FILTER (WHERE q.cr_id IS NOT NULL OR q.issue_id IS NOT NULL)::bigint AS deep,
    count(*)::bigint AS total
FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
WHERE q.created_at >= sqlc.arg('from_utc')::timestamptz
  AND q.created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityModelCostRows :many
-- Per-provider/model token totals for the model-detail trend (SDD §3.3).
-- Uncosted* tokens are the NULL-cost subset only; callers price those from the
-- generated price map and never re-price authoritative ticks (no double count).
SELECT
    LOWER(tu.provider) AS provider,
    tu.model,
    SUM(tu.input_tokens)::bigint       AS input_tokens,
    SUM(tu.output_tokens)::bigint      AS output_tokens,
    SUM(tu.cache_read_tokens)::bigint  AS cache_read_tokens,
    SUM(tu.cache_write_tokens)::bigint AS cache_write_tokens,
    COALESCE(SUM(tu.cost_usd_ticks), 0)::bigint AS cost_usd_ticks,
    COALESCE(SUM(tu.input_tokens)       FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_input_tokens,
    COALESCE(SUM(tu.output_tokens)      FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_output_tokens,
    COALESCE(SUM(tu.cache_read_tokens)  FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_cache_read_tokens,
    COALESCE(SUM(tu.cache_write_tokens) FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_cache_write_tokens,
    count(*)::bigint AS usage_rows,
    count(tu.cost_usd_ticks)::bigint AS authoritative_rows
FROM task_usage tu
JOIN agent_task_queue q ON q.id = tu.task_id
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
WHERE tu.created_at >= sqlc.arg('from_utc')::timestamptz
  AND tu.created_at <  sqlc.arg('to_utc')::timestamptz
GROUP BY LOWER(tu.provider), tu.model
ORDER BY LOWER(tu.provider), tu.model;
