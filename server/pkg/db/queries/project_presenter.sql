-- CR-2026-010 (P2 三模式聊天 CR-E): presenter (single-writer control) grant
-- queries. See migration 163 for the table shape and its two partial unique
-- indexes (single active grant, single pending request per user).

-- name: CreatePresenterRequest :one
INSERT INTO project_presenter_grant (workspace_id, project_id, user_id, status)
VALUES ($1, $2, $3, 'pending')
RETURNING *;

-- name: GetActivePresenterGrant :one
SELECT * FROM project_presenter_grant
WHERE project_id = $1 AND status = 'active';

-- name: GetPendingPresenterGrantByUser :one
SELECT * FROM project_presenter_grant
WHERE project_id = $1 AND user_id = $2 AND status = 'pending';

-- name: ListPendingPresenterGrants :many
-- Full pending list, for owner/admin callers only (TSUG-003) — see
-- GetPendingPresenterGrantByUser for the "my own request" projection every
-- caller regardless of role is allowed to see.
SELECT * FROM project_presenter_grant
WHERE project_id = $1 AND status = 'pending'
ORDER BY created_at ASC;

-- name: ApprovePresenterGrant :one
-- Flips the pending row itself to active rather than inserting a new row —
-- created_at stays the original request time, granted_by records the
-- approver. No separate "approved" state (SDD §4.2).
UPDATE project_presenter_grant
SET status = 'active', granted_by = $3
WHERE project_id = $1 AND user_id = $2 AND status = 'pending'
RETURNING *;

-- name: RejectPresenterGrant :one
UPDATE project_presenter_grant
SET status = 'rejected', resolved_by = $3, resolved_at = now()
WHERE project_id = $1 AND user_id = $2 AND status = 'pending'
RETURNING *;

-- name: CloseActivePresenterGrant :one
-- Shared terminal-close for revoke/release/transfer's outgoing half — @status
-- is one of 'revoked' | 'released' | 'transferred' (caller passes the Go-side
-- constant, never a raw literal).
UPDATE project_presenter_grant
SET status = @status, resolved_by = @resolved_by, resolved_at = now()
WHERE project_id = @project_id AND status = 'active'
RETURNING *;

-- name: CreateActivePresenterGrant :one
-- Transfer's incoming half: a fresh active row for the new presenter.
INSERT INTO project_presenter_grant (workspace_id, project_id, user_id, status, granted_by)
VALUES ($1, $2, $3, 'active', $4)
RETURNING *;

-- name: RevokeActivePresenterGrantsForUser :many
-- Member-removal linkage (workspace.go revokeAndRemoveMember): close every
-- active presenter grant this user held across the whole workspace, so a
-- departed member never remains the recorded presenter of a project they can
-- no longer access.
UPDATE project_presenter_grant
SET status = 'revoked', resolved_at = now()
WHERE workspace_id = $1 AND user_id = $2 AND status = 'active'
RETURNING *;

-- name: RejectPendingPresenterGrantsForUser :many
-- Member-removal linkage counterpart to RevokeActivePresenterGrantsForUser:
-- close every pending request this user filed, across the whole workspace.
UPDATE project_presenter_grant
SET status = 'rejected', resolved_at = now()
WHERE workspace_id = $1 AND user_id = $2 AND status = 'pending'
RETURNING *;
