package drift

// AIFIRST: CR-2026-049 TASK-13 — static contract: finding inserts never run a
// select-before-insert loop (SDD §4.1/§7.2 AC-13). Idempotency is the dedup
// index + ON CONFLICT DO NOTHING; a SELECT-then-INSERT pattern here would be
// the race the schema was designed to avoid.

import (
	"os"
	"strings"
	"testing"
)

func TestFindingUpsertHasNoSelectBeforeInsert(t *testing.T) {
	body, err := os.ReadFile("finding.go")
	if err != nil {
		t.Fatalf("read finding.go: %v", err)
	}
	src := string(body)
	if strings.Contains(src, "SELECT") {
		t.Errorf("finding.go contains SELECT — findings must be INSERT ... ON CONFLICT DO NOTHING only (SDD §4.1)")
	}
	if !strings.Contains(src, "ON CONFLICT DO NOTHING") {
		t.Errorf("finding.go must upsert via ON CONFLICT DO NOTHING")
	}
}
