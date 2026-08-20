-- Skill Market queries. AIFIRST: CR-2026-048 TASK-05.

-- name: InsertSkillUsageEvent :one
-- One row per skill ref per claim; best-effort telemetry, never gates the claim.
INSERT INTO skill_usage_event (workspace_id, skill_ref, task_id, project_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: MarketSkillUsage :many
-- Usage ranking: completed tasks only, deduplicated per (task_id, skill_ref)
-- so claim retries never inflate counts. Workspace-scoped (hard invariant 1).
SELECT e.skill_ref, COUNT(DISTINCT e.task_id) AS usage_count
FROM skill_usage_event e
JOIN agent_task_queue t ON t.id = e.task_id
WHERE e.workspace_id = $1 AND t.status = 'completed'
GROUP BY e.skill_ref;

-- name: ListOrgSkillSummariesByWorkspace :many
-- Org-visible skills for the market list. Omits the SKILL.md `content` column,
-- same payload-size rationale as ListSkillSummariesByWorkspace.
SELECT id, workspace_id, name, description, config, visibility, version, owner_actor, created_by, created_at, updated_at
FROM skill
WHERE workspace_id = $1 AND visibility = 'org'
ORDER BY name ASC;

-- name: InsertSkillAppealEvent :one
-- Appeal ledger rows live in activity_log (append-only audit semantics, no new table).
INSERT INTO activity_log (workspace_id, actor_type, actor_id, action, details)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAppealDecision :one
-- Served by the 384 partial index (details->>'appeal_id', appeal actions).
SELECT * FROM activity_log
WHERE workspace_id = $1 AND action = 'skill_appeal_approved' AND details->>'appeal_id' = sqlc.arg('appeal_id')::text
ORDER BY created_at DESC
LIMIT 1;

-- name: HasAppealSubmitted :one
-- Idempotency check for appeal submission; duplicates tolerated by audit semantics.
SELECT EXISTS(
    SELECT 1 FROM activity_log
    WHERE workspace_id = $1 AND action = 'skill_appeal_submitted' AND details->>'appeal_id' = sqlc.arg('appeal_id')::text
);
