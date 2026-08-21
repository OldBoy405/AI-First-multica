package service

// AIFIRST: CR-2026-049 TASK-10 — ScanRepo / Classify tests (SDD §3.4 AC-9/AC-12).
// Fake CommitSource pages a fixed 100+ commit chain: multi-page walk only
// classifies [B..A), a mid-scan HEAD advance never enters the round, missing
// cursors/rate limits/truncation fail without cursor advance, every page
// heartbeats, and the first scan builds a zero-finding baseline.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/drift"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

type fakeSource struct {
	commits  []ghsnapshot.CommitMeta // new -> old
	headAt   string
	failPage int // 1-based page that errors (429); 0 = none
	pages    *int
}

func (f *fakeSource) Head(_ context.Context, _, _, _, _ string) (string, error) {
	return f.commits[0].SHA, nil
}

func (f *fakeSource) Page(_ context.Context, _, _, _, _, _ string, page, perPage int) ([]ghsnapshot.CommitMeta, error) {
	*f.pages++
	if f.failPage != 0 && page == f.failPage {
		return nil, errors.New("rate limited")
	}
	start := (page - 1) * perPage
	if start >= len(f.commits) {
		return nil, nil
	}
	end := start + perPage
	if end > len(f.commits) {
		end = len(f.commits)
	}
	return f.commits[start:end], nil
}

func chain(n int, prefix func(i int) string) []ghsnapshot.CommitMeta {
	out := make([]ghsnapshot.CommitMeta, n)
	for i := 0; i < n; i++ {
		out[i] = ghsnapshot.CommitMeta{SHA: fmt.Sprintf("sha-%03d", n-1-i), Subject: prefix(n - 1 - i)}
	}
	return out
}

var prefixes = []string{"feat(", "fix(", "[cr] "}

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		subject  string
		kind     string
		severity string
		finding  bool
	}{
		{"wip: half-done work", "wip-on-trunk", "info", true},
		{"feat(x): add thing", "", "", false},
		{"fix(y): repair", "", "", false},
		{"[cr] status advance", "", "", false},
		{"Random subject", "bypass-commit", "warn", true},
		{"feature(x): not feat(", "bypass-commit", "warn", true}, // case-sensitive + delimiter
	}
	for _, c := range cases {
		kind, severity, finding := Classify(c.subject, prefixes)
		if kind != c.kind || severity != c.severity || finding != c.finding {
			t.Errorf("Classify(%q) = (%q,%q,%v), want (%q,%q,%v)", c.subject, kind, severity, finding, c.kind, c.severity, c.finding)
		}
	}
}

