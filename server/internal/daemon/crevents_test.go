package daemon

// AIFIRST: tests for the CR event collector (CR-2026-002 TASK-06). Pure
// filesystem + fake reporter + a throwaway git repo; no server or DB needed.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type fakeReporter struct {
	reports []crEventsReport
	ack     func(crEventsReport) crEventsAck
	err     error
}

func (f *fakeReporter) ReportCREvents(_ context.Context, r crEventsReport) (crEventsAck, error) {
	if f.err != nil {
		return crEventsAck{}, f.err
	}
	f.reports = append(f.reports, r)
	return f.ack(r), nil
}

func acceptAll(r crEventsReport) crEventsAck {
	ack := crEventsAck{Accepted: []string{}}
	for _, e := range r.Events {
		ack.Accepted = append(ack.Accepted, e.File)
	}
	return ack
}

func writeOutboxFile(t *testing.T, root, name string, ev crOutboxEvent) {
	t.Helper()
	dir := filepath.Join(root, ".crctl", "outbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ev)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func outboxFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".crctl", "outbox"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func testEvent(cr, kind, sha string) crOutboxEvent {
	return crOutboxEvent{V: 1, EventKind: kind, CRID: cr, FromStatus: "", ToStatus: "drafting",
		Trigger: "requirement-register", CommitSHA: sha, Actor: "t", OccurredAt: time.Now()}
}

func newTestCollector(root string, rep crEventReporter) *crEventCollector {
	return newCREventCollector([]string{root}, rep, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

func gitInitWithCRCommit(t *testing.T, root, message string) string {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "master")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", message)
	sha, err := gitLine(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestDualChannelMergedWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	// A [cr] status commit — the fallback channel will see it …
	sha := gitInitWithCRCommit(t, root, "[cr] status CR-2026-001 drafting -> requirement-reviewing")
	// … and the outbox holds the SAME event (richer, with trigger).
	ev := crOutboxEvent{V: 1, EventKind: "status", CRID: "CR-2026-001", FromStatus: "drafting",
		ToStatus: "requirement-reviewing", Trigger: "review-requirement", CommitSHA: sha, OccurredAt: time.Now()}
	writeOutboxFile(t, root, "e1.json", ev)

	rep := &fakeReporter{ack: acceptAll}
	col := newTestCollector(root, rep)
	if err := col.collectRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if len(rep.reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(rep.reports))
	}
	events := rep.reports[0].Events
	if len(events) != 1 {
		t.Fatalf("dual-channel event must be merged to one, got %d: %+v", len(events), events)
	}
	if events[0].Trigger != "review-requirement" {
		t.Fatalf("outbox (richer) event must win the merge, got trigger=%q", events[0].Trigger)
	}
	// Cursor advanced: a second pass with no new commits/files reports nothing.
	rep.reports = nil
	if err := col.collectRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if len(rep.reports) != 0 {
		t.Fatalf("second pass must be empty, got %+v", rep.reports)
	}
}

func TestPartialAcceptDeletesOnlyAckedFiles(t *testing.T) {
	root := t.TempDir()
	writeOutboxFile(t, root, "a.json", testEvent("CR-2026-001", "status", "s1"))
	writeOutboxFile(t, root, "b.json", testEvent("CR-2026-002", "status", "s2"))
	rep := &fakeReporter{ack: func(r crEventsReport) crEventsAck {
		return crEventsAck{Accepted: []string{"a.json"}, Rejected: []struct {
			File string `json:"file"`
			Code string `json:"code"`
		}{{File: "b.json", Code: "BAD_EVENT"}}}
	}}
	col := newTestCollector(root, rep)
	if err := col.collectRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	files := outboxFiles(t, root)
	if len(files) != 1 || files[0] != "b.json" {
		t.Fatalf("only acked a.json must be deleted; remaining: %v", files)
	}
}

func TestPoisonedFileQuarantinedAfterThreeRejections(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".crctl", "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Corrupt JSON: scanner turns it into a V=0 event that the server rejects.
	if err := os.WriteFile(filepath.Join(root, ".crctl", "outbox", "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &fakeReporter{ack: func(r crEventsReport) crEventsAck {
		ack := crEventsAck{}
		for _, e := range r.Events {
			ack.Rejected = append(ack.Rejected, struct {
				File string `json:"file"`
				Code string `json:"code"`
			}{File: e.File, Code: "BAD_EVENT"})
		}
		return ack
	}}
	col := newTestCollector(root, rep)
	for i := 0; i < 3; i++ {
		if err := col.collectRoot(context.Background(), root); err != nil {
			t.Fatal(err)
		}
	}
	if files := outboxFiles(t, root); len(files) != 0 {
		t.Fatalf("poisoned file must be gone from outbox (moved to dead/), outbox now: %v", files)
	}
	if _, err := os.Stat(filepath.Join(root, ".crctl", "outbox", "dead", "bad.json")); err != nil {
		t.Fatalf("bad.json must be in dead/: %v", err)
	}
}

func TestNetworkFailureKeepsBacklogAndCursor(t *testing.T) {
	root := t.TempDir()
	sha := gitInitWithCRCommit(t, root, "[cr] status CR-2026-001 drafting -> requirement-reviewing")
	_ = sha
	writeOutboxFile(t, root, "e1.json", testEvent("CR-2026-001", "status", "zzz"))
	rep := &fakeReporter{err: context.DeadlineExceeded}
	col := newTestCollector(root, rep)
	if err := col.collectRoot(context.Background(), root); err == nil {
		t.Fatal("network failure must surface as error")
	}
	if files := outboxFiles(t, root); len(files) != 1 {
		t.Fatalf("backlog must be kept on network failure: %v", files)
	}
	if _, err := os.Stat(filepath.Join(root, ".crctl", ".scan-cursor")); !os.IsNotExist(err) {
		t.Fatal("cursor must NOT advance when the report failed")
	}
}

func TestParseCRCommitMessageContracts(t *testing.T) {
	cases := []struct {
		subject  string
		wantKind string
		wantCR   string
		ok       bool
	}{
		{"[cr] status CR-2026-002 drafting -> requirement-reviewing", "status", "CR-2026-002", true},
		{"[cr] merge metadata CR-2026-002", "merge", "CR-2026-002", true},
		{"[cr] archive CR-2026-002", "archive", "CR-2026-002", true},
		{"[cr] inbox-emit CR-2026-002 event=handover", "inbox", "CR-2026-002", true},
		{"[cr] CR-2026-002: TASK-05 done", "", "", false}, // bookkeeping commit, not a contract message
		{"feat: unrelated", "", "", false},
	}
	for _, c := range cases {
		ev, ok := parseCRCommitMessage("abc", "2026-07-31T10:00:00+08:00", c.subject)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.subject, ok, c.ok)
			continue
		}
		if ok && (ev.EventKind != c.wantKind || ev.CRID != c.wantCR) {
			t.Errorf("%q: got (%s,%s) want (%s,%s)", c.subject, ev.EventKind, ev.CRID, c.wantKind, c.wantCR)
		}
	}
}

