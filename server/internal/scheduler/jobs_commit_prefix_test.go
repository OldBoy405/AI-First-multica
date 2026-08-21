package scheduler

// AIFIRST: CR-2026-049 TASK-10 — commit_prefix_scan job tests (SDD §3.2/§3.4).
// JobSpec fields are locked against the SDD; the handler integration runs
// against live Postgres (skip when unreachable): first round builds the
// three-repo baseline with zero findings, a follow-up round with a new wip:
// commit classifies only the new window and upserts a deduped row, and a
// source failure fails the plan without a new cursor.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/commitprefix"
	"github.com/multica-ai/multica/server/internal/drift"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCommitPrefixScanJobSpecLocksSDDFields(t *testing.T) {
	job := CommitPrefixScanJob(CommitPrefixScanDeps{})
	eq := func(name string, got, want any) {
		t.Helper()
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("Name", job.Name, JobNameCommitPrefixScan)
	eq("Cadence", job.Cadence, time.Hour)
	eq("CatchUpMode", job.CatchUpMode, CatchUpLatestOnly)
	eq("RunTimeout", job.RunTimeout, 10*time.Minute)
	eq("StaleTimeout", job.StaleTimeout, 15*time.Minute)
	eq("HeartbeatInterval", job.HeartbeatInterval, 30*time.Second)
	eq("AllowStaleReentry", job.AllowStaleReentry, true)
	eq("MaxAttempts", job.MaxAttempts, 3)
	eq("RetryBackoff", job.RetryBackoff, []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute})
}

const testDBURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"

func openScanPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL)
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("db not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fakeAccess struct {
	tokens map[int64]string
}

func (f *fakeAccess) ResolveRepositoryAccess(_ context.Context, ids []int64, _, _ string) (int64, error) {
	if len(ids) == 1 {
		return ids[0], nil
	}
	return 0, errors.New("repository_access_ambiguous")
}

func (f *fakeAccess) InstallationToken(_ context.Context, id int64) (string, error) {
	return f.tokens[id], nil
}

// scanFake implements ghsnapshot.CommitSource, dispatching by GitHub repo name.
type scanFake struct {
	commits map[string][]ghsnapshot.CommitMeta // repoName -> new..old
	fail    map[string]bool
}

func (s *scanFake) Head(_ context.Context, _, _, repo, _ string) (string, error) {
	if s.fail[repo] {
		return "", errors.New("rate limited")
	}
	c, ok := s.commits[repo]
	if !ok || len(c) == 0 {
		return "", errors.New("no commits")
	}
	return c[0].SHA, nil
}

func (s *scanFake) Page(_ context.Context, _, _, repo, _, _ string, page, perPage int) ([]ghsnapshot.CommitMeta, error) {
	if s.fail[repo] {
		return nil, errors.New("rate limited")
	}
	commits := s.commits[repo]
	start := (page - 1) * perPage
	if start >= len(commits) {
		return nil, nil
	}
	end := start + perPage
	if end > len(commits) {
		end = len(commits)
	}
	return commits[start:end], nil
}

