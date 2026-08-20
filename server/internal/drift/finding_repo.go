package drift

// AIFIRST: CR-2026-049 TASK-11 — finding keyset list + status CAS (SDD §3.6).
// Ordering: (status_rank ASC, found_at DESC, id DESC) with rank
// open 0 / acknowledged 1 / resolved 2 / wontfix 3; the cursor is base64url
// JSON {rank,found_at,id} validated for shape and length. PATCH is a single
// SQL CAS on (id, workspace_id, status) — zero rows re-reads to distinguish
// 404 from 409, same-state replay is idempotent 200, resolved writes
// resolved_at=now(), wontfix keeps NULL.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFindingNotFound   = errors.New("not_found")
	ErrInvalidTransition = errors.New("invalid_transition")
	ErrInvalidCursor     = errors.New("invalid_cursor")
	ErrInvalidFilter     = errors.New("invalid_query")
)

// FindingDTO is one list/PATCH row (SDD §3.6).
type FindingDTO struct {
	ID           string          `json:"id"`
	RepositoryID string          `json:"repository_id"`
	SpecID       *string         `json:"spec_id"`
	CRID         *string         `json:"cr_id"`
	Kind         string          `json:"kind"`
	Severity     string          `json:"severity"`
	Summary      string          `json:"summary"`
	Evidence     json.RawMessage `json:"evidence"`
	Status       string          `json:"status"`
	FoundAt      time.Time       `json:"found_at"`
	ResolvedAt   *time.Time      `json:"resolved_at"`
}

// FindingsPage is the keyset page (SDD §3.6).
type FindingsPage struct {
	V          int          `json:"v"`
	Findings   []FindingDTO `json:"findings"`
	NextCursor *string      `json:"next_cursor"`
}

// ListFindingsFilter carries the optional filters.
type ListFindingsFilter struct {
	Status       string
	Kind         string
	RepositoryID string
}

type cursorPayload struct {
	Rank    int       `json:"rank"`
	FoundAt time.Time `json:"found_at"`
	ID      string    `json:"id"`
}

func statusRank(status string) (int, bool) {
	switch status {
	case "open":
		return 0, true
	case "acknowledged":
		return 1, true
	case "resolved":
		return 2, true
	case "wontfix":
		return 3, true
	default:
		return 0, false
	}
}

// EncodeCursor builds the base64url cursor for the last row of a page.
func EncodeCursor(c cursorPayload) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor validates shape and length (SDD §3.7 invalid_cursor).
func DecodeCursor(s string) (cursorPayload, error) {
	if len(s) > 512 {
		return cursorPayload{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorPayload{}, ErrInvalidCursor
	}
	var c cursorPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursorPayload{}, ErrInvalidCursor
	}
	if c.Rank < 0 || c.Rank > 3 {
		return cursorPayload{}, ErrInvalidCursor
	}
	if c.FoundAt.IsZero() || c.ID == "" {
		return cursorPayload{}, ErrInvalidCursor
	}
	return c, nil
}

// FindingQueryRepo reads/patches finding rows.
type FindingQueryRepo struct {
	pool Querier
}

func NewFindingQueryRepo(pool Querier) *FindingQueryRepo { return &FindingQueryRepo{pool: pool} }

