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
    (tu.created_at AT TIME ZONE 'Asia/Shanghai')::date AS bucket_date,
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
GROUP BY (tu.created_at AT TIME ZONE 'Asia/Shanghai')::date, LOWER(tu.provider), tu.model
ORDER BY bucket_date, LOWER(tu.provider), tu.model;
-- Section 2: CR/pipeline metrics and governance guardrails (TASK-05)

-- name: MaturityArchivedCRs :many
-- CRs that first entered "archived" inside [from_utc, to_utc), with their
-- canonical project mapping (shell issue -> issue.project_id, workspace
-- checked). A CR whose shell issue is missing/cross-workspace stays org-only
-- (project_id NULL). Same-day duplicate events collapse to the earliest.
SELECT DISTINCT ON (cr.cr_id)
    cr.cr_id,
    e.occurred_at AS archived_at,
    issue.project_id
FROM cr
JOIN cr_sync_event e ON e.cr_id = cr.cr_id
    AND e.event_kind = 'status'
    AND e.payload->>'to_status' = 'archived'
    AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
    AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
LEFT JOIN issue ON issue.id = cr.shell_issue_id AND issue.workspace_id = cr.workspace_id
WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
ORDER BY cr.cr_id, e.occurred_at ASC;

-- name: MaturityCRUsers :many
-- Canonical user set per archived CR from the two verifiable sources only:
-- member comments on the shell issue, and task initiators of tasks bound to
-- the CR (the agent must belong to the same workspace — a foreign workspace
-- queue row with a colliding cr_id must not leak in). cr.owners is
-- deliberately NOT a source here: its ids are crctl free-text, handled by
-- MaturityCROwnerResolution instead.
SELECT u.cr_id, u.user_id FROM (
    SELECT cr.cr_id AS cr_id, c.author_id AS user_id
    FROM cr
    JOIN cr_sync_event e ON e.cr_id = cr.cr_id
        AND e.event_kind = 'status'
        AND e.payload->>'to_status' = 'archived'
        AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
        AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
    JOIN comment c ON c.issue_id = cr.shell_issue_id AND c.author_type = 'member'
    JOIN member m ON m.workspace_id = cr.workspace_id AND m.user_id = c.author_id
    WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
    UNION
    SELECT cr.cr_id AS cr_id, q.initiator_user_id AS user_id
    FROM cr
    JOIN cr_sync_event e ON e.cr_id = cr.cr_id
        AND e.event_kind = 'status'
        AND e.payload->>'to_status' = 'archived'
        AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
        AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
    JOIN agent_task_queue q ON (q.cr_id = cr.cr_id OR q.issue_id = cr.shell_issue_id)
    JOIN agent a ON a.id = q.agent_id AND a.workspace_id = cr.workspace_id
    WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
      AND q.initiator_user_id IS NOT NULL
) u
ORDER BY u.cr_id, u.user_id;

-- name: MaturityCROwnerResolution :many
-- One row per archived CR: how many owner entries carry a non-empty free-text
-- id. Never matches names, never casts to uuid — the values come from crctl
-- --caller and are not verifiable user identities. TASK-06 propagates a
-- non-zero count as project_collab_scale unavailable (reason
-- cr_owner_identity_unresolved) for the affected org/project scope.
WITH archived AS (
    SELECT DISTINCT ON (cr.cr_id)
        cr.cr_id, cr.workspace_id, cr.shell_issue_id, cr.owners
    FROM cr
    JOIN cr_sync_event e ON e.cr_id = cr.cr_id
        AND e.event_kind = 'status'
        AND e.payload->>'to_status' = 'archived'
        AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
        AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
    WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
    ORDER BY cr.cr_id, e.occurred_at ASC
)
SELECT
    a.cr_id,
    issue.project_id,
    count(*) FILTER (WHERE btrim(o.value->>'id') <> '')::bigint AS unresolved_owner_count
FROM archived a
LEFT JOIN issue ON issue.id = a.shell_issue_id AND issue.workspace_id = a.workspace_id
CROSS JOIN LATERAL jsonb_each(COALESCE(a.owners, '{}'::jsonb)) o
GROUP BY a.cr_id, issue.project_id;

