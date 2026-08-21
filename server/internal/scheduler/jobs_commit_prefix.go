package scheduler

// AIFIRST: CR-2026-049 TASK-10 — commit_prefix_scan job (SDD §3.2, TD-B6).
// One scope per eligible workspace (workspace.repos contains the canonical
// knowledge-base URL); the handler resolves per-workspace bindings, scans each
// bound repo from the fixed HEAD B down to the exact previous cursor A, upserts
// findings idempotently, and only on full-repo success returns a success result
// carrying the new cursors — the scheduler writes them atomically with the
// terminal status (SDD §4.1: no parallel scan_state table).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/commitprefix"
	"github.com/multica-ai/multica/server/internal/drift"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameCommitPrefixScan is the stable audit/index key for the scan job.
const JobNameCommitPrefixScan = "commit_prefix_scan"

// CommitPrefixScanDeps wires the job to its data surfaces.
type CommitPrefixScanDeps struct {
	Pool     *pgxpool.Pool
	Queries  *db.Queries
	Resolver drift.RepositoryBindingResolver
	GH       ghsnapshot.CommitSource
	Findings *drift.FindingRepo
}

// CommitPrefixScanJob builds the JobSpec (SDD §3.2 field-locked).
func CommitPrefixScanJob(deps CommitPrefixScanDeps) *JobSpec {
	return &JobSpec{
		Name:              JobNameCommitPrefixScan,
		Cadence:           time.Hour,
		ScheduleDelay:     0,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     2 * time.Hour,
		MaxPlansPerTick:   1,
		RunTimeout:        10 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            activePlatformWorkspaceScopes(deps.Pool),
		Handler:           commitPrefixScanHandler(deps),
	}
}

// activePlatformWorkspaceScopes enumerates workspaces whose workspace.repos
// contains the canonical knowledge-base URL (SDD §3.2: eligible only). Workspaces
// without the platform repo produce no plan (governance shows not_configured),
// never a failing plan.
func activePlatformWorkspaceScopes(pool *pgxpool.Pool) ScopeProvider {
	kbDecl, _ := commitprefix.GeneratedPrefixes()["ai-first-platform-docs"]
	return func(ctx context.Context, _ time.Time) ([]Scope, error) {
		rows, err := pool.Query(ctx, `SELECT id::text, repos FROM workspace`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var scopes []Scope
		for rows.Next() {
			var id string
			var reposRaw []byte
			if err := rows.Scan(&id, &reposRaw); err != nil {
				return nil, err
			}
			var repos []struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(reposRaw, &repos) != nil {
				continue // malformed repos: not eligible, never a failing plan
			}
			for _, r := range repos {
				if canonicalKBURL(r.URL) == kbDecl.CanonicalURL {
					scopes = append(scopes, Scope{Kind: "workspace", ID: id})
					break
				}
			}
		}
		return scopes, rows.Err()
	}
}

// canonicalKBURL normalizes an http(s)/ssh GitHub URL to the canonical HTTPS
// form so workspace.repos entries can be compared against the declaration.
func canonicalKBURL(raw string) string {
	u := raw
	u = trimSuffix(u, "/")
	u = trimSuffix(u, ".git")
	if len(u) >= len("git@github.com:") && u[:len("git@github.com:")] == "git@github.com:" {
		return "https://github.com/" + u[len("git@github.com:"):] + ".git"
	}
	if len(u) >= len("https://github.com/") && u[:len("https://github.com/")] == "https://github.com/" {
		return u + ".git"
	}
	return u
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func commitPrefixScanHandler(deps CommitPrefixScanDeps) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		workspaceID := in.Scope.ID
		decls := commitprefix.GeneratedPrefixes()

		// Workspace repos + GitHub installations (per-workspace, never static).
		var reposRaw []byte
		if err := deps.Pool.QueryRow(ctx, `SELECT repos FROM workspace WHERE id = $1::uuid`, workspaceID).Scan(&reposRaw); err != nil {
			return HandlerResult{}, fmt.Errorf("load workspace repos: %w", err)
		}
		var wsRepos []struct {
			URL         string `json:"url"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(reposRaw, &wsRepos); err != nil {
			return HandlerResult{}, fmt.Errorf("parse workspace repos: %w", err)
		}
		repoData := make([]drift.RepoData, 0, len(wsRepos))
		for _, r := range wsRepos {
			repoData = append(repoData, drift.RepoData{URL: r.URL, Description: r.Description})
		}
		installRows, err := deps.Queries.ListGitHubInstallationsByWorkspace(ctx, util.MustParseUUID(workspaceID))
		if err != nil {
			return HandlerResult{}, fmt.Errorf("load installations: %w", err)
		}
		installations := make([]drift.GitHubInstallation, 0, len(installRows))
		for _, row := range installRows {
			installations = append(installations, drift.GitHubInstallation{ID: row.ID.String(), InstallationID: row.InstallationID})
		}

		bound, err := deps.Resolver.ResolveBindings(ctx, workspaceID, decls, repoData, installations)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("resolve bindings: %w", err)
		}

		// Previous successful cursors from the scheduler audit trail.
		prevCursors := map[string]string{}
		var prevResult map[string]any
		err = deps.Pool.QueryRow(ctx, `
			SELECT result FROM sys_cron_executions
			WHERE job_name = $1 AND scope_kind = 'workspace' AND scope_id = $2 AND status = 'SUCCESS'
			ORDER BY started_at DESC LIMIT 1`, JobNameCommitPrefixScan, workspaceID).Scan(&prevResult)
		if err == nil && prevResult != nil {
			if rv, ok := drift.DecodeResultV1(prevResult); ok {
				prevCursors = rv.ScanCursors
			}
		}

		// Scan every bound repo; any failure fails the whole plan (cursor stays).
		repoIDs := make([]string, 0, len(bound))
		cursors := map[string]string{}
		var allFindings []drift.FindingInput
		for _, b := range bound {
			var prev *string
			if v, ok := prevCursors[b.RepoID]; ok {
				p := v
				prev = &p
			}
			res, err := service.ScanRepo(ctx, service.ScanRepoInput{
				Bound:      b,
				PrevCursor: prev,
				Heartbeat:  in.Heartbeat,
				Source:     deps.GH,
			})
			if err != nil {
				return HandlerResult{}, fmt.Errorf("scan %s: %w", b.RepoID, err)
			}
			repoIDs = append(repoIDs, b.RepoID)
			cursors[b.RepoID] = res.Cursor
			for i := range res.Findings {
				res.Findings[i].WorkspaceID = workspaceID
			}
			allFindings = append(allFindings, res.Findings...)
		}

		inserted, err := deps.Findings.UpsertFindings(ctx, allFindings)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("upsert findings: %w", err)
		}
		return HandlerResult{
			RowsAffected: inserted,
			Result:       drift.EncodeResultV1(commitprefix.GeneratedConfigRev(), repoIDs, cursors, inserted),
		}, nil
	}
}
