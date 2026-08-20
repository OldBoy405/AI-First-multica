package drift

// AIFIRST: CR-2026-049 TASK-10 — finding repository and scheduler result v1
// codec (SDD §3.2/§4.1). E5 rows always carry spec_id/cr_id NULL and complete
// evidence; insert is ON CONFLICT DO NOTHING against the dedup index (388) —
// no select-before-insert anywhere.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the narrow pgx surface the drift repos need; *pgxpool.Pool and
// handler.dbExecutor both satisfy it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// FindingInput is one classified scan row.
type FindingInput struct {
	WorkspaceID  string
	RepositoryID string
	Kind         string // bypass-commit | wip-on-trunk
	Severity     string // info | warn
	Summary      string
	Evidence     json.RawMessage // {repository_id,trunk,commit_sha,commit_subject,scanned_at}
}

// FindingRepo persists findings through the dedup index.
type FindingRepo struct {
	pool Querier
}

func NewFindingRepo(pool Querier) *FindingRepo { return &FindingRepo{pool: pool} }

// UpsertFindings inserts all rows with ON CONFLICT DO NOTHING; the returned
// count is the number of rows actually inserted this round. spec_id/cr_id are
// always NULL for E5 scan findings.
func (r *FindingRepo) UpsertFindings(ctx context.Context, findings []FindingInput) (int64, error) {
	if len(findings) == 0 {
		return 0, nil
	}
	var inserted int64
	for _, f := range findings {
		tag, err := r.pool.Exec(ctx, `
			INSERT INTO drift_finding (workspace_id, repository_id, spec_id, cr_id, kind, severity, summary, evidence)
			VALUES ($1::uuid, $2, NULL, NULL, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`,
			f.WorkspaceID, f.RepositoryID, f.Kind, f.Severity, f.Summary, f.Evidence)
		if err != nil {
			return inserted, err
		}
		inserted += tag.RowsAffected()
	}
	return inserted, nil
}

// ── scheduler success result v1 (SDD §3.2) ──────────────────────────────────

// ScanResultV1 is the decoded success result the health read (TASK-11) and the
// handler both consume.
type ScanResultV1 struct {
	V             int               `json:"v"`
	ConfigRev     string            `json:"config_rev"`
	RepositoryIDs []string          `json:"repository_ids"`
	ScanCursors   map[string]string `json:"scan_cursors"`
	FindingCount  int64             `json:"finding_count"`
}

// EncodeResultV1 builds the handler result payload.
func EncodeResultV1(configRev string, repoIDs []string, cursors map[string]string, findingCount int64) map[string]any {
	return map[string]any{
		"v":              1,
		"config_rev":     configRev,
		"repository_ids": repoIDs,
		"scan_cursors":   cursors,
		"finding_count":  findingCount,
	}
}

// DecodeResultV1 parses a stored result JSONB; ok=false when the shape is
// unknown or malformed (health treats that as stale, never as ok).
func DecodeResultV1(result map[string]any) (ScanResultV1, bool) {
	raw, err := json.Marshal(result)
	if err != nil {
		return ScanResultV1{}, false
	}
	var out ScanResultV1
	if err := json.Unmarshal(raw, &out); err != nil {
		return ScanResultV1{}, false
	}
	if out.V != 1 || out.ConfigRev == "" || len(out.ScanCursors) == 0 {
		return ScanResultV1{}, false
	}
	return out, true
}
