package governance

// AIFIRST: CR-2026-049 TASK-07 — cross-CR traceability read service (SDD §3.5/§4.3).
// Reads the ledger-only trace events straight from cr_sync_event (expression
// index 392) and projects the FR evolution timeline: the display set is the
// latest VALID complete snapshot's milestones (deduped by (cr,milestone));
// event metadata (occurred_at,id) is mapped back onto milestones, history
// without a dedicated event stays visible as baseline-imported (document
// order, before event entries), and snapshot conflicts are marked rather than
// silently overwritten. Malformed historical rows never leak raw payloads.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbQuerier is the narrow pgx surface the trace service needs; *pgxpool.Pool
// and handler.dbExecutor both satisfy it.
type dbQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TraceService serves the trace timeline and spec search reads.
type TraceService struct {
	db dbQuerier
}

func NewTraceService(db dbQuerier) *TraceService { return &TraceService{db: db} }

// ── DTOs (locked by frontend zod schemas, TASK-12) ───────────────────────────

type MilestoneView struct {
	CR           string          `json:"cr"`
	Milestone    string          `json:"milestone"`
	FRS          json.RawMessage `json:"frs"` // unified frs/fr-chain, never null
	MergeCommits json.RawMessage `json:"merge_commits"`
	Evidence     json.RawMessage `json:"evidence"` // explicit null when missing (FR-7)
	Source       string          `json:"source"`   // event | baseline-imported
	Conflict     bool            `json:"trace_snapshot_conflict,omitempty"`
}

type TraceEventDTO struct {
	EventID    int64          `json:"event_id"`
	CRID       string         `json:"cr_id"`
	CommitSHA  string         `json:"commit_sha"`
	OccurredAt *time.Time     `json:"occurred_at"`
	State      string         `json:"state"` // ok | baseline-imported | malformed
	ErrorCode  string         `json:"error_code,omitempty"`
	Milestone  *MilestoneView `json:"milestone,omitempty"`
}

type SpecTimeline struct {
	V           int             `json:"v"`
	WorkspaceID string          `json:"workspace_id"`
	SpecID      string          `json:"spec_id"`
	Events      []TraceEventDTO `json:"events"`
}