-- name: MaturityActiveProjectKeys14d :many
-- Distinct business projects with task activity or CR status events in
-- [from_utc, to_utc). Org Admin system projects are excluded by the
-- business-projects filter in the caller (this query returns raw keys).
SELECT DISTINCT COALESCE(q.project_id, issue.project_id) AS project_id
FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
LEFT JOIN issue ON issue.id = q.issue_id
WHERE q.created_at >= sqlc.arg('from_utc')::timestamptz
  AND q.created_at <  sqlc.arg('to_utc')::timestamptz
  AND COALESCE(q.project_id, issue.project_id) IS NOT NULL
UNION
SELECT DISTINCT issue.project_id
FROM cr
JOIN cr_sync_event e ON e.cr_id = cr.cr_id
    AND e.event_kind = 'status'
    AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
    AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
LEFT JOIN issue ON issue.id = cr.shell_issue_id AND issue.workspace_id = cr.workspace_id
WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
  AND issue.project_id IS NOT NULL;

-- name: MaturityPrototypeGates :many
-- Review-gate node rows for CRs archived in the window. review_node_ids come
-- from governance.ReviewGateNodes[requirement|tech-design|code] — the SQL
-- never hardcodes UUID constants. Multiple runs may yield multiple attempts
-- per gate; the caller applies the once-through (attempt=1, passed) rule.
WITH archived AS (
    SELECT DISTINCT ON (cr.cr_id)
        cr.cr_id, cr.workspace_id
    FROM cr
    JOIN cr_sync_event e ON e.cr_id = cr.cr_id
        AND e.event_kind = 'status'
        AND e.payload->>'to_status' = 'archived'
        AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
        AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
    WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
    ORDER BY cr.cr_id, e.occurred_at ASC
)
SELECT a.cr_id, pnr.node_id, pnr.attempt, pnr.status, pnr.completed_at
FROM archived a
JOIN pipeline_run pr ON pr.workspace_id = a.workspace_id AND pr.cr_id = a.cr_id
JOIN pipeline_node_run pnr ON pnr.run_id = pr.id
    AND pnr.node_id = ANY(sqlc.arg('review_node_ids')::uuid[]);

-- name: MaturityPipelineCompletions :many
-- Completed pipeline runs for CRs archived in the window; the caller checks
-- the four-pipeline set (requirement-authoring/architecture-design/
-- code-implementation/feature-writeback) for the process-completion metric.
WITH archived AS (
    SELECT DISTINCT ON (cr.cr_id)
        cr.cr_id, cr.workspace_id
    FROM cr
    JOIN cr_sync_event e ON e.cr_id = cr.cr_id
        AND e.event_kind = 'status'
        AND e.payload->>'to_status' = 'archived'
        AND e.occurred_at >= sqlc.arg('from_utc')::timestamptz
        AND e.occurred_at <  sqlc.arg('to_utc')::timestamptz
    WHERE cr.workspace_id = sqlc.arg('workspace_id')::uuid
    ORDER BY cr.cr_id, e.occurred_at ASC
)
SELECT a.cr_id, pr.pipeline_id
FROM archived a
JOIN pipeline_run pr ON pr.workspace_id = a.workspace_id AND pr.cr_id = a.cr_id
WHERE pr.status = 'completed';

-- name: MaturityGateFirstPass :one
-- Governance: completed review gates in the window vs those passed on the
-- first attempt. review_node_ids scope the count to the three review gates.
SELECT
    count(*)::bigint AS completed,
    count(*) FILTER (WHERE pnr.attempt = 1 AND pnr.status = 'passed')::bigint AS first_pass
FROM pipeline_node_run pnr
JOIN pipeline_run pr ON pr.id = pnr.run_id
    AND pr.workspace_id = sqlc.arg('workspace_id')::uuid
WHERE pnr.node_id = ANY(sqlc.arg('review_node_ids')::uuid[])
  AND pnr.completed_at >= sqlc.arg('from_utc')::timestamptz
  AND pnr.completed_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityEvidenceDriftCount :one
SELECT count(*)::bigint AS n
FROM activity_log
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND action = 'aifirst.evidence_drift'
  AND created_at >= sqlc.arg('from_utc')::timestamptz
  AND created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityForbiddenAttemptCount :one
SELECT count(*)::bigint AS n
FROM activity_log
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND action = 'aifirst.gitguard_denied'
  AND created_at >= sqlc.arg('from_utc')::timestamptz
  AND created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityApprovalLatencies :many
-- One positive latency sample per approval decision: the latest completed
-- review node of that stage before approval_record.created_at. Only approve
-- decisions count (SDD §4.3).
SELECT DISTINCT ON (ar.id)
    ar.stage,
    (extract(epoch FROM (ar.created_at - pnr.completed_at)) * 1000)::bigint AS latency_ms
FROM approval_record ar
JOIN pipeline_run pr ON pr.workspace_id = ar.workspace_id AND pr.cr_id = ar.cr_id
JOIN pipeline_node_run pnr ON pnr.run_id = pr.id
    AND pnr.node_id = ANY(sqlc.arg('review_node_ids')::uuid[])
    AND pnr.completed_at IS NOT NULL
    AND pnr.completed_at < ar.created_at
WHERE ar.workspace_id = sqlc.arg('workspace_id')::uuid
  AND ar.decision = 'approve'
  AND ar.created_at >= sqlc.arg('from_utc')::timestamptz
  AND ar.created_at <  sqlc.arg('to_utc')::timestamptz
ORDER BY ar.id, pnr.completed_at DESC;

-- name: MaturityBaselinePercentiles :many
-- Week-4 baseline suggestions (SDD §4.4): over the FIRST consecutive 28 org
-- buckets (must be exactly 28 distinct days spanning 27), per metric keep only
-- ready non-null raw values; a metric needs >= 21 samples. Percentiles are
-- PostgreSQL percentile_cont — the caller never recomputes them in Go.
WITH first28 AS (
    SELECT bucket_date, metrics
    FROM maturity_snapshot
    WHERE workspace_id = $1 AND scope = 'org' AND scope_id = '·'
    ORDER BY bucket_date ASC
    LIMIT 28
),
checked AS (
    SELECT
        count(DISTINCT bucket_date) AS n_days,
        max(bucket_date) - min(bucket_date) AS span_days
    FROM first28
),
kv AS (
    SELECT j.key::text AS metric_key, (j.value->>'value')::float8 AS value
    FROM first28, LATERAL jsonb_each(metrics->'metric_values') AS j(key, value)
    WHERE j.value->>'data_status' = 'ready'
      AND j.value->>'value' IS NOT NULL
)
SELECT
    metric_key,
    count(*)::bigint AS sample_count,
    percentile_cont(0.10) WITHIN GROUP (ORDER BY value) AS p10,
    percentile_cont(0.75) WITHIN GROUP (ORDER BY value) AS p75
FROM kv, checked
WHERE checked.n_days = 28 AND checked.span_days = 27
GROUP BY metric_key
HAVING count(*) >= 21;

-- Section 3: rollup write path (TASK-06)

-- name: MaturityWorkspaces :many
SELECT id FROM workspace ORDER BY id;

-- name: MaturitySnapshotMaxBucket :one
SELECT max(bucket_date)::date AS max_bucket
FROM maturity_snapshot
WHERE workspace_id = $1;

-- name: MaturitySnapshotFirstBucket :one
SELECT min(bucket_date)::date AS first_bucket
FROM maturity_snapshot
WHERE workspace_id = $1 AND scope = 'org' AND scope_id = '·';

-- name: MaturitySnapshotInsert :execrows
INSERT INTO maturity_snapshot
    (workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, bucket_date, scope, scope_id) DO NOTHING;

-- name: MaturityTaskDepthRows :many
-- Task-level deep/total samples for project and user scopes (SDD §4.2 metric 7):
-- deep = the task carries a CR or issue binding. Same tenant join and
-- created_at window as MaturityTeamAgentCounts so org and per-scope numbers
-- share one definition.
SELECT
    q.initiator_user_id,
    COALESCE(q.project_id, issue.project_id) AS project_id,
    (q.cr_id IS NOT NULL OR q.issue_id IS NOT NULL)::boolean AS deep
FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id AND a.workspace_id = sqlc.arg('workspace_id')::uuid
LEFT JOIN issue ON issue.id = q.issue_id
WHERE q.created_at >= sqlc.arg('from_utc')::timestamptz
  AND q.created_at <  sqlc.arg('to_utc')::timestamptz;