func TestCommitPrefixScanHandlerBaselineAndDedup(t *testing.T) {
	pool := openScanPool(t)
	ctx := context.Background()

	var wsID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, repos) VALUES ('scan-tests', 'scan-tests', $1::jsonb)
		ON CONFLICT (slug) DO UPDATE SET repos = EXCLUDED.repos, updated_at = now()
		RETURNING id::text`,
		fmt.Sprintf(`[{"url":%q},{"url":"https://github.com/OldBoy405/AI-First-multica.git"},{"url":"https://github.com/OldBoy405/AI-First-tools.git"}]`,
			commitprefix.GeneratedPrefixes()["ai-first-platform-docs"].CanonicalURL)).Scan(&wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM drift_finding WHERE workspace_id = $1::uuid`, wsID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_installation WHERE workspace_id = $1::uuid`, wsID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sys_cron_executions WHERE scope_id = $1 AND job_name = 'commit_prefix_scan'`, wsID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1::uuid`, wsID)
	})
	queries := db.New(pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type)
		VALUES ($1::uuid, 777, 'OldBoy405', 'User')
		ON CONFLICT (workspace_id, installation_id) DO NOTHING`, wsID); err != nil {
		t.Fatalf("seed installation: %v", err)
	}

	parent := &scanFake{
		commits: map[string][]ghsnapshot.CommitMeta{
			"AI-First-Platform": {{SHA: "kb-b", Subject: "[cr] status"}},
			"AI-First-multica":  {{SHA: "mu-b", Subject: "feat(m) add"}},
			"AI-First-tools":    {{SHA: "to-b", Subject: "chore(t) bump"}},
		},
		fail: map[string]bool{},
	}
	deps := CommitPrefixScanDeps{
		Pool: pool, Queries: queries,
		Resolver: drift.NewResolver(&fakeAccess{tokens: map[int64]string{777: "tok"}}),
		GH:       parent,
		Findings: drift.NewFindingRepo(pool),
	}
	job := CommitPrefixScanJob(deps)
	run := func() HandlerResult {
		t.Helper()
		res, err := job.Handler(ctx, HandlerInput{Job: job, Scope: Scope{Kind: "workspace", ID: wsID}, Heartbeat: func(context.Context) error { return nil }})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		return res
	}
	seedPrev := func(rv drift.ScanResultV1) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM sys_cron_executions WHERE job_name='commit_prefix_scan' AND scope_id=$1`, wsID); err != nil {
			t.Fatalf("clear prev result: %v", err)
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO sys_cron_executions (job_name, scope_kind, scope_id, plan_time, status, result, started_at)
			VALUES ('commit_prefix_scan', 'workspace', $1, now(), 'SUCCESS', $2::jsonb, now())`,
			wsID, drift.EncodeResultV1(rv.ConfigRev, rv.RepositoryIDs, rv.ScanCursors, rv.FindingCount))
		if err != nil {
			t.Fatalf("seed prev result: %v", err)
		}
	}

	// First round: baseline only, zero findings, three cursors.
	rv, ok := drift.DecodeResultV1(run().Result)
	if !ok {
		t.Fatalf("result v1 decode failed")
	}
	if len(rv.RepositoryIDs) != 3 || rv.FindingCount != 0 {
		t.Fatalf("baseline result = %+v", rv)
	}
	for _, id := range []string{"ai-first-platform-docs", "multica", "tools"} {
		if rv.ScanCursors[id] == "" {
			t.Errorf("cursor missing for %s", id)
		}
	}
	if rv.ConfigRev != commitprefix.GeneratedConfigRev() {
		t.Errorf("config_rev = %s", rv.ConfigRev)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM drift_finding WHERE workspace_id = $1::uuid`, wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("baseline findings = %d, want 0", n)
	}
	seedPrev(rv)

	// Second round: tools trunk gains a wip: commit on top of the cursor.
	parent.commits["AI-First-tools"] = []ghsnapshot.CommitMeta{
		{SHA: "to-c", Subject: "wip: half-done"},
		{SHA: "to-b", Subject: "chore(t) bump"},
	}
	rv2, _ := drift.DecodeResultV1(run().Result)
	if rv2.FindingCount != 1 {
		t.Fatalf("round2 finding_count = %d, want 1 (%+v)", rv2.FindingCount, rv2)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM drift_finding WHERE workspace_id = $1::uuid`, wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("findings after round2 = %d, want 1", n)
	}
	seedPrev(rv2)

	// Third round over the same window: dedup keeps exactly one row.
	run()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM drift_finding WHERE workspace_id = $1::uuid`, wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("findings after dedup round = %d, want 1", n)
	}

	// Source failure: plan fails and no new cursor is written.
	parent.fail["AI-First-tools"] = true
	if _, err := job.Handler(ctx, HandlerInput{Job: job, Scope: Scope{Kind: "workspace", ID: wsID}, Heartbeat: func(context.Context) error { return nil }}); err == nil {
		t.Fatal("failing source must fail the plan")
	}
	parent.fail["AI-First-tools"] = false
}
