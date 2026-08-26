package main

// contract-ignore: pre-390 fixture seed (pre-migration rows below are seeded with the old global key on purpose).
// AIFIRST: CR-2026-049 TASK-05 — cr_sync_event workspace migrations 390–397
// integration test (SDD §2.2/§2.4/§2.5). Runs the real migration files against
// a throwaway schema with minimal 362-shaped tables, asserts the deterministic
// preflight (orphan/multi-tenant hard fail), per-workspace idempotency, and a
// clean up/down roundtrip. Skips when the shared test Postgres is unreachable.

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

const workspaceMigrationDir = "../../migrations"

func workspaceMigration(t *testing.T, v string) string {
	return filepath.Join(workspaceMigrationDir, v)
}

func wsSchemaFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_ws_test_" + suffix
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, pgx.Identifier{schema}.Sanitize()))
	})
	return schema
}

// Minimal 362-shaped DDL (same column set / constraint names that 390-397 touch).
func wsSchemaDDL() []string {
	return []string{
		`CREATE TABLE cr (
			workspace_id UUID NOT NULL,
			cr_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'drafting',
			title TEXT NOT NULL DEFAULT '',
			owners JSONB NOT NULL DEFAULT '{}',
			target_version TEXT NOT NULL DEFAULT '',
			projected_commit TEXT NOT NULL DEFAULT '',
			needs_reconcile BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (workspace_id, cr_id)
		)`,
		`CREATE TABLE cr_sync_event (
			id BIGSERIAL PRIMARY KEY,
			cr_id TEXT NOT NULL,
			commit_sha TEXT NOT NULL DEFAULT '',
			event_kind TEXT NOT NULL,
			payload JSONB NOT NULL,
			evidence JSONB NOT NULL DEFAULT '{}',
			actor TEXT NOT NULL DEFAULT '',
			occurred_at TIMESTAMPTZ NOT NULL,
			received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			processed_at TIMESTAMPTZ,
			UNIQUE (cr_id, commit_sha, event_kind)
		)`,
		`CREATE INDEX idx_cr_sync_event_unprocessed ON cr_sync_event(cr_id, received_at) WHERE processed_at IS NULL`,
		`CREATE TABLE approval_record (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL,
			cr_id TEXT NOT NULL,
			stage TEXT NOT NULL,
			decision TEXT NOT NULL,
			approver_user_id UUID NOT NULL,
			evidence_digest TEXT NOT NULL,
			key_id TEXT NOT NULL,
			signature TEXT NOT NULL,
			reject_reason TEXT NOT NULL DEFAULT '',
			grant_json JSONB NOT NULL DEFAULT '{}',
			delivered_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE UNIQUE INDEX approval_record_approve_uniq ON approval_record (cr_id, stage, evidence_digest) WHERE decision = 'approve'`,
	}
}

