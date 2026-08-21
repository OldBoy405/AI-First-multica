package drift

// AIFIRST: CR-2026-049 TASK-09 — per-workspace repository binding resolver
// (SDD §3.3/§3.4, TD-B5). Pure logic over the generated declaration, the
// workspace's repos and its GitHub installations; the concrete GitHub client is
// injected through a narrow interface so tests fake it and the ghsnapshot
// package never imports this one (no cycle).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/commitprefix"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// RepoData is the persisted workspace.repos entry (URL + description only —
// trunk comes from the generated declaration, never from workspace.repos).
type RepoData struct {
	URL         string
	Description string
}

// GitHubInstallation is the binding's view of a github_installation row.
type GitHubInstallation struct {
	ID             string
	InstallationID int64
}

// BoundRepo is one fully resolved repo: declaration + the installation token
// minted for it. Prefixes come verbatim from the declaration — the scan task
// never re-reads static config.
type BoundRepo struct {
	RepoID   string
	Owner    string
	Repo     string
	Trunk    string
	Prefixes []string
	Token    string
}

// RepositoryBindingResolver resolves all generated declarations for one
// workspace; concrete implementations back it with DB rows + a GitHub client.
type RepositoryBindingResolver interface {
	ResolveBindings(ctx context.Context, workspaceID string, decls map[string]commitprefix.RepoPrefixDecl, wsRepos []RepoData, installations []GitHubInstallation) ([]BoundRepo, error)
}

// AccessResolver checks Contents:Read across installations and mints cached
// installation tokens; the ghsnapshot.Client satisfies it.
type AccessResolver interface {
	ResolveRepositoryAccess(ctx context.Context, installationIDs []int64, owner, repo string) (int64, error)
	InstallationToken(ctx context.Context, installationID int64) (string, error)
}

type resolver struct {
	access AccessResolver
}

// NewResolver wires the default resolver around a GitHub access client.
func NewResolver(access AccessResolver) RepositoryBindingResolver {
	return &resolver{access: access}
}

var (
	ErrRepositoryAccessMissing   = errors.New("repository_access_missing")
	ErrRepositoryAccessAmbiguous = errors.New("repository_access_ambiguous")
	ErrProviderUnsupported       = errors.New("repository_provider_unsupported")
)

// canonicalOwnerRepo normalizes any supported remote URL to (owner, repo) for
// GitHub only. HTTPS/SSH forms of the same repo compare equal.
func canonicalOwnerRepo(raw string) (owner, repo string, err error) {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "https://github.com/"):
		parts := strings.Split(strings.TrimPrefix(u, "https://github.com/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], nil
		}
	case strings.HasPrefix(u, "git@github.com:"):
		parts := strings.Split(strings.TrimPrefix(u, "git@github.com:"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], nil
		}
	}
	return "", "", ErrProviderUnsupported
}

// ResolveBindings requires every generated declaration to match exactly one
// workspace.repos entry (canonicalized) and resolve to exactly one GitHub
// installation with Contents:Read. All-or-nothing: one failure fails the whole
// workspace plan (SDD §3.3), no silent repo skipping.
func (r *resolver) ResolveBindings(ctx context.Context, workspaceID string, decls map[string]commitprefix.RepoPrefixDecl, wsRepos []RepoData, installations []GitHubInstallation) ([]BoundRepo, error) {
	type canonRepo struct {
		owner, repo string
		github      bool
	}
	canon := make([]canonRepo, 0, len(wsRepos))
	for _, wr := range wsRepos {
		owner, repo, err := canonicalOwnerRepo(wr.URL)
		canon = append(canon, canonRepo{owner: owner, repo: repo, github: err == nil})
	}
	ids := make([]int64, 0, len(installations))
	for _, in := range installations {
		ids = append(ids, in.InstallationID)
	}

	// Deterministic order: declaration id ascending.
	declIDs := make([]string, 0, len(decls))
	for id := range decls {
		declIDs = append(declIDs, id)
	}
	for i := 1; i < len(declIDs); i++ {
		for j := i; j > 0 && declIDs[j] < declIDs[j-1]; j-- {
			declIDs[j], declIDs[j-1] = declIDs[j-1], declIDs[j]
		}
	}

	out := make([]BoundRepo, 0, len(decls))
	for _, id := range declIDs {
		decl := decls[id]
		declOwner, declRepo, err := canonicalOwnerRepo(decl.CanonicalURL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %s", id, ErrProviderUnsupported, decl.CanonicalURL)
		}
		matchCount := 0
		for i := range canon {
			c := &canon[i]
			if c.github && c.owner == declOwner && c.repo == declRepo {
				matchCount++
			}
		}
		if matchCount == 0 {
			return nil, fmt.Errorf("%s: %w: no workspace.repos entry matches %s/%s", id, ErrRepositoryAccessMissing, declOwner, declRepo)
		}
		if matchCount > 1 {
			return nil, fmt.Errorf("%s: %w: multiple workspace.repos entries match %s/%s", id, ErrRepositoryAccessAmbiguous, declOwner, declRepo)
		}
		installationID, err := r.access.ResolveRepositoryAccess(ctx, ids, declOwner, declRepo)
		if err != nil {
			switch err {
			case ghsnapshot.ErrRepositoryAccessMissing:
				return nil, fmt.Errorf("%s: %w", id, ErrRepositoryAccessMissing)
			case ghsnapshot.ErrRepositoryAccessAmbiguous:
				return nil, fmt.Errorf("%s: %w", id, ErrRepositoryAccessAmbiguous)
			default:
				return nil, err
			}
		}
		token, err := r.access.InstallationToken(ctx, installationID)
		if err != nil {
			return nil, err
		}
		out = append(out, BoundRepo{
			RepoID: id, Owner: declOwner, Repo: declRepo, Trunk: decl.Trunk,
			Prefixes: append([]string(nil), decl.Prefixes...),
			Token:    token,
		})
	}
	return out, nil
}
