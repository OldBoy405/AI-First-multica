package ghsnapshot

// AIFIRST: CR-2026-049 TASK-09 — repository access resolution and commit
// listing extensions for the E5 commit-prefix scan (SDD §3.3/§3.4).
// Token discipline: installation tokens live only in the in-memory cache;
// errors/results never carry tokens or auth headers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CommitMeta is one commit subject line for classification.
type CommitMeta struct {
	SHA     string
	Subject string // first line of the commit message, LF-normalized
}

// ListCommitsOptions paginates the commits endpoint (sha=branch or headSHA).
type ListCommitsOptions struct {
	Page    int
	PerPage int
}

// CommitSource is the narrow reader surface the scan service consumes; the
// concrete Client satisfies it, and tests substitute a fake. Primitive params
// keep this package import-free of the drift package (no import cycle).
type CommitSource interface {
	Head(ctx context.Context, token, owner, repo, branch string) (string, error)
	Page(ctx context.Context, token, owner, repo, branch, headSHA string, page, perPage int) ([]CommitMeta, error)
}

// Sentinels for binding resolution (surface codes, never token material).
var (
	ErrRepositoryAccessMissing   = errors.New("repository_access_missing")
	ErrRepositoryAccessAmbiguous = errors.New("repository_access_ambiguous")
	ErrRepositoryProvider        = errors.New("repository_provider_unsupported")
)

// Head returns the first commit SHA of the branch (SDD §3.4 step 1: the fixed
// upper bound B for one scan round).
func (c *Client) Head(ctx context.Context, token, owner, repo, branch string) (string, error) {
	commits, err := c.ListCommits(ctx, token, owner, repo, branch, ListCommitsOptions{Page: 1, PerPage: 1})
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", errors.New("github commits: empty page for HEAD lookup")
	}
	return commits[0].SHA, nil
}

// Page reads one page of commits rooted at the fixed headSHA. The sha query
// param pins the pagination root, so trunk advancing mid-scan never enters the
// round (SDD §3.4).
func (c *Client) Page(ctx context.Context, token, owner, repo, branch, headSHA string, page, perPage int) ([]CommitMeta, error) {
	return c.ListCommits(ctx, token, owner, repo, headSHA, ListCommitsOptions{Page: page, PerPage: perPage})
}

// ResolveRepositoryAccess finds the single installation among installationIDs
// that can read owner/repo with Contents:Read. Zero → repository_access_missing;
// more than one → repository_access_ambiguous (SDD §3.3). Token mint failures
// are surfaced as structured errors without token material.
func (c *Client) ResolveRepositoryAccess(ctx context.Context, installationIDs []int64, owner, repo string) (int64, error) {
	var found []int64
	for _, id := range installationIDs {
		ok, err := c.installationHasContentsRead(ctx, id, owner, repo)
		if err != nil {
			// Rate limits and transport failures abort resolution instead of
			// silently narrowing the candidate set.
			return 0, err
		}
		if ok {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return 0, ErrRepositoryAccessMissing
	case 1:
		return found[0], nil
	default:
		return 0, ErrRepositoryAccessAmbiguous
	}
}

// installationHasContentsRead probes GET /repos/{owner}/{repo}/contents/ with
// per_page=1: Contents:Read is required for the root listing.
func (c *Client) installationHasContentsRead(ctx context.Context, installationID int64, owner, repo string) (bool, error) {
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return false, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/contents/?per_page=1",
		strings.TrimRight(c.apiBase, "/"), url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		return false, nil
	case http.StatusTooManyRequests:
		return false, rateLimitFromResponse(resp, c.now())
	default:
		return false, fmt.Errorf("github repo access: unexpected status %d", resp.StatusCode)
	}
}

// ListCommits reads GET /repos/{owner}/{repo}/commits with the sha query param
// url-encoded (trunk names like "custom/main" are values, never path parts).
// Subjects are the LF-normalized first lines of commit messages. 403/429/5xx,
// timeouts and malformed JSON surface as structured errors without token or
// header material.
func (c *Client) ListCommits(ctx context.Context, token, owner, repo, branch string, opts ListCommitsOptions) ([]CommitMeta, error) {
	if opts.Page < 1 {
		opts.Page = 1
	}
	if opts.PerPage < 1 || opts.PerPage > 100 {
		return nil, fmt.Errorf("github commits: per_page %d out of range", opts.PerPage)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits?sha=%s&per_page=%d&page=%d",
		strings.TrimRight(c.apiBase, "/"), url.PathEscape(owner), url.PathEscape(repo),
		url.QueryEscape(branch), opts.PerPage, opts.Page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github commits: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, fmt.Errorf("github commits: read body: %w", readErr)
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, rateLimitFromResponse(resp, c.now())
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("github commits: server status %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("github commits: unexpected status %d", resp.StatusCode)
	}
	var parsed []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("github commits: malformed response")
	}
	out := make([]CommitMeta, 0, len(parsed))
	for _, p := range parsed {
		msg := strings.ReplaceAll(p.Commit.Message, "\r\n", "\n")
		first, _, _ := strings.Cut(msg, "\n")
		out = append(out, CommitMeta{SHA: p.SHA, Subject: first})
	}
	return out, nil
}

var _ CommitSource = (*Client)(nil)
