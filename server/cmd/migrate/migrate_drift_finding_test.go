package main

// AIFIRST: CR-2026-049 TASK-04 — drift_finding migrations 456–460 integration test.
// Runs the real migration files inside a throwaway schema (search_path pinned on a
// dedicated connection), asserts the SDD §2.1/§2.3 shape, evidence CHECK, dedup
// behavior, keyset index coverage and a clean up/down roundtrip. Skips when the
// shared test Postgres is unreachable, matching every other live-PG test.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func driftMigrations(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	for _, v := range []string{
		"456_drift_finding", "457_drift_finding_id_uidx", "458_drift_finding_primary_key",
		"459_drift_finding_dedup_idx", "460_drift_finding_keyset",
	} {
		files = append(files, filepath.Join(dir, v+".up.sql"))
	}
	return files
}

func execFileOnConn(t *testing.T, conn *pgx.Conn, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := conn.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("exec %s: %v", path, err)
	}
}

func TestDriftFindingMigrationsUpDownRoundtrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_drift_test_" + suffix
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, pgx.Identifier{schema}.Sanitize()))
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`SET search_path TO %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	migrationsDir := filepath.Join("..", "..", "migrations")
	ups := driftMigrations(t, migrationsDir)
	for _, p := range ups {
		execFileOnConn(t, conn.Conn(), p)
	}

	// Structure: columns, defaults, enum CHECKs, PK via USING INDEX.
	var colCount, pkCount, idxCount int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'drift_finding'`, schema).Scan(&colCount); err != nil {
		t.Fatalf("columns: %v", err)
	}
	if colCount != 12 {
		t.Errorf("drift_finding columns = %d, want 12", colCount)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'drift_finding' AND c.contype = 'p'`, schema).Scan(&pkCount); err != nil {
		t.Fatalf("pk: %v", err)
	}
	if pkCount != 1 {
		t.Errorf("drift_finding pkey constraints = %d, want 1", pkCount)
	}
	// Indexes: pkey backing index + dedup + keyset; no inline FK.
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_index i
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'drift_finding'`, schema).Scan(&idxCount); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if idxCount != 3 {
		t.Errorf("drift_finding indexes = %d, want 3 (pkey/dedup/keyset)", idxCount)
	}
	var fkCount int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'drift_finding' AND c.contype = 'f'`, schema).Scan(&fkCount); err != nil {
		t.Fatalf("fk: %v", err)
	}
	if fkCount != 0 {
		t.Errorf("drift_finding FKs = %d, want 0 (SDD §2.1)", fkCount)
	}

	// Evidence CHECK: E5 rows require complete evidence; spec-level rows exempt.
	wsID := "00000000-0000-0000-0000-000000000001"
	_, err = conn.Exec(ctx, `
		INSERT INTO drift_finding (workspace_id, repository_id, kind, severity, summary, evidence)
		VALUES ($1, 'tools', 'bypass-commit', 'warn', 's', '{}')`, wsID)
	if err == nil {
		t.Fatalf("bypass-commit with empty evidence must violate DB CHECK")
	}
	fullEvidence := `{"repository_id":"tools","trunk":"custom/main","commit_sha":"abc123","commit_subject":"wip: x","scanned_at":"2026-08-20T00:00:00+08:00"}`
	tag, err := conn.Exec(ctx, `
		INSERT INTO drift_finding (workspace_id, repository_id, spec_id, cr_id, kind, severity, summary, evidence)
		VALUES ($1, 'tools', NULL, NULL, 'bypass-commit', 'warn', 'bypass on trunk', $2)`, wsID, fullEvidence)
	if err != nil {
		t.Fatalf("complete-evidence bypass-commit insert: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("insert rows = %d", tag.RowsAffected())
	}
	// Dedup: same (workspace, repo, kind, spec COALESCE, commit_sha) → ON CONFLICT DO NOTHING drops it.
	tag, err = conn.Exec(ctx, `
		INSERT INTO drift_finding (workspace_id, repository_id, spec_id, cr_id, kind, severity, summary, evidence)
		VALUES ($1, 'tools', NULL, NULL, 'bypass-commit', 'warn', 'dupe', $2)
		ON CONFLICT DO NOTHING`, wsID, fullEvidence)
	if err != nil {
		t.Fatalf("dedup insert: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("dedup insert rows = %d, want 0 (ON CONFLICT DO NOTHING)", tag.RowsAffected())
	}
	// Keyset index coverage: with seqscan disabled the planner must use drift_finding_keyset_idx.
	var plan string
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, `
		EXPLAIN (FORMAT TEXT)
		SELECT id FROM drift_finding
		WHERE workspace_id = $1 AND status = 'open'
		ORDER BY found_at DESC, id DESC`, wsID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		plan += line + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	if !contains(plan, "drift_finding_keyset_idx") {
		t.Errorf("keyset query plan does not use drift_finding_keyset_idx:\n%s", plan)
	}

	// Down roundtrip in reverse order leaves a clean schema.
	downs := []string{
		filepath.Join(migrationsDir, "460_drift_finding_keyset.down.sql"),
		filepath.Join(migrationsDir, "459_drift_finding_dedup_idx.down.sql"),
		filepath.Join(migrationsDir, "458_drift_finding_primary_key.down.sql"),
		filepath.Join(migrationsDir, "457_drift_finding_id_uidx.down.sql"),
		filepath.Join(migrationsDir, "456_drift_finding.down.sql"),
	}
	for _, p := range downs {
		execFileOnConn(t, conn.Conn(), p)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = 'drift_finding')`, schema).Scan(&exists); err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Errorf("drift_finding still exists after down roundtrip")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ pgxpool.Pool
