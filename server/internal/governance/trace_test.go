package governance

// AIFIRST: CR-2026-049 TASK-07 — trace read service tests (SDD §3.5/§7.2 AC-5/AC-7).
// Pure ProjectTimeline table tests (display set from latest valid snapshot,
// baseline-imported doc order, event timing, conflict marking, malformed
// handling, evidence-missing) plus live-PG service tests for cross-workspace
// isolation and spec-search filtering/pagination.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func ts(t time.Time) *time.Time { return &t }

func TestProjectTimelineLatestSnapshotOwnsDisplaySet(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	payload := func(crRef string, crs []string) json.RawMessage {
		milestones := "["
		for i, cr := range crs {
			if i > 0 {
				milestones += ","
			}
			milestones += `{"cr":"` + cr + `","milestone":"M` + string(rune('0'+i)) + `","frs":[{"fr":"FR-1"}]}`
		}
		milestones += "]"
		return json.RawMessage(`{"spec_id":"s","traceability":{"spec-id":"s","cr-ref":"` + crRef + `","milestones":` + milestones + `}}`)
	}
	rows := []traceRow{
		// First snapshot imports history A,B (no dedicated events for them).
		{ID: 1, CRID: "CR-2026-002", CommitSHA: "c2", OccurredAt: base, Payload: payload("CR-2026-002", []string{"CR-2026-001", "CR-2026-002"})},
		// Later snapshot adds C; A,B remain as baseline-imported.
		{ID: 2, CRID: "CR-2026-003", CommitSHA: "c3", OccurredAt: base.Add(time.Hour), Payload: payload("CR-2026-003", []string{"CR-2026-001", "CR-2026-002", "CR-2026-003"})},
	}
	out := ProjectTimeline(rows)
	if len(out) != 3 {
		t.Fatalf("entries = %d, want 3", len(out))
	}
	// baseline-imported first in doc order (CR-2026-001 has no dedicated event).
	if out[0].CRID != "CR-2026-001" || out[0].State != "baseline-imported" || out[0].Milestone.Source != "baseline-imported" {
		t.Errorf("entry0 = %+v", out[0])
	}
	// event-backed entries follow, ordered by (occurred_at, id).
	if out[1].CRID != "CR-2026-002" || out[1].State != "ok" || out[1].EventID != 1 || out[1].CommitSHA != "c2" {
		t.Errorf("entry1 = %+v", out[1])
	}
	if out[2].CRID != "CR-2026-003" || out[2].State != "ok" || out[2].EventID != 2 || out[2].CommitSHA != "c3" {
		t.Errorf("entry2 = %+v", out[2])
	}
	if out[2].Milestone.FRS == nil || string(out[2].Milestone.FRS) != `[{"fr":"FR-1"}]` {
		t.Errorf("frs not unified: %s", out[2].Milestone.FRS)
	}
}