func TestScanRepoFirstRoundBaselineOnly(t *testing.T) {
	pages := 0
	src := &fakeSource{commits: chain(120, func(i int) string { return "feat(ok)" }), pages: &pages}
	beats := 0
	res, err := ScanRepo(context.Background(), ScanRepoInput{
		Bound: drift.BoundRepo{RepoID: "tools", Owner: "o", Repo: "r", Trunk: "custom/main", Prefixes: prefixes, Token: "tok"},
		Source: src,
		Heartbeat: func(context.Context) error {
			beats++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if res.Cursor != "sha-119" || len(res.Findings) != 0 {
		t.Fatalf("baseline = %+v", res)
	}
	if pages != 0 || beats != 0 {
		t.Errorf("pages=%d beats=%d, want 0/0 (HEAD lookup only, no paging, no heartbeat)", pages, beats)
	}
}

func TestScanRepoMultiPageClassifiesOnlyInclusiveRange(t *testing.T) {
	// 120 commits; A = sha-010 (10th newest); walk classifies [B..A): 10 commits.
	subjects := func(i int) string {
		switch {
		case i == 118:
			return "wip: top"
		case i == 115:
			return "Random bypass"
		default:
			return "feat(ok)"
		}
	}
	pages := 0
	src := &fakeSource{commits: chain(120, subjects), pages: &pages}
	beats := 0
	a := "sha-010"
	res, err := ScanRepo(context.Background(), ScanRepoInput{
		Bound: drift.BoundRepo{RepoID: "tools", Owner: "o", Repo: "r", Trunk: "main", Prefixes: prefixes, Token: "tok"},
		Source:     src,
		PrevCursor: &a,
		Heartbeat: func(context.Context) error {
			beats++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Cursor != "sha-119" {
		t.Errorf("cursor = %s, want sha-119", res.Cursor)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(res.Findings), res.Findings)
	}
	// (A,B] only: the wip at 118 and bypass at 115; nothing older than A.
	for _, f := range res.Findings {
		ev := string(f.Evidence)
		if !strings.Contains(ev, "sha-118") && !strings.Contains(ev, "sha-115") {
			t.Errorf("finding outside [B..A): %s", ev)
		}
		if f.RepositoryID != "tools" || f.Kind == "" {
			t.Errorf("finding shape: %+v", f)
		}
	}
	// 2 pages: page1=100 commits (0..99), page2 hits A at index 9 → wait:
	// commits new->old; A=sha-010 is at position 109 in the slice → page1 covers 0..99, page2 covers 100..119 (20 commits). A at 109 found in page2.
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
	if beats != 2 {
		t.Errorf("heartbeats = %d, want one per page", beats)
	}
}

func TestScanRepoMissingCursorFails(t *testing.T) {
	pages := 0
	src := &fakeSource{commits: chain(120, func(i int) string { return "feat(ok)" }), pages: &pages}
	a := "sha-not-in-history"
	_, err := ScanRepo(context.Background(), ScanRepoInput{
		Bound: drift.BoundRepo{RepoID: "tools", Owner: "o", Repo: "r", Trunk: "main", Prefixes: prefixes, Token: "tok"},
		Source: src, PrevCursor: &a,
	})
	if err == nil || !errors.Is(err, ErrCursorUnreachable) {
		t.Fatalf("err = %v, want cursor_unreachable", err)
	}
}

func TestScanRepoRateLimitFailsWithoutCursorAdvance(t *testing.T) {
	pages := 0
	src := &fakeSource{commits: chain(120, func(i int) string { return "feat(ok)" }), pages: &pages, failPage: 2}
	a := "sha-000"
	_, err := ScanRepo(context.Background(), ScanRepoInput{
		Bound: drift.BoundRepo{RepoID: "tools", Owner: "o", Repo: "r", Trunk: "main", Prefixes: prefixes, Token: "tok"},
		Source: src, PrevCursor: &a,
	})
	if err == nil || !errors.Is(err, ErrScanTruncated) {
		t.Fatalf("err = %v, want scan_truncated", err)
	}
}

func TestScanRepoPageBudgetExhausted(t *testing.T) {
	pages := 0
	never := &neverSource{pages: &pages}
	a := "sha-never"
	_, err := ScanRepo(context.Background(), ScanRepoInput{
		Bound: drift.BoundRepo{RepoID: "tools", Owner: "o", Repo: "r", Trunk: "main", Prefixes: prefixes, Token: "tok"},
		Source: never, PrevCursor: &a,
	})
	if err == nil || !errors.Is(err, ErrCursorUnreachable) {
		t.Fatalf("err = %v, want cursor_unreachable", err)
	}
	if pages > maxPages {
		t.Errorf("pages = %d beyond budget", pages)
	}
}

// neverSource returns one non-matching commit per page forever — the cursor is
// unreachable within the page budget.
type neverSource struct {
	pages *int
}

func (n *neverSource) Head(_ context.Context, _, _, _, _ string) (string, error) { return "sha-x", nil }

func (n *neverSource) Page(_ context.Context, _, _, _, _, _ string, page, _ int) ([]ghsnapshot.CommitMeta, error) {
	*n.pages++
	return []ghsnapshot.CommitMeta{{SHA: fmt.Sprintf("sha-p%d", page), Subject: "feat"}}, nil
}