-- name: MaturityRetryablePlans :many
-- Retry-eligible FAILED maturity_snapshot plans inside the 7-day window,
-- oldest first. The hook merges these with fresh cron occurrences so an
-- older failed plan is never stranded behind a newer success.
SELECT plan_time
FROM sys_cron_executions
WHERE job_name = 'maturity_snapshot'
  AND scope_kind = 'global'
  AND scope_id = 'global'
  AND status = 'FAILED'
  AND attempt < max_attempts
  AND next_retry_at <= $1
  AND plan_time > $2
ORDER BY plan_time ASC
LIMIT 7;

-- Section 4: read path (TASK-08)

-- name: GetMaturitySnapshot :one
SELECT workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev, created_at
FROM maturity_snapshot
WHERE workspace_id = $1 AND bucket_date = $2 AND scope = $3 AND scope_id = $4;

-- name: ListMaturitySnapshots :many
SELECT workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev, created_at
FROM maturity_snapshot
WHERE workspace_id = $1 AND scope = $2 AND scope_id = $3
  AND bucket_date >= $4 AND bucket_date <= $5
ORDER BY bucket_date ASC
LIMIT $6;

-- name: LatestMaturitySnapshot :one
SELECT workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev, created_at
FROM maturity_snapshot
WHERE workspace_id = $1 AND scope = $2 AND scope_id = $3
ORDER BY bucket_date DESC
LIMIT 1;

-- name: MaturityReportInboxExistsLocked :one
-- Serialize same-week retries without adding a CR-A-only inbox constraint.
SELECT EXISTS (
    SELECT 1
    FROM inbox_item
    WHERE workspace_id = sqlc.arg('workspace_id')::uuid
      AND recipient_type = 'member'
      AND recipient_id = sqlc.arg('recipient_id')::uuid
      AND type = 'maturity_report_ready'
      AND details->>'report_key' = sqlc.arg('report_key')::text
) AS exists
FROM (
    SELECT pg_advisory_xact_lock(hashtextextended(
        'maturity-report-inbox:' || sqlc.arg('report_key')::text, 0
    ))
) AS locked;

-- name: MaturityOrgAdminProjectID :one
SELECT id
FROM project
WHERE workspace_id = $1 AND settings->>'system_key' = 'org-admin-workspace'
LIMIT 1;

-- name: MaturityReportHistory :many
-- report_key ends in the ISO week and is immutable after completion. Keyset
-- pagination therefore stays stable when a newer weekly report arrives.
SELECT id, completed_at, result
FROM (
  SELECT DISTINCT ON (result->>'report_key') id, completed_at, result
  FROM agent_task_queue
  WHERE project_id = sqlc.arg('project_id')::uuid
    AND status = 'completed'
    AND result->>'schema' = sqlc.arg('schema')::text
    AND (
      sqlc.arg('before_report_key')::text = ''
      OR result->>'report_key' < sqlc.arg('before_report_key')::text
    )
  ORDER BY result->>'report_key', completed_at DESC, id DESC
) reports
ORDER BY result->>'report_key' DESC
LIMIT sqlc.arg('page_limit');

-- name: MaturityReportLatest :one
SELECT id, completed_at, result
FROM agent_task_queue
WHERE project_id = $1
  AND status = 'completed'
  AND result->>'schema' = 'ai-first.maturity-report/v1'
ORDER BY result->>'report_key' DESC, id DESC
LIMIT 1;

-- name: ListMaturitySnapshotsByScope :many
SELECT workspace_id, bucket_date, scope, scope_id, metrics, scores, config_rev, created_at
FROM maturity_snapshot
WHERE workspace_id = $1 AND scope = $2
  AND bucket_date >= $3 AND bucket_date <= $4
ORDER BY bucket_date ASC, scope_id ASC
LIMIT $5;

-- Section 5: Org Admin + weekly report (TASK-10)

-- name: MaturityOrgAdminAutopilot :one
SELECT *
FROM autopilot
WHERE workspace_id = $1 AND project_id = $2 AND title = 'AI Maturity Weekly Report'
LIMIT 1;

-- name: MaturityOrgAdminScheduleTrigger :one
SELECT *
FROM autopilot_trigger
WHERE autopilot_id = $1 AND kind = 'schedule'
LIMIT 1;