func TestProjectTimelineConflictAndMalformed(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	payload := func(crRef, cr, frTitle string) json.RawMessage {
		return json.RawMessage(`{"spec_id":"s","traceability":{"spec-id":"s","cr-ref":"` + crRef + `","milestones":[{"cr":"` + cr + `","milestone":"M0","frs":[{"fr":"FR-1","title":"` + frTitle + `"}]}]}}`)
	}
	rows := []traceRow{
		{ID: 1, CRID: "CR-2026-001", CommitSHA: "c1", OccurredAt: base, Payload: payload("CR-2026-001", "CR-2026-001", "v1")},
		// Same key, different semantic content → conflict must be marked.
		{ID: 2, CRID: "CR-2026-001", CommitSHA: "c1b", OccurredAt: base.Add(time.Hour), Payload: payload("CR-2026-001", "CR-2026-001", "v2")},
		// Malformed historical row: state=malformed, no raw payload leak.
		{ID: 3, CRID: "CR-2026-002", CommitSHA: "c2", OccurredAt: base.Add(2 * time.Hour), Payload: json.RawMessage(`{"spec_id":"s","traceability":"not-an-object"}`)},
	}
	out := ProjectTimeline(rows)
	var foundConflict, foundMalformed bool
	for _, e := range out {
		if e.State == "malformed" {
			foundMalformed = true
			if e.ErrorCode != "trace_payload_invalid" || e.Milestone != nil {
				t.Errorf("malformed entry shape: %+v", e)
			}
		}
		if e.Milestone != nil && e.Milestone.Conflict {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Errorf("same-key semantic drift must be marked trace_snapshot_conflict")
	}
	if !foundMalformed {
		t.Errorf("malformed row must appear as state=malformed")
	}
}

func TestProjectTimelineEvidenceMissingIsExplicitNull(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"spec_id":"s","traceability":{"spec-id":"s","cr-ref":"CR-2026-001","milestones":[{"cr":"CR-2026-001","milestone":"M0","frs":[{"fr":"FR-1"}]}]}}`)
	out := ProjectTimeline([]traceRow{{ID: 1, CRID: "CR-2026-001", CommitSHA: "c1", OccurredAt: base, Payload: payload}})
	if len(out) != 1 || out[0].Milestone.Evidence == nil {
		t.Fatalf("missing evidence must be explicit: %+v", out)
	}
	if string(out[0].Milestone.Evidence) != "null" {
		t.Errorf("evidence = %s, want null", out[0].Milestone.Evidence)
	}
}

func TestTraceServiceTimelineCrossWorkspaceIsolation(t *testing.T) {
	crID := "CR-9999-011"
	resetCR(t, crID)
	defer resetCR(t, crID)
	otherWS := "00000000-0000-0000-0000-0000000000ff"

	payloadA := validTracePayload(crID)
	payloadB := json.RawMessage(`{"spec_id":"other-spec","traceability":{"spec-id":"other-spec","cr-ref":"` + crID + `","milestones":[{"cr":"` + crID + `","milestone":"M0","frs":[]}]}}`)

	svc := NewSyncService(testPool, nil)
	if resp := postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{traceEvent(crID, "trace-iso-a", "a.json", payloadA)}); len(resp.Accepted) != 1 {
		t.Fatalf("seed A: %+v", resp)
	}
	if resp := postEventsRaw(t, svc, otherWS, []OutboxEvent{traceEvent(crID, "trace-iso-b", "b.json", payloadB)}); len(resp.Accepted) != 1 {
		t.Fatalf("seed B: %+v", resp)
	}

	ts := NewTraceService(testPool)
	timeline, err := ts.SpecTimeline(context.Background(), testWorkspaceID, "test-spec")
	if err != nil {
		t.Fatal(err)
	}
	// validTracePayload carries history CR-2026-001 (baseline-imported) + the
	// event CR; the event-backed entry must be the last one.
	if len(timeline.Events) != 2 {
		t.Fatalf("workspace A timeline = %+v", timeline.Events)
	}
	last := timeline.Events[len(timeline.Events)-1]
	if last.CRID != crID || last.State != "ok" || last.CommitSHA != "trace-iso-a" {
		t.Fatalf("event entry = %+v", last)
	}
	if timeline.Events[0].State != "baseline-imported" {
		t.Fatalf("history entry = %+v", timeline.Events[0])
	}
	// other-spec must not leak into workspace A.
	timelineOther, err := ts.SpecTimeline(context.Background(), testWorkspaceID, "other-spec")
	if err != nil {
		t.Fatal(err)
	}
	if len(timelineOther.Events) != 0 {
		t.Fatalf("cross-workspace spec leaked: %+v", timelineOther.Events)
	}
}

func TestTraceServiceSpecSearchFilterAndCursor(t *testing.T) {
	crA, crB := "CR-9999-012", "CR-9999-013"
	resetCR(t, crA)
	resetCR(t, crB)
	defer resetCR(t, crA)
	defer resetCR(t, crB)

	// cr rows carry owners (free-text identity "Ray").
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO cr (workspace_id, cr_id, status, owners) VALUES
		($1::uuid, $2, 'archived', '{"requirement":{"id":"Ray"}}'),
		($1::uuid, $3, 'archived', '{"requirement":{"id":"Ada"}}')
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET owners = EXCLUDED.owners`,
		testWorkspaceID, crA, crB); err != nil {
		t.Fatal(err)
	}
	payloadFor := func(cr, spec string) json.RawMessage {
		return json.RawMessage(`{"spec_id":"` + spec + `","traceability":{"spec-id":"` + spec + `","cr-ref":"` + cr + `","milestones":[{"cr":"` + cr + `","milestone":"M0","frs":[]}]}}`)
	}
	svc := NewSyncService(testPool, nil)
	postEventsRaw(t, svc, testWorkspaceID, []OutboxEvent{
		traceEvent(crA, "trace-search-a", "a.json", payloadFor(crA, "alpha-spec")),
		traceEvent(crB, "trace-search-b", "b.json", payloadFor(crB, "beta-spec")),
	})

	tr := NewTraceService(testPool)
	// Owner exact (case-insensitive): only alpha-spec (Ray).
	page, err := tr.SpecSearch(context.Background(), testWorkspaceID, "", "ray", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Specs) != 1 || page.Specs[0].SpecID != "alpha-spec" {
		t.Fatalf("owner filter: %+v", page.Specs)
	}
	// q filter on spec_id.
	page, err = tr.SpecSearch(context.Background(), testWorkspaceID, "beta", "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Specs) != 1 || page.Specs[0].SpecID != "beta-spec" {
		t.Fatalf("q filter: %+v", page.Specs)
	}
	// Keyset cursor: limit=1 → next_cursor = alpha-spec; follow it.
	page, err = tr.SpecSearch(context.Background(), testWorkspaceID, "", "", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Specs) != 1 || page.Specs[0].SpecID != "alpha-spec" || page.NextCursor == nil {
		t.Fatalf("first page: %+v", page)
	}
	page2, err := tr.SpecSearch(context.Background(), testWorkspaceID, "", "", 10, page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Specs) != 1 || page2.Specs[0].SpecID != "beta-spec" || page2.NextCursor != nil {
		t.Fatalf("second page: %+v", page2)
	}
}
