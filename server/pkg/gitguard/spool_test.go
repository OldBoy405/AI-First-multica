package gitguard

// AIFIRST: denial spool tests (CR-2026-002 TASK-10).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpoolDenialWritesOutboxEvent(t *testing.T) {
	root := t.TempDir()
	if err := SpoolDenial(root, "agent-42", "push", CodeForbiddenSubcommand); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".crctl", "outbox")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 outbox file, got %d", len(entries))
	}
	name := entries[0].Name()
	if strings.HasPrefix(name, ".tmp-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("bad outbox file name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		V         int    `json:"v"`
		EventKind string `json:"event_kind"`
		CRID      string `json:"cr_id"`
		Payload   struct {
			Action string `json:"action"`
			Caller string `json:"caller"`
			Sub    string `json:"sub"`
			Code   string `json:"code"`
		} `json:"payload"`
		OccurredAt string `json:"occurred_at"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("outbox file must parse as event schema v1: %v", err)
	}
	if ev.V != 1 || ev.EventKind != "audit" || ev.CRID != "" || ev.OccurredAt == "" {
		t.Fatalf("bad envelope: %+v", ev)
	}
	p := ev.Payload
	if p.Action != ActionDenied || p.Caller != "agent-42" || p.Sub != "push" || p.Code != CodeForbiddenSubcommand {
		t.Fatalf("bad payload: %+v", p)
	}
	// Two denials in the same millisecond must not collide on the file name.
	if err := SpoolDenial(root, "agent-42", "push", CodeForbiddenSubcommand); err != nil {
		t.Fatal(err)
	}
	if entries, _ = os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("want 2 outbox files after second spool, got %d", len(entries))
	}
}

func TestSpoolDenialNoRoot(t *testing.T) {
	if err := SpoolDenial("", "agent", "push", CodeForbiddenSubcommand); err == nil {
		t.Fatal("empty root must error (caller treats spool as best-effort)")
	}
}
