-- name: ListProjects :many
SELECT * FROM project
WHERE workspace_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::text IS NULL OR priority = sqlc.narg('priority'))
ORDER BY created_at DESC;

-- name: GetProject :one
SELECT * FROM project
WHERE id = $1;

-- name: GetProjectInWorkspace :one
SELECT * FROM project
WHERE id = $1 AND workspace_id = $2;

-- name: CreateProject :one
INSERT INTO project (
    workspace_id, title, description, icon, status,
    lead_type, lead_id, priority
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
) RETURNING *;

-- name: UpdateProject :one
UPDATE project SET
    title = COALESCE(sqlc.narg('title'), title),
    description = sqlc.narg('description'),
    icon = sqlc.narg('icon'),
    status = COALESCE(sqlc.narg('status'), status),
    priority = COALESCE(sqlc.narg('priority'), priority),
    lead_type = sqlc.narg('lead_type'),
    lead_id = sqlc.narg('lead_id'),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteProject :exec
-- Defense-in-depth: workspace_id is a SQL-layer tenant guard. See DeleteIssue.
DELETE FROM project WHERE id = $1 AND workspace_id = $2;

-- name: CountIssuesByProject :one
-- CR-2026-006/CR-2026-009: exclude the hidden Team Agent chat and Discussion
-- container issues from counts.
SELECT count(*) FROM issue
WHERE project_id = $1
  AND origin_type IS DISTINCT FROM 'project_chat'
  AND origin_type IS DISTINCT FROM 'project_discussion';

-- name: GetProjectIssueStats :many
-- CR-2026-006/CR-2026-009: exclude the hidden Team Agent chat and Discussion
-- container issues from stats.
SELECT project_id,
       count(*)::bigint AS total_count,
       count(*) FILTER (WHERE status IN ('done', 'cancelled'))::bigint AS done_count
FROM issue
WHERE project_id = ANY(sqlc.arg('project_ids')::uuid[])
  AND origin_type IS DISTINCT FROM 'project_chat'
  AND origin_type IS DISTINCT FROM 'project_discussion'
GROUP BY project_id;

-- name: GetProjectSettings :one
SELECT settings FROM project WHERE id = $1;

-- name: UpdateProjectSettings :one
-- Shallow-merge the given keys into the project settings bag. Values are
-- caller-validated; workspace_id is the SQL-layer tenant guard (see DeleteProject).
UPDATE project
SET settings = settings || @patch::jsonb, updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
RETURNING *;
