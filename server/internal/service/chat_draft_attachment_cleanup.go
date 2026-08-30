package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SweepChatDraftAttachments implements the 1h draft TTL sweeper (SDD §4.10,
// AC-28): candidates are unbound draft rows — all five bind targets empty and
// no source context — older than 168 hours (strict: a row created exactly
// 168h00s ago is retained this round).
//
// Each candidate is processed in its own transaction: the locked re-read
// (FOR UPDATE SKIP LOCKED) re-checks the predicate under the row lock, the
// object delete runs while the lock is held (a concurrent send Bind waits on
// this row, so the object cannot be deleted out from under a just-bound
// draft), and only then the conditional DeleteUnboundDraftAttachment removes
// the row — never the shared DeleteAttachment, which would also delete
// already-bound rows (BLOCK-011).
//
// A nil storage, an empty URL, or a failed object delete rolls the candidate
// back for the next tick; the row survives with its URL so the retry can find
// it again. Returns the number of rows deleted this tick.
func SweepChatDraftAttachments(ctx context.Context, q *db.Queries, tx TxStarter, st storage.Storage, maxPerTick int32) (deleted int, err error) {
	candidates, err := q.ListUnboundDraftAttachmentCandidates(ctx, maxPerTick)
	if err != nil {
		return 0, fmt.Errorf("list draft attachment candidates: %w", err)
	}
	for _, id := range candidates {
		sweepTx, err := tx.Begin(ctx)
		if err != nil {
			return deleted, fmt.Errorf("begin draft sweep tx: %w", err)
		}
		qtx := q.WithTx(sweepTx)

		row, err := qtx.LockUnboundDraftAttachmentCandidate(ctx, id)
		if err != nil {
			_ = sweepTx.Rollback(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				// A send bound the row between the scan and the lock, or a
				// concurrent tick claimed it (SKIP LOCKED). Nothing to do.
				continue
			}
			return deleted, fmt.Errorf("lock draft attachment candidate: %w", err)
		}

		if st == nil || row.Url == "" {
			// No object store, or a legacy row with no URL: deleting the row
			// first would leak the object. Leave it for a later round.
			_ = sweepTx.Rollback(ctx)
			slog.Warn("draft attachment sweeper: candidate has no deletable storage; leaving for next tick",
				"attachment_id", util.UUIDToString(row.ID))
			continue
		}
		if err := st.DeleteObject(ctx, st.KeyFromURL(row.Url)); err != nil {
			_ = sweepTx.Rollback(ctx)
			slog.Warn("draft attachment sweeper: object delete failed; retrying next tick",
				"attachment_id", util.UUIDToString(row.ID), "error", err)
			continue
		}
		if _, err := qtx.DeleteUnboundDraftAttachment(ctx, db.DeleteUnboundDraftAttachmentParams{
			ID:          row.ID,
			WorkspaceID: row.WorkspaceID,
		}); err != nil {
			// The row lock makes a predicate miss impossible here (a Bind
			// UPDATE would have had to wait on this lock); any other failure
			// keeps the row for the next tick, which is strictly safer than
			// committing a half-swept candidate.
			_ = sweepTx.Rollback(ctx)
			slog.Warn("draft attachment sweeper: row delete failed; retrying next tick",
				"attachment_id", util.UUIDToString(row.ID), "error", err)
			continue
		}
		if err := sweepTx.Commit(ctx); err != nil {
			return deleted, fmt.Errorf("commit draft sweep: %w", err)
		}
		deleted++
	}
	return deleted, nil
}
