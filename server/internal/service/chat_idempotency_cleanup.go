package service

import (
	"context"
	"fmt"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SweepChatIdempotency deletes idempotency rows strictly older than cutoff
// (CR-2026-059 SDD §4.6, FR-24). The strict threshold keeps rows exactly 24h
// old this round — PRD FR-24 reads as a retention LOWER bound, mirroring the
// 168h boundary semantics of SweepChatDraftAttachments. The caller (cmd/server
// sweeper wiring, TASK-03) owns the hourly tick and the maxPerTick batching
// loop, matching the draft sweeper's shape.
func SweepChatIdempotency(ctx context.Context, q *db.Queries, cutoff time.Time) (int64, error) {
	if q == nil {
		return 0, fmt.Errorf("sweep chat idempotency: nil queries")
	}
	return q.SweepChatIdempotency(ctx, cutoff)
}
