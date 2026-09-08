package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AIFIRST: CR-2026-061 TASK-01 (SDD §2.2/§2.5/§2.6/§13, AC-10): up/down
// round-trip for the promotion migrations (505–507). Runs against a
// throwaway schema with the minimal shapes the migrations alter, so the
// sequence can be applied and rolled back without touching the public
// schema. The constraint name chat_idempotency_scope_type_check is
// confirmed via pg_constraint before the DROP in 505 relies on it.

// promotionMigrations returns the sorted 505–507 up (or down) file paths.
func promotionMigrations(t *testing.T, dir, direction string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*_*."+direction+".sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	var picked []string
	for _, p := range paths {
		base := filepath.Base(p)
		version := strings.SplitN(base, "_", 2)[0]
		if version >= "505" && version <= "507" {
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

func TestPromotionMigrationsUpDownRoundtrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_promotion_" + suffix
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

	// Minimal prerequisite shapes. chat_idempotency is created with the
	// inline column CHECK so PostgreSQL derives the same default constraint
	// name (chat_idempotency_scope_type_check) the real 501 created.
	prereqs := []string{
		`CREATE TABLE chat_idempotency (
			workspace_id UUID NOT NULL,
			user_id      UUID NOT NULL,
			scope_type   TEXT NOT NULL CHECK (scope_type IN ('discussion_message', 'merge_forward_messages')),
			scope_id     UUID NOT NULL,
			key          TEXT NOT NULL,
			fingerprint  TEXT NOT NULL,
			response_status INT NOT NULL,
			response_body   JSONB,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE issue (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL,
			context_refs JSONB)`,
		`CREATE TABLE pipeline_run (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL,
			pipeline_id TEXT NOT NULL,
			cr_id TEXT,
			issue_id UUID,
			status TEXT NOT NULL DEFAULT 'running',
			inputs JSONB NOT NULL DEFAULT '{}',
			execution_context JSONB NOT NULL DEFAULT '{}',
			started_by UUID NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			completed_at TIMESTAMPTZ)`,
		`CREATE TABLE chat_session (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL)`,
		`CREATE TABLE chat_message (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			chat_session_id UUID NOT NULL)`,
		`CREATE TABLE attachment (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL,
			chat_session_id UUID,
			chat_message_id UUID)`,
	}
	for _, stmt := range prereqs {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("prereq %s: %v", stmt, err)
		}
	}

	// Confirm the default constraint name before 505's DROP relies on it
	// (SDD §2.6 explicit verification requirement).
	var conName string
	if err := conn.QueryRow(ctx, `
		SELECT c.conname FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'chat_idempotency' AND c.contype = 'c'`,
		schema).Scan(&conName); err != nil {
		t.Fatalf("constraint lookup: %v", err)
	}
	if conName != "chat_idempotency_scope_type_check" {
		t.Fatalf("scope_type constraint name = %q, want chat_idempotency_scope_type_check", conName)
	}

	migrationsDir := filepath.Join("..", "..", "migrations")
	ups := promotionMigrations(t, migrationsDir, "up")
	if len(ups) != 3 {
		t.Fatalf("found %d promotion up migrations, want 3", len(ups))
	}
	for _, p := range ups {
		execFileOnConn(t, conn.Conn(), p)
	}

	// 505: the new scope_type enum is accepted and the old set still is.
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_idempotency (workspace_id, user_id, scope_type, scope_id, key, fingerprint, response_status)
		VALUES (gen_random_uuid(), gen_random_uuid(), 'discussion_promotion', gen_random_uuid(), 'k1', 'f1', 0)`); err != nil {
		t.Fatalf("insert discussion_promotion row after 505: %v", err)
	}
	var checkDef string
	if err := conn.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND t.relname = 'chat_idempotency' AND c.conname = 'chat_idempotency_scope_type_check'`,
		schema).Scan(&checkDef); err != nil {
		t.Fatalf("constraint def: %v", err)
	}
	if !strings.Contains(checkDef, "'discussion_promotion'") {
		t.Errorf("constraint def = %q, want discussion_promotion in enum", checkDef)
	}

	// 506: second non-terminal requirement-authoring run for the same
	// (workspace, issue) must raise a unique violation (23505); a run for a
	// different issue or a NULL issue_id must not conflict.
	wsID := "00000000-0000-0000-0000-000000000061"
	issueID := "00000000-0000-0000-0000-000000000062"
	otherIssue := "00000000-0000-0000-0000-000000000063"
	insertRun := func(issue string) error {
		_, err := conn.Exec(ctx, `
			INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, issue_id, status, inputs, execution_context, started_by)
			VALUES ($1::uuid, 'requirement-authoring', NULL, $2::uuid, 'running', '{}'::jsonb, '{}'::jsonb, $1::uuid)`,
			wsID, issue)
		return err
	}
	if err := insertRun(issueID); err != nil {
		t.Fatalf("first run insert: %v", err)
	}
	err = insertRun(issueID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("second run for same issue: err = %v, want unique violation 23505", err)
	}
	if err := insertRun(otherIssue); err != nil {
		t.Errorf("run for different issue must not conflict: %v", err)
	}

	// 507: the GIN index exists and is valid.
	assertPromotionIndexExists(t, conn.Conn(), schema, "issue", "idx_issue_context_refs_gin", true)

	// Down direction: 505.down is data-dependent — it must fail while a
	// discussion_promotion row remains and succeed after the row is gone.
	if _, err := conn.Exec(ctx, execFileBody(t, filepath.Join(migrationsDir, "505_chat_idempotency_promotion_scope.down.sql"))); err == nil {
		t.Error("505.down succeeded with discussion_promotion rows present, want failure")
	}
	if _, err := conn.Exec(ctx, "DELETE FROM chat_idempotency WHERE scope_type = 'discussion_promotion'"); err != nil {
		t.Fatalf("delete promotion rows: %v", err)
	}
	if _, err := conn.Exec(ctx, execFileBody(t, filepath.Join(migrationsDir, "505_chat_idempotency_promotion_scope.down.sql"))); err != nil {
		t.Fatalf("505.down after cleanup: %v", err)
	}
	// Old enum restored: discussion_promotion is rejected again.
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_idempotency (workspace_id, user_id, scope_type, scope_id, key, fingerprint, response_status)
		VALUES (gen_random_uuid(), gen_random_uuid(), 'discussion_promotion', gen_random_uuid(), 'k2', 'f2', 0)`); err == nil {
		t.Error("discussion_promotion insert after 505.down succeeded, want CHECK failure")
	}

	// 506/507 downs: plain index drops.
	if _, err := conn.Exec(ctx, execFileBody(t, filepath.Join(migrationsDir, "506_pipeline_run_promotion_active_unique.down.sql"))); err != nil {
		t.Fatalf("506.down: %v", err)
	}
	if _, err := conn.Exec(ctx, execFileBody(t, filepath.Join(migrationsDir, "507_issue_context_refs_gin.down.sql"))); err != nil {
		t.Fatalf("507.down: %v", err)
	}
	assertPromotionIndexExists(t, conn.Conn(), schema, "pipeline_run", "idx_pipeline_run_promotion_active_issue", false)
	assertPromotionIndexExists(t, conn.Conn(), schema, "issue", "idx_issue_context_refs_gin", false)
}

func execFileBody(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func assertPromotionIndexExists(t *testing.T, conn *pgx.Conn, schema, table, index string, want bool) {
	t.Helper()
	var valid bool
	err := conn.QueryRow(context.Background(), `
		SELECT i.indisvalid FROM pg_index i
		JOIN pg_class idx ON idx.oid = i.indexrelid
		JOIN pg_class tbl ON tbl.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = tbl.relnamespace
		WHERE n.nspname = $1 AND tbl.relname = $2 AND idx.relname = $3`,
		schema, table, index).Scan(&valid)
	switch {
	case err == nil && !want:
		t.Errorf("index %s unexpectedly exists", index)
	case err != nil && want:
		t.Errorf("index %s missing: %v", index, err)
	case err == nil && want && !valid:
		t.Errorf("index %s exists but is invalid", index)
	}
}
