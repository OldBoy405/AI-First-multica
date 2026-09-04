-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.6/§4.6, FR-24): idempotency record
-- CRUD for shared Discussion sends and merge-forward. scope_type ∈
-- ('discussion_message', 'merge_forward_messages'); scope_id is the session
-- id for messages and the project id for merge-forward. PK conflict
-- arbitration targets the CONSTRAINT name chat_idempotency_pkey (489) so
-- index renames cannot drift.

-- name: InsertChatIdempotencyReservation :one
-- Reservation inside the send transaction. On a unique conflict PostgreSQL
-- blocks until the concurrent winner commits/rolls back, then returns no
-- rows (pgx.ErrNoRows); the caller reads the winner via
-- GetChatIdempotencyByKey. response_body stays NULL (placeholder) until the
-- owning transaction finalizes.
INSERT INTO chat_idempotency (workspace_id, user_id, scope_type, scope_id, key, fingerprint)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT ON CONSTRAINT chat_idempotency_pkey DO NOTHING
RETURNING *;

-- name: GetChatIdempotencyByKey :one
-- Winner lookup after a reservation conflict (SDD §4.6): same fingerprint +
-- response_body set => replay the stored response; same fingerprint + NULL
-- body => this request takes over (previous execution interrupted);
-- different fingerprint => 409 idempotency_key_reused.
SELECT * FROM chat_idempotency
WHERE workspace_id = $1 AND user_id = $2 AND scope_type = $3 AND scope_id = $4 AND key = $5;

-- name: FinalizeChatIdempotency :execrows
-- Store the committed response (status/body) in the SAME transaction that
-- performed the side effects. Callers assert rows == 1.
UPDATE chat_idempotency
SET response_status = $6, response_body = $7
WHERE workspace_id = $1 AND user_id = $2 AND scope_type = $3 AND scope_id = $4 AND key = $5;

-- name: DeleteChatIdempotencyByKey :execrows
-- Release the reservation when the kernel failed and the key should be
-- reusable (merge-forward: sendProjectChatCore runs its own transaction and
-- cannot be wrapped).
DELETE FROM chat_idempotency
WHERE workspace_id = $1 AND user_id = $2 AND scope_type = $3 AND scope_id = $4 AND key = $5;

-- name: SweepChatIdempotency :execrows
-- 24h sweeper range delete, strict threshold (rows exactly 24h old are kept
-- this round; PRD FR-24 reads as a retention LOWER bound). Shape mirrors
-- SweepChatDraftAttachments' maxPerTick caller-side loop.
DELETE FROM chat_idempotency WHERE created_at < $1;