// ListFindings pages findings with the rank/found_at/id keyset (SDD §3.6).
func (r *FindingQueryRepo) ListFindings(ctx context.Context, workspaceID string, filter ListFindingsFilter, limit int, cursor *string) (*FindingsPage, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	args := []any{workspaceID}
	sql := `
		SELECT id::text, repository_id, spec_id, cr_id, kind, severity, summary, evidence, status, found_at, resolved_at,
		       CASE status WHEN 'open' THEN 0 WHEN 'acknowledged' THEN 1 WHEN 'resolved' THEN 2 ELSE 3 END AS rank
		FROM drift_finding
		WHERE workspace_id = $1::uuid`
	n := 1
	if filter.Status != "" {
		if _, ok := statusRank(filter.Status); !ok {
			return nil, ErrInvalidFilter
		}
		n++
		sql += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, filter.Status)
	}
	if filter.Kind != "" {
		if filter.Kind != "alignment-drift" && filter.Kind != "impact-stale" && filter.Kind != "bypass-commit" && filter.Kind != "wip-on-trunk" {
			return nil, ErrInvalidFilter
		}
		n++
		sql += fmt.Sprintf(` AND kind = $%d`, n)
		args = append(args, filter.Kind)
	}
	if filter.RepositoryID != "" {
		n++
		sql += fmt.Sprintf(` AND repository_id = $%d`, n)
		args = append(args, filter.RepositoryID)
	}
	if cursor != nil && *cursor != "" {
		c, err := DecodeCursor(*cursor)
		if err != nil {
			return nil, err
		}
		// rank > :r OR (rank = :r AND found_at < :f) OR (rank = :r AND found_at = :f AND id < :i)
		n++
		sql += fmt.Sprintf(` AND (CASE status WHEN 'open' THEN 0 WHEN 'acknowledged' THEN 1 WHEN 'resolved' THEN 2 ELSE 3 END > $%d
			OR (CASE status WHEN 'open' THEN 0 WHEN 'acknowledged' THEN 1 WHEN 'resolved' THEN 2 ELSE 3 END = $%d AND found_at < $%d)
			OR (CASE status WHEN 'open' THEN 0 WHEN 'acknowledged' THEN 1 WHEN 'resolved' THEN 2 ELSE 3 END = $%d AND found_at = $%d AND id::text < $%d))`,
			n, n, n+1, n, n+1, n+2)
		args = append(args, c.Rank, c.FoundAt, c.ID)
		n += 2
	}
	sql += ` ORDER BY rank ASC, found_at DESC, id DESC LIMIT $` + fmt.Sprintf("%d", n+1)
	args = append(args, limit+1)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &FindingsPage{V: 1, Findings: []FindingDTO{}}
	for rows.Next() {
		var d FindingDTO
		var rank int
		if err := rows.Scan(&d.ID, &d.RepositoryID, &d.SpecID, &d.CRID, &d.Kind, &d.Severity, &d.Summary, &d.Evidence, &d.Status, &d.FoundAt, &d.ResolvedAt, &rank); err != nil {
			return nil, err
		}
		page.Findings = append(page.Findings, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Findings) > limit {
		last := page.Findings[limit-1]
		rk, _ := statusRank(last.Status)
		c := EncodeCursor(cursorPayload{Rank: rk, FoundAt: last.FoundAt, ID: last.ID})
		page.Findings = page.Findings[:limit]
		page.NextCursor = &c
	}
	return page, nil
}

// PatchStatus is the single-SQL CAS (SDD §3.6): open→{acknowledged,resolved,
// wontfix}, acknowledged→{resolved,wontfix}, same-state replay 200 idempotent,
// resolved/wontfix terminal, everything else invalid_transition. Cross-workspace
// ids are indistinguishable from missing (404).
func (r *FindingQueryRepo) PatchStatus(ctx context.Context, workspaceID, findingID, fromStatus, toStatus string) (*FindingDTO, error) {
	if fromStatus == toStatus {
		// Idempotent replay: confirm the row exists in this workspace with that status.
		d, err := r.getFinding(ctx, workspaceID, findingID)
		if err != nil {
			return nil, err
		}
		if d.Status != fromStatus {
			return nil, ErrInvalidTransition
		}
		return d, nil
	}
	allowed := false
	switch fromStatus {
	case "open":
		allowed = toStatus == "acknowledged" || toStatus == "resolved" || toStatus == "wontfix"
	case "acknowledged":
		allowed = toStatus == "resolved" || toStatus == "wontfix"
	}
	if !allowed {
		return nil, ErrInvalidTransition
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE drift_finding
		SET status = $3,
		    resolved_at = CASE WHEN $3 = 'resolved' THEN now() ELSE resolved_at END
		WHERE id = $1::uuid AND workspace_id = $2::uuid AND status = $4`,
		findingID, workspaceID, toStatus, fromStatus)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return r.getFinding(ctx, workspaceID, findingID)
	}
	// Zero rows: distinguish 404 from 409 by re-reading.
	d, err := r.getFinding(ctx, workspaceID, findingID)
	if err != nil {
		return nil, err
	}
	if d.Status == toStatus {
		return d, nil // concurrent same-state CAS won; idempotent success
	}
	return nil, ErrInvalidTransition
}

func (r *FindingQueryRepo) getFinding(ctx context.Context, workspaceID, findingID string) (*FindingDTO, error) {
	var d FindingDTO
	err := r.pool.QueryRow(ctx, `
		SELECT id::text, repository_id, spec_id, cr_id, kind, severity, summary, evidence, status, found_at, resolved_at
		FROM drift_finding
		WHERE id = $1::uuid AND workspace_id = $2::uuid`, findingID, workspaceID).
		Scan(&d.ID, &d.RepositoryID, &d.SpecID, &d.CRID, &d.Kind, &d.Severity, &d.Summary, &d.Evidence, &d.Status, &d.FoundAt, &d.ResolvedAt)
	if err != nil {
		if err.Error() == "no rows in result set" || strings.Contains(err.Error(), "no rows") {
			return nil, ErrFindingNotFound
		}
		return nil, err
	}
	return &d, nil
}
