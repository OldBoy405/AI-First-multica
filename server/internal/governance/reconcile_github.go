// AIFIRST: server-mode reconcile — GitHub authority fetcher + sys_cron job
// (CR-2026-002 TASK-07, AC-3②).
//
// Credentials discipline: the PAT is expected to be fine-grained, single-repo,
// Contents: Read-only. It lives in memory only and is never logged; every
// error path reports status codes and URLs, never headers.
package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/scheduler"
)

// ReconcileConfig comes from the environment (FromReconcileEnv).
type ReconcileConfig struct {
	Mode      string // "server" enables the polling job; "daemon"/"" rely on daemon snapshots
	RemoteURL string // https://github.com/{owner}/{repo}[.git]
	Token     string // read-only PAT; never logged
	Interval  time.Duration
}

// FromReconcileEnv reads REMOTE_RECONCILE_MODE / KNOWLEDGE_BASE_REMOTE_URL /
// KNOWLEDGE_BASE_TOKEN / RECONCILE_INTERVAL. Returns ok=false when server-mode
// polling should not be mounted (unset or daemon mode); an error only for a
// server-mode config that is unusable (fail loudly at startup, not at 3am).
func FromReconcileEnv() (ReconcileConfig, bool, error) {
	cfg := ReconcileConfig{
		Mode:      strings.TrimSpace(os.Getenv("REMOTE_RECONCILE_MODE")),
		RemoteURL: strings.TrimSpace(os.Getenv("KNOWLEDGE_BASE_REMOTE_URL")),
		Token:     strings.TrimSpace(os.Getenv("KNOWLEDGE_BASE_TOKEN")),
		Interval:  5 * time.Minute,
	}
	if raw := strings.TrimSpace(os.Getenv("RECONCILE_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Minute {
			return cfg, false, fmt.Errorf("RECONCILE_INTERVAL invalid (want a duration >= 1m): %q", raw)
		}
		cfg.Interval = d
	}
	switch cfg.Mode {
	case "server":
		if _, _, err := parseGitHubRemote(cfg.RemoteURL); err != nil {
			return cfg, false, err
		}
		return cfg, true, nil
	case "", "daemon":
		return cfg, false, nil
	default:
		return cfg, false, fmt.Errorf("REMOTE_RECONCILE_MODE must be server or daemon, got %q", cfg.Mode)
	}
}

var githubRemoteRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(\.git)?/?$`)

func parseGitHubRemote(url string) (owner, repo string, err error) {
	m := githubRemoteRe.FindStringSubmatch(url)
	if m == nil {
		return "", "", fmt.Errorf("KNOWLEDGE_BASE_REMOTE_URL must be an https://github.com/{owner}/{repo} URL (server-mode reconcile speaks the GitHub API; use daemon mode for other remotes), got %q", url)
	}
	return m[1], m[2], nil
}

// FetchGitHubSnapshot reads the authority from GitHub: default-branch HEAD sha
// plus the raw _backlog.yml at exactly that sha (no torn read between the two).
func FetchGitHubSnapshot(ctx context.Context, cfg ReconcileConfig) (AuthoritySnapshot, error) {
	owner, repo, err := parseGitHubRemote(cfg.RemoteURL)
	if err != nil {
		return AuthoritySnapshot{}, err
	}
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)

	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := githubGet(ctx, cfg.Token, base, "application/vnd.github+json", &meta, nil); err != nil {
		return AuthoritySnapshot{}, fmt.Errorf("repo metadata: %w", err)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := githubGet(ctx, cfg.Token, base+"/commits/"+meta.DefaultBranch, "application/vnd.github+json", &commit, nil); err != nil {
		return AuthoritySnapshot{}, fmt.Errorf("HEAD of %s: %w", meta.DefaultBranch, err)
	}
	var raw []byte
	if err := githubGet(ctx, cfg.Token, base+"/contents/change-requests/_backlog.yml?ref="+commit.SHA, "application/vnd.github.raw+json", nil, &raw); err != nil {
		return AuthoritySnapshot{}, fmt.Errorf("_backlog.yml@%.8s: %w", commit.SHA, err)
	}
	statuses, err := ParseBacklog(raw)
	if err != nil {
		return AuthoritySnapshot{}, err
	}
	return AuthoritySnapshot{HeadSHA: commit.SHA, Statuses: statuses}, nil
}

func githubGet(ctx context.Context, token, url, accept string, jsonOut any, rawOut *[]byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// Body may echo request details; report the status line only.
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	if rawOut != nil {
		*rawOut = body
		return nil
	}
	return json.Unmarshal(body, jsonOut)
}

// ReconcileJob is the sys_cron job for server mode. One global scope: the
// single-org deployment has one knowledge base; every workspace holding cr
// rows is reconciled against it.
// ponytail: multi-knowledge-base needs a workspace->remote mapping table; add
// it when a second knowledge base exists.
func ReconcileJob(pool *pgxpool.Pool, svc *SyncService, cfg ReconcileConfig) scheduler.JobSpec {
	return scheduler.JobSpec{
		Name:              "aifirst_cr_reconcile",
		Cadence:           cfg.Interval,
		CatchUpMode:       scheduler.CatchUpLatestOnly,
		CatchUpWindow:     time.Hour,
		RunTimeout:        2 * time.Minute,
		StaleTimeout:      5 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, // ApplySnapshot is idempotent
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{30 * time.Second},
		Scopes:            scheduler.StaticScopes(scheduler.ScopeGlobal),
		Handler: func(ctx context.Context, in scheduler.HandlerInput) (scheduler.HandlerResult, error) {
			snap, err := FetchGitHubSnapshot(ctx, cfg)
			if err != nil {
				return scheduler.HandlerResult{}, err
			}
			rows, err := pool.Query(ctx, `SELECT DISTINCT workspace_id::text FROM cr`)
			if err != nil {
				return scheduler.HandlerResult{}, err
			}
			var workspaces []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return scheduler.HandlerResult{}, err
				}
				workspaces = append(workspaces, id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return scheduler.HandlerResult{}, err
			}
			healed := 0
			for _, wid := range workspaces {
				n, err := svc.ApplySnapshot(ctx, wid, snap)
				healed += n
				if err != nil {
					return scheduler.HandlerResult{RowsAffected: int64(healed)}, err
				}
			}
			return scheduler.HandlerResult{
				RowsAffected: int64(healed),
				Result:       map[string]any{"head": snap.HeadSHA, "crs": len(snap.Statuses), "healed": healed},
			}, nil
		},
	}
}
