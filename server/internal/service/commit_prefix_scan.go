package service

// AIFIRST: CR-2026-049 TASK-10 — precise incremental commit-prefix scan
// (SDD §3.4, TD-B6). Pure scan logic: fix the round's HEAD B, page from B
// down to the exact previous cursor A, classify [B..A) with case-sensitive
// prefixes (wip: wins first), heartbeat every page, and never advance the
// cursor on any failure (history rewrite, truncation, rate limit, transport).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/drift"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// ScanRepoInput carries everything one repo round needs; Prefixes come from
// in.Bound only — the scanner never re-reads static config.
type ScanRepoInput struct {
	Bound      drift.BoundRepo
	PrevCursor *string
	Heartbeat  func(ctx context.Context) error
	Source     ghsnapshot.CommitSource
}

// ScanResult is the candidate cursor + classified findings for one repo.
type ScanResult struct {
	HeadSHA  string
	Cursor   string
	Findings []drift.FindingInput
}

// maxPages bounds one round at 100 pages × 100 commits = 10,000 commits
// (SDD §3.4 step 5 fail-safe).
const maxPages = 100

// ErrCursorUnreachable reports a cursor that is not an ancestor of B within
// the page budget (history rewrite / truncation / missing cursor). The plan
// FAILED and the cursor does not advance.
var ErrCursorUnreachable = errors.New("cursor_unreachable")

// ErrScanTruncated reports a mid-scan transport/limit failure.
var ErrScanTruncated = errors.New("scan_truncated")

// Classify maps one subject to (kind, severity, isFinding). wip: wins over the
// whitelist; whitelist prefixes match case-sensitively with strings.HasPrefix.
func Classify(subject string, prefixes []string) (kind, severity string, isFinding bool) {
	if strings.HasPrefix(subject, "wip:") {
		return "wip-on-trunk", "info", true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(subject, p) {
			return "", "", false
		}
	}
	return "bypass-commit", "warn", true
}

// ScanRepo walks one repository round (SDD §3.4):
//  1. Source.Head fixes B (the round's upper bound; trunk advancing mid-round
//     cannot enter the set because every page is rooted at sha=B).
//  2. First scan (no A): baseline only — Cursor=B, zero findings.
//  3. Otherwise page from B until the exact SHA==A; commits before A (new →
//     old) are classified into [B..A). Every page heartbeats; empty page
//     without hitting A, >maxPages, or any error → error, cursor unchanged.
func ScanRepo(ctx context.Context, in ScanRepoInput) (*ScanResult, error) {
	if in.Source == nil {
		return nil, errors.New("scan: nil CommitSource")
	}
	b, err := in.Source.Head(ctx, in.Bound.Token, in.Bound.Owner, in.Bound.Repo, in.Bound.Trunk)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", in.Bound.RepoID, err)
	}
	if b == "" {
		return nil, fmt.Errorf("%s: %w: empty HEAD", in.Bound.RepoID, ErrScanTruncated)
	}
	if in.PrevCursor == nil {
		return &ScanResult{HeadSHA: b, Cursor: b, Findings: nil}, nil
	}
	a := *in.PrevCursor
	if a == b {
		return &ScanResult{HeadSHA: b, Cursor: b, Findings: nil}, nil
	}

	var findings []drift.FindingInput
	reached := false
	for page := 1; page <= maxPages; page++ {
		if in.Heartbeat != nil {
			if err := in.Heartbeat(ctx); err != nil {
				return nil, fmt.Errorf("%s: heartbeat: %w", in.Bound.RepoID, err)
			}
		}
		commits, err := in.Source.Page(ctx, in.Bound.Token, in.Bound.Owner, in.Bound.Repo, in.Bound.Trunk, b, page, 100)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %v", in.Bound.RepoID, ErrScanTruncated, err)
		}
		if len(commits) == 0 {
			return nil, fmt.Errorf("%s: %w: empty page before reaching cursor %s", in.Bound.RepoID, ErrCursorUnreachable, a)
		}
		for _, c := range commits {
			if c.SHA == a {
				reached = true
				break
			}
			kind, severity, isFinding := Classify(c.Subject, in.Bound.Prefixes)
			if !isFinding {
				continue
			}
			evidence := map[string]string{
				"repository_id":  in.Bound.RepoID,
				"trunk":          in.Bound.Trunk,
				"commit_sha":     c.SHA,
				"commit_subject": c.Subject,
				"scanned_at":     nowRFC3339(),
			}
			raw, err := json.Marshal(evidence)
			if err != nil {
				return nil, err
			}
			findings = append(findings, drift.FindingInput{
				// WorkspaceID is stamped by the scheduler handler (the scan
				// function is repo-scoped and contract-fixed without it).
				RepositoryID: in.Bound.RepoID,
				Kind:         kind,
				Severity:     severity,
				Summary:      fmt.Sprintf("%s on %s@%s: %s", kind, in.Bound.RepoID, c.SHA[:min(8, len(c.SHA))], c.Subject),
				Evidence:     raw,
			})
		}
		if reached {
			return &ScanResult{HeadSHA: b, Cursor: b, Findings: findings}, nil
		}
	}
	return nil, fmt.Errorf("%s: %w: %d pages without reaching cursor %s", in.Bound.RepoID, ErrCursorUnreachable, maxPages, a)
}

func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}