// ── TASK-07 daemon-mode reconcile: periodic snapshot events ─────────────────

func TestSnapshotEventEmittedAndThrottled(t *testing.T) {
	root := t.TempDir()
	sha := gitInitWithCRCommit(t, root, "wip: seed")
	if err := os.MkdirAll(filepath.Join(root, "change-requests"), 0o755); err != nil {
		t.Fatal(err)
	}
	backlog := "change-requests:\n  - id: CR-2026-002\n    status: developing\n"
	if err := os.WriteFile(filepath.Join(root, "change-requests", "_backlog.yml"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := &fakeReporter{ack: acceptAll}
	col := newTestCollector(root, rep)
	col.collectAll(context.Background())
	if len(rep.reports) == 0 {
		t.Fatal("first pass must report (snapshot due immediately)")
	}
	var snap *crOutboxEvent
	for i := range rep.reports[0].Events {
		if rep.reports[0].Events[i].EventKind == "snapshot" {
			snap = &rep.reports[0].Events[i]
		}
	}
	if snap == nil {
		t.Fatalf("no snapshot event in first report: %+v", rep.reports[0].Events)
	}
	if snap.File != "snapshot:"+sha {
		t.Fatalf("bad synthetic file id %q", snap.File)
	}
	var p struct {
		HeadSHA string `json:"head_sha"`
		Backlog string `json:"backlog"`
	}
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.HeadSHA != sha || p.Backlog != backlog {
		t.Fatalf("payload mismatch: %+v", p)
	}

	// Within the interval the snapshot must not repeat.
	rep.reports = nil
	col.collectAll(context.Background())
	for _, r := range rep.reports {
		for _, e := range r.Events {
			if e.EventKind == "snapshot" {
				t.Fatal("snapshot re-sent within the throttle interval")
			}
		}
	}

	// After the interval elapses it is sent again.
	col.lastSnapshot[root] = time.Now().Add(-6 * time.Minute)
	rep.reports = nil
	col.collectAll(context.Background())
	found := false
	for _, r := range rep.reports {
		for _, e := range r.Events {
			if e.EventKind == "snapshot" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("snapshot not re-sent after the interval")
	}
}

func TestSnapshotSkippedWithoutBacklog(t *testing.T) {
	root := t.TempDir()
	gitInitWithCRCommit(t, root, "wip: seed")
	rep := &fakeReporter{ack: acceptAll}
	col := newTestCollector(root, rep)
	col.collectAll(context.Background())
	for _, r := range rep.reports {
		for _, e := range r.Events {
			if e.EventKind == "snapshot" {
				t.Fatal("root without _backlog.yml must not emit snapshots")
			}
		}
	}
}

func TestSnapshotReportFailureRetriesNextTick(t *testing.T) {
	root := t.TempDir()
	gitInitWithCRCommit(t, root, "wip: seed")
	if err := os.MkdirAll(filepath.Join(root, "change-requests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "change-requests", "_backlog.yml"),
		[]byte("change-requests: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &fakeReporter{ack: acceptAll, err: context.DeadlineExceeded}
	col := newTestCollector(root, rep)
	col.collectAll(context.Background())
	if !col.lastSnapshot[root].IsZero() {
		t.Fatal("failed report must not advance lastSnapshot")
	}
	rep.err = nil
	col.collectAll(context.Background())
	if col.lastSnapshot[root].IsZero() {
		t.Fatal("successful report must advance lastSnapshot")
	}
}

// CR-2026-003 FR-2: the daemon snapshot ships _history.yml so archived CRs can
// heal server-side; absence of the file degrades to an empty field.
func TestSnapshotCarriesHistory(t *testing.T) {
	root := t.TempDir()
	gitInitWithCRCommit(t, root, "wip: seed")
	if err := os.MkdirAll(filepath.Join(root, "change-requests"), 0o755); err != nil {
		t.Fatal(err)
	}
	backlog := "change-requests: []\n"
	history := "history:\n  - id: CR-2026-001\n    final-status: archived\n"
	if err := os.WriteFile(filepath.Join(root, "change-requests", "_backlog.yml"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "change-requests", "_history.yml"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, ok := buildSnapshotEvent(root)
	if !ok {
		t.Fatal("snapshot expected")
	}
	var p struct {
		Backlog string `json:"backlog"`
		History string `json:"history"`
	}
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Backlog != backlog || p.History != history {
		t.Fatalf("payload mismatch: %+v", p)
	}
	// No history file: field degrades to empty, snapshot still emitted.
	if err := os.Remove(filepath.Join(root, "change-requests", "_history.yml")); err != nil {
		t.Fatal(err)
	}
	snap, ok = buildSnapshotEvent(root)
	if !ok {
		t.Fatal("snapshot expected without history file")
	}
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.History != "" {
		t.Fatalf("missing history file must yield empty field, got %q", p.History)
	}
}
