package governance

// AIFIRST: server-mode reconcile live test against the real GitHub origin
// (CR-2026-002 TASK-07, AC-3②). Runs only when KNOWLEDGE_BASE_REMOTE_URL and
// KNOWLEDGE_BASE_TOKEN are set (read-only single-repo PAT); skips everywhere
// else, so CI without credentials is unaffected. Read-only against GitHub;
// writes only to the local test workspace's cr rows.

import (
	"context"
	"os"
	"testing"

	"github.com/multica-ai/multica/server/internal/scheduler"
)

func TestReconcileLiveGitHub(t *testing.T) {
	remote := os.Getenv("KNOWLEDGE_BASE_REMOTE_URL")
	token := os.Getenv("KNOWLEDGE_BASE_TOKEN")
	if remote == "" || token == "" {
		t.Skip("KNOWLEDGE_BASE_REMOTE_URL / KNOWLEDGE_BASE_TOKEN not set")
	}
	cfg := ReconcileConfig{Mode: "server", RemoteURL: remote, Token: token}

	snap, err := FetchGitHubSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fetch from GitHub origin: %v", err)
	}
	if snap.HeadSHA == "" || len(snap.Statuses) == 0 {
		t.Fatalf("empty snapshot from origin: %+v", snap)
	}
	t.Logf("origin HEAD %.12s, %d CRs in backlog", snap.HeadSHA, len(snap.Statuses))

	// Pick a real CR from the authoritative backlog and tamper its projection
	// (AC-3①: manual UPDATE, then the job heals it).
	var crID, authStatus string
	for id, st := range snap.Statuses {
		if KnownStatuses[st] {
			crID, authStatus = id, st
			break
		}
	}
	if crID == "" {
		t.Fatal("no CR with a known status in the origin backlog")
	}
	resetCR(t, crID)
	t.Cleanup(func() { resetCR(t, crID) })
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO cr (workspace_id, cr_id, status, projected_commit, needs_reconcile)
		VALUES ($1::uuid, $2, 'drafting', 'tampered-sha', TRUE)`,
		testWorkspaceID, crID); err != nil {
		t.Fatal(err)
	}

	// Drive the real job handler (the same code the sys_cron scheduler runs).
	svc := NewSyncService(testPool, nil)
	job := ReconcileJob(testPool, svc, cfg)
	res, err := job.Handler(context.Background(), scheduler.HandlerInput{})
	if err != nil {
		t.Fatalf("reconcile job: %v", err)
	}
	if res.RowsAffected < 1 {
		t.Fatalf("expected at least the tampered row healed, got %+v", res)
	}
	st, nr, pc := crRow(t, crID)
	if st != authStatus || nr || pc != snap.HeadSHA {
		t.Fatalf("row not healed to authority: status=%s needs_reconcile=%v projected=%s (want %s/false/%s)",
			st, nr, pc, authStatus, snap.HeadSHA)
	}
	t.Logf("healed %s: drafting→%s, projected_commit=%.12s (AC-3①② server mode)", crID, st, pc)
}