func wsSchemaConn(t *testing.T, pool *pgxpool.Pool, schema string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(ctx, fmt.Sprintf(`SET search_path TO %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	return conn.Conn()
}

func runSQLFile(t *testing.T, conn *pgx.Conn, path string) error {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, err = conn.Exec(context.Background(), string(body))
	return err
}

var wsUps = []string{
	"461_cr_sync_event_workspace_id.up.sql",
	"462_cr_sync_event_workspace_uniq.up.sql",
	"463_cr_sync_event_trace_spec_idx.up.sql",
	"464_cr_sync_event_ws_unprocessed_idx.up.sql",
	"465_drop_cr_sync_event_old_uniq.up.sql",
	"466_drop_cr_sync_event_unprocessed_idx.up.sql",
	"467_approval_workspace_approve_uniq.up.sql",
	"468_drop_approval_record_approve_uniq.up.sql",
}

var wsDowns = []string{
	"468_drop_approval_record_approve_uniq.down.sql",
	"467_approval_workspace_approve_uniq.down.sql",
	"466_drop_cr_sync_event_unprocessed_idx.down.sql",
	"465_drop_cr_sync_event_old_uniq.down.sql",
	"464_cr_sync_event_ws_unprocessed_idx.down.sql",
	"463_cr_sync_event_trace_spec_idx.down.sql",
	"462_cr_sync_event_workspace_uniq.down.sql",
	"461_cr_sync_event_workspace_id.down.sql",
}

func TestCRSyncEventWorkspaceMigrationHappyPath(t *testing.T) {
	pool := openTestPool(t)
	schema := wsSchemaFixture(t, pool)
	conn := wsSchemaConn(t, pool, schema)
	ctx := context.Background()
	for _, ddl := range wsSchemaDDL() {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("fixture ddl: %v", err)
		}
	}
	wsA := "00000000-0000-0000-0000-00000000000a"
	wsB := "00000000-0000-0000-0000-00000000000b"
	// Backfill preflight requires one cr_id → one workspace: seed only wsA before
	// 461; the same-named wsB CR is created post-migration (new events carry
	// workspace_id directly, the new unique key is workspace-scoped).
	if _, err := conn.Exec(ctx, `INSERT INTO cr (workspace_id, cr_id) VALUES ($1,'CR-2026-001')`, wsA); err != nil {
		t.Fatalf("seed cr: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO cr_sync_event (cr_id, commit_sha, event_kind, payload, occurred_at)
		VALUES ('CR-2026-001','shaA','status','{}',now()),
		       ('CR-2026-001','shaB','status','{}',now())`); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	for _, v := range wsUps {
		if err := runSQLFile(t, conn, workspaceMigration(t, v)); err != nil {
			t.Fatalf("up %s: %v", v, err)
		}
	}
	// Backfill: every row got the workspace of its cr; column is NOT NULL.
	var nullCount int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM cr_sync_event WHERE workspace_id IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("null check: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("null workspace_id rows = %d", nullCount)
	}
	var notNull bool
	if err := conn.QueryRow(ctx, `
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'cr_sync_event'::regclass AND attname = 'workspace_id'`).Scan(&notNull); err != nil {
		t.Fatalf("notnull check: %v", err)
	}
	if !notNull {
		t.Errorf("workspace_id not NOT NULL after 390")
	}
	// New unique key is workspace-scoped: same (cr_id, commit_sha, event_kind)
	// lands once per workspace post-migration.
	tag, err := conn.Exec(ctx, `
		INSERT INTO cr_sync_event (workspace_id, cr_id, commit_sha, event_kind, payload, occurred_at)
		VALUES ($1,'CR-2026-001','shaA','status','{}',now())
		ON CONFLICT (workspace_id, cr_id, commit_sha, event_kind) DO NOTHING`, wsA)
	if err != nil {
		t.Fatalf("cross-workspace insert: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("same-workspace dedup insert rows = %d, want 0", tag.RowsAffected())
	}
	tag, err = conn.Exec(ctx, `
		INSERT INTO cr_sync_event (workspace_id, cr_id, commit_sha, event_kind, payload, occurred_at)
		VALUES ($1,'CR-2026-001','shaA','status','{}',now())
		ON CONFLICT (workspace_id, cr_id, commit_sha, event_kind) DO NOTHING`, wsB)
	if err != nil {
		t.Fatalf("cross-workspace insert: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("other-workspace same-key insert rows = %d, want 1", tag.RowsAffected())
	}
	// Old global indexes gone, new ones present (schema-scoped lookups).
	for _, idx := range []string{"cr_sync_event_cr_id_commit_sha_event_kind_key", "idx_cr_sync_event_unprocessed", "approval_record_approve_uniq"} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2)`, schema, idx).Scan(&exists); err != nil {
			t.Fatalf("old index %s: %v", idx, err)
		}
		if exists {
			t.Errorf("old index %s still exists after switch", idx)
		}
	}
	for _, idx := range []string{"cr_sync_event_workspace_dedup_idx", "cr_sync_event_trace_spec_idx", "cr_sync_event_ws_unprocessed_idx", "approval_record_approve_ws_uniq"} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2)`, schema, idx).Scan(&exists); err != nil {
			t.Fatalf("new index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("new index %s missing", idx)
		}
	}
	// Rolling back workspace scoping requires the cross-tenant same-key rows to
	// be reconciled first (the old global key cannot span them); drop wsB rows.
	if _, err := conn.Exec(ctx, `DELETE FROM cr_sync_event WHERE workspace_id = $1`, wsB); err != nil {
		t.Fatalf("cleanup wsB events: %v", err)
	}
	// Down roundtrip restores the pre-workspace shape.
	for _, v := range wsDowns {
		if err := runSQLFile(t, conn, workspaceMigration(t, v)); err != nil {
			t.Fatalf("down %s: %v", v, err)
		}
	}
	var colExists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'cr_sync_event' AND column_name = 'workspace_id')`, schema).Scan(&colExists); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if colExists {
		t.Errorf("workspace_id column still exists after down roundtrip")
	}
	for _, idx := range []string{"cr_sync_event_cr_id_commit_sha_event_kind_key", "idx_cr_sync_event_unprocessed", "approval_record_approve_uniq"} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2)`, schema, idx).Scan(&exists); err != nil {
			t.Fatalf("restored index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("old index %s not restored after down roundtrip", idx)
		}
	}
}

func TestCRSyncEventWorkspacePreflightBlocksOrphanAndAmbiguous(t *testing.T) {
	pool := openTestPool(t)

	t.Run("orphan", func(t *testing.T) {
		schema := wsSchemaFixture(t, pool)
		conn := wsSchemaConn(t, pool, schema)
		ctx := context.Background()
		for _, ddl := range wsSchemaDDL() {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("fixture ddl: %v", err)
			}
		}
		// Event with no cr row: count(DISTINCT workspace_id) = 0 <> 1.
		if _, err := conn.Exec(ctx, `
			INSERT INTO cr_sync_event (cr_id, commit_sha, event_kind, payload, occurred_at)
			VALUES ('CR-2026-999','shaX','status','{}',now())`); err != nil {
			t.Fatalf("seed orphan: %v", err)
		}
		if err := runSQLFile(t, conn, workspaceMigration(t, "461_cr_sync_event_workspace_id.up.sql")); err == nil {
			t.Fatalf("461 must hard-fail on orphan rows")
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		schema := wsSchemaFixture(t, pool)
		conn := wsSchemaConn(t, pool, schema)
		ctx := context.Background()
		for _, ddl := range wsSchemaDDL() {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("fixture ddl: %v", err)
			}
		}
		wsA := "00000000-0000-0000-0000-00000000000a"
		wsB := "00000000-0000-0000-0000-00000000000b"
		if _, err := conn.Exec(ctx, `INSERT INTO cr (workspace_id, cr_id) VALUES ($1,'CR-2026-001'),($2,'CR-2026-001')`, wsA, wsB); err != nil {
			t.Fatalf("seed cr: %v", err)
		}
		if _, err := conn.Exec(ctx, `
			INSERT INTO cr_sync_event (cr_id, commit_sha, event_kind, payload, occurred_at)
			VALUES ('CR-2026-001','shaA','status','{}',now())`); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if err := runSQLFile(t, conn, workspaceMigration(t, "461_cr_sync_event_workspace_id.up.sql")); err == nil {
			t.Fatalf("461 must hard-fail on ambiguous cr_id")
		}
	})
}
