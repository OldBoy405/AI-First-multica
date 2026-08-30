-- name: GetActiveProjectChatSession :one
SELECT * FROM project_chat_session
WHERE workspace_id = $1 AND project_id = $2 AND status = 'active';

-- name: GetProjectChatSessionByID :one
SELECT * FROM project_chat_session
WHERE id = $1 AND workspace_id = $2 AND project_id = $3;

-- name: LockProjectChatSessionByID :one
SELECT * FROM project_chat_session
WHERE id = $1 AND workspace_id = $2 AND project_id = $3
FOR UPDATE;

-- name: InsertProjectChatSession :one
-- New sessions snapshot the creating Team Agent's defaults into base_* at
-- INSERT time (SDD §2.2); issue_id starts NULL (container is bound lazily on
-- first send or explicit POST container) and status starts 'active'.
INSERT INTO project_chat_session (
    id, workspace_id, project_id, agent_id, issue_id,
    base_model, base_thinking_level,
    model_override, thinking_level_override,
    status, created_by, created_at, updated_at
)
VALUES ($1, $2, $3, $4, NULL, $5, $6, NULL, NULL, 'active', $7, now(), now())
RETURNING *;

-- name: PatchProjectChatSessionConfig :one
-- Writes concrete override values resolved by the caller from the three-state
-- PATCH payload (omitted = keep current, null = clear, value = set). The
-- caller holds the session row lock from LockProjectChatSessionByID.
UPDATE project_chat_session
SET model_override = $3,
    thinking_level_override = $4,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: BindProjectChatSessionIssue :one
-- CAS bind: only fills issue_id when still NULL (adoption or first container).
UPDATE project_chat_session
SET issue_id = $2,
    updated_at = now()
WHERE id = $1 AND issue_id IS NULL
RETURNING *;

-- name: CloseActiveProjectChatSession :one
-- Rebinding a project to a different Team Agent closes the active session.
UPDATE project_chat_session
SET status = 'closed', updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status = 'active'
RETURNING *;

-- name: CountProjectChatSessions :one
-- Includes closed history rows; the adoption predicate (SDD §2.1) requires
-- COUNT == 1.
SELECT COUNT(*) FROM project_chat_session
WHERE workspace_id = $1 AND project_id = $2;
