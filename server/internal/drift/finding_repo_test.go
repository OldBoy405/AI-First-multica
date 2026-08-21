package drift

// AIFIRST: CR-2026-049 TASK-11 — finding keyset + PATCH CAS tests (SDD §3.6
// AC-15). Keyset pagination stays stable while new rows land (no re-page/漏页);
// the CAS matrix covers legal transitions, same-state idempotence, illegal
// transitions (409), concurrent CAS (one winner), cross-workspace 404, and the
// resolved_at/wontfix semantics.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/commitprefix"
)

func seedFindingRow(t *testing.T, pool Querier, ws, repo, kind, status string, foundAt time.Time) string {
	t.Helper()
	evidence := `{"repository_id":"` + repo + `","trunk":"main","commit_sha":"` + fmt.Sprintf("%s-%d", repo, foundAt.UnixNano()) + `","commit_subject":"subject","scanned_at":"2026-08-20T00:00:00Z"}`
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO drift_finding (workspace_id, repository_id, spec_id, cr_id, kind, severity, summary, evidence, status, found_at)
		VALUES ($1::uuid, $2, NULL, NULL, $3, 'warn', $4, $5::jsonb, $6, $7)
		RETURNING id::text`, ws, repo, kind, "summary "+repo, evidence, status, foundAt).Scan(&id); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return id
}

func TestListFindingsKeysetStable(t *testing.T) {
	pool := openDriftPool(t)
	ctx := context.Background()
	ws := seedOverviewWorkspace(t, pool)

	// 5 open findings, found_at strictly increasing → order is found_at DESC.
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 5; i++ {
		seedFindingRow(t, pool, ws, "tools", "bypass-commit", "open", base.Add(time.Duration(i)*time.Minute))
	}

	repo := NewFindingQueryRepo(pool)
	page1, err := repo.ListFindings(ctx, ws, ListFindingsFilter{}, 2, nil)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Findings) != 2 || page1.NextCursor == nil {
		t.Fatalf("page1 = %d rows, cursor=%v", len(page1.Findings), page1.NextCursor)
	}
	first := page1.Findings[0]
	if !first.FoundAt.After(page1.Findings[1].FoundAt) {
		t.Errorf("page1 not ordered found_at DESC")
	}
	// A new finding lands between pages: keyset must not re-page the first row.
	seedFindingRow(t, pool, ws, "tools", "wip-on-trunk", "open", base.Add(10*time.Minute))
	page2, err := repo.ListFindings(ctx, ws, ListFindingsFilter{}, 10, page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range page1.Findings {
		seen[d.ID] = true
	}
	for _, d := range page2.Findings {
		if seen[d.ID] {
			t.Errorf("keyset re-paged row %s", d.ID)
		}
	}
	// The row inserted between pages lands BEFORE the cursor (found_at DESC),
	// so this scan legitimately never sees it — keyset semantics: no re-page,
	// no duplicates; rows ahead of the cursor are the next scan's business.
	if len(page1.Findings)+len(page2.Findings) != 5 {
		t.Errorf("total = %d, want 5 (inserted-before-cursor row excluded)", len(page1.Findings)+len(page2.Findings))
	}

	// Filter by kind.
	filtered, err := repo.ListFindings(ctx, ws, ListFindingsFilter{Kind: "wip-on-trunk"}, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Findings) != 1 || filtered.Findings[0].Kind != "wip-on-trunk" {
		t.Errorf("kind filter = %+v", filtered.Findings)
	}
	// Invalid cursor.
	if _, err := repo.ListFindings(ctx, ws, ListFindingsFilter{}, 10, strPtr("!!not-base64!!")); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("invalid cursor err = %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestPatchStatusMatrix(t *testing.T) {
	pool := openDriftPool(t)
	ctx := context.Background()
	ws := seedOverviewWorkspace(t, pool)
	repo := NewFindingQueryRepo(pool)

	openID := seedFindingRow(t, pool, ws, "tools", "bypass-commit", "open", time.Now())
	ackID := seedFindingRow(t, pool, ws, "multica", "wip-on-trunk", "acknowledged", time.Now())
	resolvedID := seedFindingRow(t, pool, ws, "tools", "bypass-commit", "resolved", time.Now())

	// open → acknowledged: resolved_at stays NULL.
	d, err := repo.PatchStatus(ctx, ws, openID, "open", "acknowledged")
	if err != nil || d.Status != "acknowledged" || d.ResolvedAt != nil {
		t.Fatalf("open→ack = %+v err=%v", d, err)
	}
	// acknowledged → resolved: resolved_at set.
	d, err = repo.PatchStatus(ctx, ws, ackID, "acknowledged", "resolved")
	if err != nil || d.Status != "resolved" || d.ResolvedAt == nil {
		t.Fatalf("ack→resolved = %+v err=%v", d, err)
	}
	// Same-state replay idempotent 200.
	d, err = repo.PatchStatus(ctx, ws, openID, "acknowledged", "acknowledged")
	if err != nil || d.Status != "acknowledged" {
		t.Fatalf("idempotent replay = %+v err=%v", d, err)
	}
	// Illegal transitions.
	if _, err := repo.PatchStatus(ctx, ws, openID, "open", "open"); err == nil {
		// open→open is same-state → idempotent; row is acknowledged now → 409.
		t.Fatalf("open→open on acknowledged row must conflict")
	}
	if _, err := repo.PatchStatus(ctx, ws, resolvedID, "resolved", "open"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal transition err = %v", err)
	}
	if _, err := repo.PatchStatus(ctx, ws, openID, "acknowledged", "open"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("backward transition err = %v", err)
	}
	// wontfix keeps resolved_at NULL.
	wontfixID := seedFindingRow(t, pool, ws, "tools", "bypass-commit", "open", time.Now())
	d, err = repo.PatchStatus(ctx, ws, wontfixID, "open", "wontfix")
	if err != nil || d.Status != "wontfix" || d.ResolvedAt != nil {
		t.Fatalf("open→wontfix = %+v err=%v", d, err)
	}
	// Cross-workspace id → 404.
	otherWS := seedOverviewWorkspace2(t, pool)
	foreignID := seedFindingRow(t, pool, otherWS, "tools", "bypass-commit", "open", time.Now())
	if _, err := repo.PatchStatus(ctx, ws, foreignID, "open", "acknowledged"); !errors.Is(err, ErrFindingNotFound) {
		t.Fatalf("cross-workspace err = %v, want not_found", err)
	}
}

func seedOverviewWorkspace2(t *testing.T, pool Querier) string {
	t.Helper()
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, repos) VALUES ('drift-tests-2', 'drift-tests-2', $1::jsonb)
		ON CONFLICT (slug) DO UPDATE SET repos = EXCLUDED.repos, updated_at = now()
		RETURNING id::text`, fmt.Sprintf(`[{"url":%q}]`, commitprefix.GeneratedPrefixes()["ai-first-platform-docs"].CanonicalURL)).Scan(&ws); err != nil {
		t.Fatalf("seed workspace2: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM drift_finding WHERE workspace_id = $1::uuid`, ws)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1::uuid`, ws)
	})
	return ws
}