type SpecSearchItem struct {
	SpecID     string          `json:"spec_id"`
	LatestCRID string          `json:"latest_cr_id"`
	Owners     json.RawMessage `json:"owners"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type SpecSearchPage struct {
	V          int              `json:"v"`
	Specs      []SpecSearchItem `json:"specs"`
	NextCursor *string          `json:"next_cursor"`
}

// ── timeline projection (pure) ───────────────────────────────────────────────

type traceRow struct {
	ID         int64
	CRID       string
	CommitSHA  string
	OccurredAt time.Time
	Payload    json.RawMessage
}

type parsedSnapshot struct {
	SpecID       string `json:"spec_id"`
	Traceability struct {
		SpecID     string            `json:"spec-id"`
		CRRef      string            `json:"cr-ref"`
		Milestones []json.RawMessage `json:"milestones"`
	} `json:"traceability"`
}

func semanticHash(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// milestoneKey dedups the display set by (cr, milestone).
func milestoneKey(raw json.RawMessage) (key string, view MilestoneView, ok bool) {
	var m struct {
		CR        string          `json:"cr"`
		Milestone string          `json:"milestone"`
		FRS       json.RawMessage `json:"frs"`
		FRChain   json.RawMessage `json:"fr-chain"`
		Merges    json.RawMessage `json:"merge-commits"`
		Evidence  json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.CR == "" || m.Milestone == "" {
		return "", MilestoneView{}, false
	}
	view = MilestoneView{
		CR: m.CR, Milestone: m.Milestone,
		FRS:          m.FRS,
		MergeCommits: m.Merges,
		Evidence:     m.Evidence,
	}
	if len(view.FRS) == 0 {
		view.FRS = m.FRChain
	}
	if len(view.FRS) == 0 {
		view.FRS = json.RawMessage(`[]`)
	}
	if len(view.MergeCommits) == 0 {
		view.MergeCommits = json.RawMessage(`[]`)
	}
	if len(view.Evidence) == 0 {
		view.Evidence = json.RawMessage(`null`)
	}
	return m.CR + "\x00" + m.Milestone, view, true
}

// ProjectTimeline is the pure projection: rows are already ordered by
// (occurred_at, id). Latest VALID complete snapshot owns the display set;
// per-key semantic hashes across all valid snapshots detect conflicts.
func ProjectTimeline(rows []traceRow) []TraceEventDTO {
	type eventMeta struct {
		occurredAt time.Time
		id         int64
	}
	eventByCR := map[string]eventMeta{}
	var events []TraceEventDTO
	var latest *parsedSnapshot
	// key → first semantic hash seen across all valid snapshots
	hashByKey := map[string]string{}
	conflictKeys := map[string]bool{}
	// displayed milestones from the latest snapshot (dedup first-wins, doc order)
	var displayed []struct {
		key  string
		view MilestoneView
	}

	visit := func(snap parsedSnapshot, row traceRow, commitHash bool) {
		seen := map[string]bool{}
		for _, raw := range snap.Traceability.Milestones {
			key, view, ok := milestoneKey(raw)
			if !ok {
				continue
			}
			h := semanticHash(raw)
			if h == "" {
				continue
			}
			if first, exists := hashByKey[key]; exists && first != h {
				conflictKeys[key] = true
			} else if !exists {
				hashByKey[key] = h
			}
			if !seen[key] {
				seen[key] = true
				displayed = append(displayed, struct {
					key  string
					view MilestoneView
				}{key, view})
			}
		}
	}

	for _, row := range rows {
		var snap parsedSnapshot
		if err := json.Unmarshal(row.Payload, &snap); err != nil || snap.SpecID == "" || snap.Traceability.Milestones == nil {
			events = append(events, TraceEventDTO{
				EventID: row.ID, CRID: row.CRID, CommitSHA: row.CommitSHA,
				OccurredAt: &row.OccurredAt, State: "malformed", ErrorCode: "trace_payload_invalid",
			})
			continue
		}
		// Malformed rows never replace the last valid event for their CR.
		eventByCR[row.CRID] = eventMeta{occurredAt: row.OccurredAt, id: row.ID}
		// Valid complete snapshot: becomes the latest display set and feeds
		// the conflict hash table.
		latest = &snap
		displayed = displayed[:0]
		visit(snap, row, false)
		events = append(events, TraceEventDTO{
			EventID: row.ID, CRID: row.CRID, CommitSHA: row.CommitSHA,
			OccurredAt: &row.OccurredAt, State: "ok",
		})
	}

	out := []TraceEventDTO{}
	if latest != nil {
		// baseline-imported first (document order), then event-backed entries
		// sorted by (occurred_at, id).
		eventEntries := map[string]TraceEventDTO{}
		for _, e := range events {
			if e.State == "ok" {
				eventEntries[e.CRID] = e
			} else {
				out = append(out, e)
			}
		}
		var eventBacked []TraceEventDTO
		for _, d := range displayed {
			view := d.view
			view.Conflict = conflictKeys[d.key]
			if _, ok := eventByCR[d.view.CR]; ok {
				view.Source = "event"
				e, exists := eventEntries[d.view.CR]
				if !exists {
					continue
				}
				e.Milestone = &view
				eventBacked = append(eventBacked, e)
			} else {
				view.Source = "baseline-imported"
				out = append(out, TraceEventDTO{
					EventID: 0, CRID: d.view.CR, State: "baseline-imported", Milestone: &view,
				})
			}
		}
		sort.SliceStable(eventBacked, func(i, j int) bool {
			left, right := eventByCR[eventBacked[i].CRID], eventByCR[eventBacked[j].CRID]
			if left.occurredAt.Equal(right.occurredAt) {
				return left.id < right.id
			}
			return left.occurredAt.Before(right.occurredAt)
		})
		out = append(out, eventBacked...)
	} else {
		out = append(out, events...)
	}
	return out
}

// SpecTimeline loads and projects the timeline for one spec in one workspace.
func (s *TraceService) SpecTimeline(ctx context.Context, workspaceID, specID string) (*SpecTimeline, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, cr_id, commit_sha, occurred_at, payload
		FROM cr_sync_event
		WHERE workspace_id = $1::uuid AND event_kind = 'trace' AND payload->>'spec_id' = $2
		ORDER BY occurred_at, id`, workspaceID, specID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []traceRow
	for rows.Next() {
		var r traceRow
		if err := rows.Scan(&r.ID, &r.CRID, &r.CommitSHA, &r.OccurredAt, &r.Payload); err != nil {
			return nil, err
		}
		events = append(events, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &SpecTimeline{V: 1, WorkspaceID: workspaceID, SpecID: specID, Events: ProjectTimeline(events)}, nil
}

// escapeLike escapes %, _ and \ for ILIKE patterns (ESCAPE '\').
func escapeLike(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	q = strings.ReplaceAll(q, `_`, `\_`)
	return q
}

// SpecSearch lists specs visible in this workspace (from trace events), joined
// to cr for owners, with keyset pagination over spec_id (SDD §3.5).
func (s *TraceService) SpecSearch(ctx context.Context, workspaceID, q, owner string, limit int, cursor *string) (*SpecSearchPage, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	cursorVal := ""
	if cursor != nil {
		cursorVal = *cursor
	}
	like := ""
	if q != "" {
		like = "%" + escapeLike(q) + "%"
	}
	rows, err := s.db.Query(ctx, `
		WITH ranked AS (
			SELECT payload->>'spec_id' AS spec_id,
			       cr_id,
			       occurred_at,
			       row_number() OVER (PARTITION BY payload->>'spec_id' ORDER BY occurred_at DESC, id DESC) AS rn
			FROM cr_sync_event
			WHERE workspace_id = $1::uuid AND event_kind = 'trace'
			  AND payload->>'spec_id' IS NOT NULL AND payload->>'spec_id' <> ''
		)
		SELECT r.spec_id, r.cr_id, r.occurred_at, COALESCE(c.owners, '{}'::jsonb) AS owners
		FROM ranked r
		LEFT JOIN cr c ON c.workspace_id = $1::uuid AND c.cr_id = r.cr_id
		WHERE r.rn = 1
		  AND ($5::text = '' OR r.spec_id > $5)
		  AND ($3::text = '' OR EXISTS (
		        SELECT 1
		        FROM cr_sync_event oe
		        JOIN cr oc ON oc.workspace_id = $1::uuid AND oc.cr_id = oe.cr_id
		        CROSS JOIN LATERAL jsonb_each(COALESCE(oc.owners, '{}'::jsonb)) o
		        WHERE oe.workspace_id = $1::uuid
		          AND oe.event_kind = 'trace'
		          AND oe.payload->>'spec_id' = r.spec_id
		          AND lower(o.value->>'id') = lower($3)))
		  AND ($4::text = '' OR r.spec_id ILIKE $4 ESCAPE '\'
		        OR EXISTS (
		        SELECT 1
		        FROM cr_sync_event oe
		        JOIN cr oc ON oc.workspace_id = $1::uuid AND oc.cr_id = oe.cr_id
		        CROSS JOIN LATERAL jsonb_each(COALESCE(oc.owners, '{}'::jsonb)) o
		        WHERE oe.workspace_id = $1::uuid
		          AND oe.event_kind = 'trace'
		          AND oe.payload->>'spec_id' = r.spec_id
		          AND o.value->>'id' ILIKE $4 ESCAPE '\'))
		ORDER BY r.spec_id
		LIMIT $2 + 1`, workspaceID, limit, owner, like, cursorVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &SpecSearchPage{V: 1, Specs: []SpecSearchItem{}}
	for rows.Next() {
		var item SpecSearchItem
		var owners []byte
		if err := rows.Scan(&item.SpecID, &item.LatestCRID, &item.UpdatedAt, &owners); err != nil {
			return nil, err
		}
		item.Owners = json.RawMessage(owners)
		page.Specs = append(page.Specs, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Specs) > limit {
		last := page.Specs[limit-1].SpecID
		page.Specs = page.Specs[:limit]
		page.NextCursor = &last
	}
	return page, nil
}
