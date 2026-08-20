package governance

// AIFIRST: CR-2026-049 TASK-05 — static contract tests (SDD §2.5 seam list / §7.2):
// every cr_sync_event join, insert, conflict target and processed update in the
// server must be workspace-scoped; no spec_trace projection table may exist.
// Pure source scan, no database needed — a missing workspace predicate is a
// compile-passing tenant-isolation regression, so it must fail at test time.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readServerSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	roots := []string{"internal", "pkg", "cmd"}
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("../..", path)
		if err != nil {
			return err
		}
		rel = strings.ReplaceAll(rel, "\\", "/")
		top := strings.Split(rel, "/")[0]
		inScope := false
		for _, r := range roots {
			if top == r {
				inScope = true
				break
			}
		}
		if !inScope {
			return nil
		}
		if strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, ".sql") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk server sources: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no server sources scanned")
	}
	return out
}

func TestCRSyncEventEveryJoinIsWorkspaceScoped(t *testing.T) {
	// ON clauses of joins: `JOIN cr_sync_event e ON ...` must mention
	// workspace_id within the same ON expression (covers `e.cr_id = cr.cr_id
	// AND e.workspace_id = cr.workspace_id` and `e.workspace_id=r.workspace_id`).
	joinRe := regexp.MustCompile(`(?is)JOIN\s+cr_sync_event\s+\w+\s+ON\s+(.+?)(?:\n\s*(?:WHERE|JOIN|LEFT|INNER|GROUP|ORDER|\)|\z))`)
	for rel, body := range readServerSources(t) {
		for _, m := range joinRe.FindAllStringSubmatch(body, -1) {
			if !strings.Contains(m[1], "workspace_id") {
				t.Errorf("%s: cr_sync_event join missing workspace_id scope: %s", rel, strings.TrimSpace(m[1]))
			}
		}
	}
}

func TestCRSyncEventEveryInsertIsWorkspaceScoped(t *testing.T) {
	insertRe := regexp.MustCompile(`(?i)INSERT\s+INTO\s+cr_sync_event\s*\(([^)]*)\)`)
	for rel, body := range readServerSources(t) {
		if strings.Contains(body, "contract-ignore: pre-390 fixture seed") {
			continue
		}
		for _, m := range insertRe.FindAllStringSubmatch(body, -1) {
			cols := strings.ToLower(m[1])
			if !strings.Contains(cols, "workspace_id") {
				t.Errorf("%s: cr_sync_event INSERT missing workspace_id column: (%s)", rel, strings.TrimSpace(m[1]))
			}
		}
	}
}

func TestCRSyncEventConflictAndProcessedAreWorkspaceScoped(t *testing.T) {
	// Conflict targets on cr_sync_event inserts and processed_at updates must
	// carry workspace_id as the first key.
	conflictRe := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(([^)]*)\)`)
	processedRe := regexp.MustCompile(`(?is)UPDATE\s+cr_sync_event\s+SET\s+processed_at\s*=.*?WHERE\s+(.*?)\n`)
	for rel, body := range readServerSources(t) {
		if !strings.Contains(body, "cr_sync_event") {
			continue
		}
		for _, m := range conflictRe.FindAllStringSubmatch(body, -1) {
			if strings.Contains(m[1], "cr_id") && !strings.Contains(m[1], "workspace_id") {
				t.Errorf("%s: conflict target missing workspace_id: (%s)", rel, strings.TrimSpace(m[1]))
			}
		}
		for _, m := range processedRe.FindAllStringSubmatch(body, -1) {
			if !strings.Contains(m[1], "workspace_id") {
				t.Errorf("%s: processed_at UPDATE missing workspace_id scope: WHERE %s", rel, strings.TrimSpace(m[1]))
			}
		}
	}
}

func TestNoSpecTraceProjectionTable(t *testing.T) {
	// AC-4: trace reads go through cr_sync_event expression index; a parallel
	// spec_trace projection must not exist.
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?spec_trace\b`).Match(b) {
			t.Errorf("%s: spec_trace projection table found (SDD §5 forbids a second trace projection)", e.Name())
		}
	}
}
