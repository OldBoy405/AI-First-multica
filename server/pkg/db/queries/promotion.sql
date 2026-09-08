-- AIFIRST: CR-2026-061 TASK-01 (SDD §4.2/§4.3/§4.5): promotion data access.
-- No new tables (FR-12): all writes ride existing issue/pipeline_run/
-- pipeline_node_run/attachment/chat_message relations.
--
-- FR-5 zero-write invariant: this file contains no INSERT/UPDATE/DELETE
-- against chat_message, attachment or chat_session — the promotion
-- transaction only READS chat rows (the attachment join below is a read).

-- name: FindPromotionDuplicateIssue :one
-- Dedupe lookup (SDD §4.2): containment match on the entry's dedupe_key;
-- 507 GIN index accelerates it. Caller treats ErrNoRows as "no duplicate".
SELECT * FROM issue
WHERE workspace_id = @workspace_id
  AND context_refs @> jsonb_build_array(jsonb_build_object('dedupe_key', @dedupe_key::text))
LIMIT 1;

-- name: AppendIssueContextRefs :exec
-- Idempotent append of a promotion entry to the array tail (SDD §2.1);
-- never overwrites existing entries.
UPDATE issue SET context_refs = context_refs || @context_refs::jsonb WHERE id = @id;

-- name: MergeIssueContextRefPipelineRun :one
-- Dedupe-hit backfill (SDD §4.3 step 10 / §2.1 append semantics): merge
-- pipeline_run_id into the single matched promotion element IN PLACE.
-- Every other array element and every unknown field of the matched
-- element survives verbatim — the element-level `||` merge never
-- re-marshals or drops history (B-CODE-01 regression fix). Returns the
-- merged array; the service verifies the matched element carries the
-- expected run id and fails the transaction otherwise.
UPDATE issue
SET context_refs = COALESCE((
    SELECT jsonb_agg(elem ORDER BY ord)
    FROM (
        SELECT t.ord,
               CASE
                   WHEN t.elem->>'kind' = 'discussion_promotion'
                    AND t.elem->>'dedupe_key' = @dedupe_key::text
                   THEN t.elem || jsonb_build_object('pipeline_run_id', @pipeline_run_id::text)
                   ELSE t.elem
               END AS elem
        FROM jsonb_array_elements(issue.context_refs) WITH ORDINALITY AS t(elem, ord)
    ) AS merged
), issue.context_refs)
WHERE id = @id
RETURNING context_refs;

-- name: InsertPipelineRun :one
-- Pre-built requirement-authoring run (SDD §2.3): cr_id NULL until the
-- bind transaction CAS-es it to the new CR-ID (same row, never a second).
-- id is the CALLER's pre-generated run id (dbid.NewV7, SDD §4.3) so the
-- first node insert can reference it in the same transaction.
INSERT INTO pipeline_run (
    id, workspace_id, pipeline_id, cr_id, issue_id, status, inputs, execution_context, started_by
) VALUES (
    @id, @workspace_id, 'requirement-authoring', sqlc.narg('cr_id'), @issue_id, 'running', @inputs, @execution_context, @started_by
)
RETURNING *;

-- name: InsertPipelineNodeRun :one
-- First node of the requirement-authoring template (SDD §2.3): seq=1,
-- kind='skill', ref='requirement-register', attempt=1, status='running'.
INSERT INTO pipeline_node_run (
    run_id, node_id, ref, kind, seq, status, attempt, started_at
) VALUES (
    @run_id, @node_id, @ref, @kind, @seq, @status, @attempt, now()
)
RETURNING *;

-- name: FindActiveRequirementRunForIssue :one
-- Recognition-key read (SDD §4.3): after a 506 unique violation on a
-- concurrent backfill, re-read the surviving run and adopt its id.
SELECT * FROM pipeline_run
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
  AND pipeline_id = 'requirement-authoring'
  AND status IN ('running', 'waiting_approval')
LIMIT 1;

-- name: FindUnboundPromotionRunByID :one
-- Bind transaction step 1 (SDD §4.5): lock the run row. Caller derives
-- RUN_NOT_FOUND / RUN_CR_CONFLICT from ErrNoRows / CrID mismatch.
SELECT * FROM pipeline_run
WHERE id = @id AND workspace_id = @workspace_id
FOR UPDATE;

-- name: BindPromotionRunIfNull :execrows
-- CAS write of run.cr_id (SDD §4.5 step 3). Returns the affected row
-- count; 0 rows after a locked read means a concurrent bind won — the
-- caller re-reads under the lock before deciding.
UPDATE pipeline_run
SET cr_id = @cr_id
WHERE id = @id AND cr_id IS NULL AND pipeline_id = 'requirement-authoring';

-- name: MarkPipelineNodePassed :execrows
-- First-node completion signal (SDD §4.5/D-5): running -> passed, in the
-- same bind transaction as the cr_id assignment. Only the running row is
-- targeted so a replay cannot resurrect a passed node.
UPDATE pipeline_node_run
SET status = 'passed'
WHERE run_id = @run_id AND node_id = @node_id AND status = 'running';

-- name: ListAttachmentsForPromotion :many
-- Source-attachment validation (SDD §4.3 step 6): attachments must be
-- bound to a message of the requested session (draft attachments have
-- chat_message_id IS NULL and never match). READ only — no chat writes.
SELECT a.* FROM attachment a
JOIN chat_message m ON m.id = a.chat_message_id
WHERE a.id = ANY(@attachment_ids::uuid[])
  AND m.chat_session_id = @chat_session_id
  AND a.chat_message_id IS NOT NULL;
