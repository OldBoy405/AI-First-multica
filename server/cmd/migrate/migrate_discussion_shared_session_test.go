package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// AIFIRST: CR-2026-059 TASK-01 (SDD §2.1–§2.6 / §4.9, AC-19): up/down
// round-trip for the Discussion shared-session and idempotency migrations
// (481–490). Runs against a throwaway schema that pre-creates the minimal
// chat_session/chat_message/agent shapes the migrations alter, so the whole
// sequence can be applied and rolled back 490→481 without touching the real
// public schema.

// discussionMigrations returns the sorted 481–490 up (or down) file paths.
func discussionMigrations(t *testing.T, dir, direction string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*_*."+direction+".sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	var picked []string
	for _, p := range paths {
		base := filepath.Base(p)
		version := strings.SplitN(base, "_", 2)[0]
		if version >= "481" && version <= "490" {
			picked = append(picked, p)
		}
	}
	sort.Strings(picked)
	if direction == "down" {
		for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
			picked[i], picked[j] = picked[j], picked[i]
		}
	}
	return picked
}

func TestDiscussionSharedSessionMigrationsUpDownRoundtrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_discussion_" + suffix
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE")
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET search_path TO "+schemaIdent); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	// Minimal prerequisite shapes (same names/constraints the migrations
	// touch; the wide pre-kind unique index 484 drops is pre-created).
	prereqs := []string{
		"CREATE TABLE agent (id UUID PRIMARY KEY DEFAULT gen_random_uuid())",
		`CREATE TABLE chat_session (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID,
			agent_id UUID NOT NULL,
			creator_id UUID,
			title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			runtime_id UUID,
			project_id UUID,
			base_model TEXT,
			base_thinking_level TEXT,
			model_override TEXT,
			thinking_level_override TEXT)`,
		`ALTER TABLE chat_session ADD CONSTRAINT chat_session_agent_id_fkey
			FOREIGN KEY (agent_id) REFERENCES agent(id) ON DELETE CASCADE`,
		`CREATE UNIQUE INDEX chat_session_project_creator_active_unique
			ON chat_session (project_id, creator_id)
			WHERE project_id IS NOT NULL AND status = 'active'`,
		`CREATE TABLE chat_message (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			chat_session_id UUID NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			task_id UUID,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	}
	for _, stmt := range prereqs {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("prereq %s: %v", stmt, err)
		}
	}
	// One populated row: 481.down's SET NOT NULL must succeed on it, and
	// 482 keeps it 'private' via the column default.
	agentID := "00000000-0000-0000-0000-00000000000a"
	if _, err := conn.Exec(ctx, "INSERT INTO agent (id) VALUES ($1)", agentID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO chat_session (workspace_id, agent_id, creator_id, title)
		 VALUES ($1, $2, $2, 'Private Ask')`, agentID, agentID); err != nil {
		t.Fatalf("insert chat_session: %v", err)
	}

	migrationsDir := filepath.Join("..", "..", "migrations")
	ups := discussionMigrations(t, migrationsDir, "up")
	if len(ups) != 10 {
		t.Fatalf("found %d discussion up migrations, want 10", len(ups))
	}
	for _, p := range ups {
		execFileOnConn(t, conn.Conn(), p)
	}

	// 481: agent_id nullable + FK ON DELETE SET NULL.
	var isNullable string
	if err := conn.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name = 'agent_id'`,
		schema).Scan(&isNullable); err != nil {
		t.Fatalf("agent_id nullable check: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("agent_id is_nullable = %s, want YES", isNullable)
	}
	var fkDef string
	if err := conn.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'chat_session' AND c.conname = 'chat_session_agent_id_fkey'`,
		schema).Scan(&fkDef); err != nil {
		t.Fatalf("fk def: %v", err)
	}
	if !strings.Contains(fkDef, "ON DELETE SET NULL") {
		t.Errorf("fk def = %q, want ON DELETE SET NULL", fkDef)
	}

	// 483/485/488/490 indexes exist and are valid; 487 table has no inline
	// PK before 489 (asserted after the full up: the PK exists via USING
	// INDEX, plus the created_at index).
	for _, idx := range []string{
		"chat_session_private_creator_active_unique",
		"chat_session_project_shared_active_unique",
		"chat_idempotency_pkey",
		"idx_chat_idempotency_created",
	} {
		var count int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_index i
			JOIN pg_class t ON t.oid = i.indrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = $1 AND t.relname = CASE
				WHEN $2 IN ('chat_session_private_creator_active_unique','chat_session_project_shared_active_unique') THEN 'chat_session'
				ELSE 'chat_idempotency' END
			AND i.indexrelid = $3::regclass`,
			schema, idx, idx).Scan(&count); err != nil {
			t.Fatalf("index %s: %v", idx, err)
		}
		if count != 1 {
			t.Errorf("index %s present = %d, want 1", idx, count)
		}
	}
	// Old wide index dropped by 484.up.
	var oldWide int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'chat_session_project_creator_active_unique'`,
		schema).Scan(&oldWide); err != nil {
		t.Fatalf("old wide index: %v", err)
	}
	if oldWide != 0 {
		t.Errorf("old wide index still present after 484.up (%d)", oldWide)
	}
	// kind column with the private default for the pre-existing row.
	var kind string
	if err := conn.QueryRow(ctx, "SELECT kind FROM chat_session").Scan(&kind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kind != "private" {
		t.Errorf("pre-existing row kind = %q, want private", kind)
	}
	// 486: author columns exist.
	var authorCols int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_message'
		AND column_name IN ('author_type','author_id')`, schema).Scan(&authorCols); err != nil {
		t.Fatalf("author cols: %v", err)
	}
	if authorCols != 2 {
		t.Errorf("chat_message author columns = %d, want 2", authorCols)
	}

	// Rollback 490 → 481.
	downs := discussionMigrations(t, migrationsDir, "down")
	if len(downs) != 10 {
		t.Fatalf("found %d discussion down migrations, want 10", len(downs))
	}
	for _, p := range downs {
		execFileOnConn(t, conn.Conn(), p)
	}

	// Post-rollback: 481.down re-imposes NOT NULL + CASCADE.
	if err := conn.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'chat_session' AND c.conname = 'chat_session_agent_id_fkey'`,
		schema).Scan(&fkDef); err != nil {
		t.Fatalf("fk def after down: %v", err)
	}
	if !strings.Contains(fkDef, "ON DELETE CASCADE") {
		t.Errorf("fk def after down = %q, want ON DELETE CASCADE", fkDef)
	}
	if err := conn.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name = 'agent_id'`,
		schema).Scan(&isNullable); err != nil {
		t.Fatalf("agent_id nullable after down: %v", err)
	}
	if isNullable != "NO" {
		t.Errorf("agent_id is_nullable after down = %s, want NO", isNullable)
	}
	// 484.down restored the old wide index; 483.down dropped the new one;
	// 482.down dropped kind; 486.down dropped author columns; 487.down
	// dropped the idempotency table.
	var restored int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'chat_session_project_creator_active_unique'`,
		schema).Scan(&restored); err != nil {
		t.Fatalf("restored index: %v", err)
	}
	if restored != 1 {
		t.Errorf("old wide index restored after down = %d, want 1", restored)
	}
	var narrowed int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'chat_session_private_creator_active_unique'`,
		schema).Scan(&narrowed); err != nil {
		t.Fatalf("narrowed index: %v", err)
	}
	if narrowed != 0 {
		t.Errorf("narrowed index present after down = %d, want 0", narrowed)
	}
	var idempotencyTable int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = 'chat_idempotency'`, schema).Scan(&idempotencyTable); err != nil {
		t.Fatalf("idempotency table: %v", err)
	}
	if idempotencyTable != 0 {
		t.Errorf("chat_idempotency still present after 487.down (%d)", idempotencyTable)
	}
	var kindCols int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name = 'kind'`, schema).Scan(&kindCols); err != nil {
		t.Fatalf("kind col: %v", err)
	}
	if kindCols != 0 {
		t.Errorf("kind column still present after 482.down (%d)", kindCols)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_message'
		AND column_name IN ('author_type','author_id')`, schema).Scan(&authorCols); err != nil {
		t.Fatalf("author cols after down: %v", err)
	}
	if authorCols != 0 {
		t.Errorf("author columns still present after 486.down (%d)", authorCols)
	}
}
