// AIFIRST: CR event collector (CR-2026-002 TASK-06, P1 design §A.3).
//
// Runs on the heartbeat cadence and, for every configured crctl workspace root
// (Config.CRWorkspaceRoots / MULTICA_CR_WORKSPACES):
//
//  1. scans {root}/.crctl/outbox/*.json (the primary channel — crctl writes
//     every event to the --workspace root, including when invoked from a CR
//     worktree, so scanning worktrees separately is unnecessary);
//  2. scans knowledge-base commits since {root}/.crctl/.scan-cursor as the
//     fallback channel (covers old crctl versions, manual git operations and
//     orchestrators committing directly);
//  3. merges the two by (cr_id, commit_sha, event_kind) — the same event
//     arriving via both channels is the normal case;
//  4. reports batches of ≤100 to POST /api/daemon/cr-events, deletes exactly
//     the outbox files the server ACKed, moves files rejected 3 times to
//     .crctl/outbox/dead/, and advances the scan cursor only after a
//     successful report — offline backlogs simply retry next tick.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

const crEventsBatchLimit = 100

// crOutboxEvent mirrors the crctl outbox schema v1 plus the File field used by
// the server ACK protocol. Commit-scan events use a synthetic "commit:" file id
// (nothing on disk to delete).
type crOutboxEvent struct {
	V          int               `json:"v"`
	File       string            `json:"file"`
	EventKind  string            `json:"event_kind"`
	CRID       string            `json:"cr_id"`
	FromStatus string            `json:"from_status"`
	ToStatus   string            `json:"to_status"`
	Trigger    string            `json:"trigger"`
	CommitSHA  string            `json:"commit_sha"`
	Actor      string            `json:"actor"`
	Evidence   map[string]string `json:"evidence,omitempty"`
	Payload    json.RawMessage   `json:"payload,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
}

type crEventsReport struct {
	WorkspaceRootHash string          `json:"workspace_root_hash"`
	Events            []crOutboxEvent `json:"events"`
}

type crEventsAck struct {
	Accepted []string `json:"accepted"`
	Rejected []struct {
		File string `json:"file"`
		Code string `json:"code"`
	} `json:"rejected"`
}

// crEventReporter is the narrow client surface the collector needs; tests
// substitute a fake.
type crEventReporter interface {
	ReportCREvents(ctx context.Context, report crEventsReport) (crEventsAck, error)
}

// ReportCREvents posts one batch to the server (implements crEventReporter).
func (c *Client) ReportCREvents(ctx context.Context, report crEventsReport) (crEventsAck, error) {
	var ack crEventsAck
	err := c.postJSON(ctx, "/api/daemon/cr-events", report, &ack)
	return ack, err
}

// ── grant delivery (CR-2026-002 TASK-08, P1 §B.1 ④) ─────────────────────────

type pendingGrant struct {
	ID    string          `json:"id"`
	CRID  string          `json:"cr_id"`
	Stage string          `json:"stage"`
	Grant json.RawMessage `json:"grant"`
}

type pendingGrantsResponse struct {
	Grants []pendingGrant `json:"grants"`
}

// grantFetcher is the narrow client surface for grant delivery; tests fake it.
type grantFetcher interface {
	FetchPendingGrants(ctx context.Context) (pendingGrantsResponse, error)
	AckGrants(ctx context.Context, ids []string) error
}

func (c *Client) FetchPendingGrants(ctx context.Context) (pendingGrantsResponse, error) {
	var resp pendingGrantsResponse
	err := c.getJSON(ctx, "/api/daemon/approvals/pending", &resp)
	return resp, err
}

func (c *Client) AckGrants(ctx context.Context, ids []string) error {
	return c.postJSON(ctx, "/api/daemon/approvals/ack", map[string]any{"ids": ids}, nil)
}

// deliverGrants writes every pending grant to {root}/.crctl/grants/ for each
// configured workspace root, then acks. Idempotent: overwriting the same grant
// file is harmless, and crctl re-verifies the signature on use anyway.
func deliverGrants(ctx context.Context, roots []string, fetcher grantFetcher, logger *slog.Logger) {
	resp, err := fetcher.FetchPendingGrants(ctx)
	if err != nil {
		logger.Debug("grant fetch deferred", "error", err)
		return
	}
	if len(resp.Grants) == 0 {
		return
	}
	acked := make([]string, 0, len(resp.Grants))
	for _, g := range resp.Grants {
		written := false
		for _, root := range roots {
			dir := filepath.Join(root, ".crctl", "grants")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Warn("cannot create grants dir", "dir", dir, "error", err)
				continue
			}
			name := fmt.Sprintf("%s-%s.grant.json", g.CRID, g.Stage)
			if err := os.WriteFile(filepath.Join(dir, name), append([]byte(g.Grant), '\n'), 0o644); err != nil {
				logger.Warn("cannot write grant file", "file", name, "error", err)
				continue
			}
			logger.Info("grant delivered", "cr", g.CRID, "stage", g.Stage, "root", root)
			written = true
		}
		if written {
			acked = append(acked, g.ID)
		}
	}
	if len(acked) > 0 {
		if err := fetcher.AckGrants(ctx, acked); err != nil {
			logger.Debug("grant ack deferred (grants stay pending, redelivery is idempotent)", "error", err)
		}
	}
}

// The four stable [cr] commit-message contracts (P1 design §4.3 / SDD §4.3),
// plus a fifth (CR-2026-011 TASK-03) for review-verdict visibility: unlike the
// other four, this one is not itself a status transition — a blocked review
// never advances the CR, so it never produces a "[cr] status" commit at all,
// and would otherwise be invisible to the platform (SDD DD-3).
var (
	crCommitStatusRe  = regexp.MustCompile(`^\[cr\] status (CR-\d{4}-\d{3}) (\S+) -> (\S+)$`)
	crCommitMergeRe   = regexp.MustCompile(`^\[cr\] merge metadata (CR-\d{4}-\d{3})`)
	crCommitArchiveRe = regexp.MustCompile(`^\[cr\] archive (CR-\d{4}-\d{3})`)
	crCommitInboxRe   = regexp.MustCompile(`^\[cr\] inbox-emit (CR-\d{4}-\d{3}) event=(\S+)`)
	// Matches only the three review-annotation stages the gate-node projector
	// tracks (SDD DD-3) — dev-start has no preceding AI review, and
	// write-test-report/review-planning-report are out of D7's scope. No `$`
	// anchor: the commit subject continues with free text ("verdict=pass, 0
	// blockers, ...") that this event does not need to parse.
	crCommitReviewRe = regexp.MustCompile(`^\[cr\] review-(requirement|tech-design|code) (CR-\d{4}-\d{3}): verdict=(\w+)`)
)

type crEventCollector struct {
	roots    []string
	reporter crEventReporter
	logger   *slog.Logger
	// rejectCounts tracks per-file consecutive server rejections; at 3 the file
	// moves to outbox/dead/ so one poisoned event cannot wedge the channel.
	rejectCounts map[string]int
	// lastSnapshot tracks the last successfully reported reconcile snapshot
	// per root (TASK-07 daemon mode). Zero time = send on the first pass.
	lastSnapshot map[string]time.Time
	// snapshotInterval is the daemon-mode reconcile cadence.
	// ponytail: fixed 5min matches the server-mode default; make it Config-driven
	// if a deployment ever needs a different cadence.
	snapshotInterval time.Duration
}

func newCREventCollector(roots []string, reporter crEventReporter, logger *slog.Logger) *crEventCollector {
	return &crEventCollector{
		roots: roots, reporter: reporter, logger: logger,
		rejectCounts: map[string]int{}, lastSnapshot: map[string]time.Time{},
		snapshotInterval: 5 * time.Minute,
	}
}

// crEventsLoop is launched from Daemon.Run when CRWorkspaceRoots is non-empty.
func (d *Daemon) crEventsLoop(ctx context.Context) {
	col := newCREventCollector(d.cfg.CRWorkspaceRoots, d.client, d.logger)
	d.logger.Info("cr event collector started", "roots", d.cfg.CRWorkspaceRoots, "interval", d.cfg.HeartbeatInterval)
	ticker := time.NewTicker(d.cfg.HeartbeatInterval)
	defer ticker.Stop()
	col.collectAll(ctx) // first pass immediately, not one interval late
	deliverGrants(ctx, d.cfg.CRWorkspaceRoots, d.client, d.logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			col.collectAll(ctx)
			deliverGrants(ctx, d.cfg.CRWorkspaceRoots, d.client, d.logger)
		}
	}
}

func (c *crEventCollector) collectAll(ctx context.Context) {
	for _, root := range c.roots {
		if err := c.collectRoot(ctx, root); err != nil {
			// Network failures keep everything in place; next tick retries.
			c.logger.Debug("cr event collection deferred", "root", root, "error", err)
		}
	}
}

func (c *crEventCollector) collectRoot(ctx context.Context, root string) error {
	outboxDir := filepath.Join(root, ".crctl", "outbox")
	fileEvents, err := scanOutboxDir(outboxDir)
	if err != nil {
		return err
	}
	commitEvents, newCursor, err := scanCommitsSinceCursor(root)
	if err != nil {
		c.logger.Debug("cr commit scan skipped", "root", root, "error", err)
	}
	events := mergeCREvents(fileEvents, commitEvents)
	snapshotSent := false
	if time.Since(c.lastSnapshot[root]) >= c.snapshotInterval {
		if snap, ok := buildSnapshotEvent(root); ok {
			events = append(events, snap)
			snapshotSent = true
		}
	}
	if len(events) == 0 {
		return nil
	}
	rootHash := sha256Hex(root)
	for start := 0; start < len(events); start += crEventsBatchLimit {
		end := min(start+crEventsBatchLimit, len(events))
		ack, err := c.reporter.ReportCREvents(ctx, crEventsReport{WorkspaceRootHash: rootHash, Events: events[start:end]})
		if err != nil {
			return err // whole remaining backlog stays; retried next tick
		}
		c.applyAck(outboxDir, ack)
	}
	// Only after every batch reported successfully may the cursor advance —
	// a mid-run failure must rescan the same commit range next tick.
	if newCursor != "" {
		// The root may not have a .crctl dir yet (crctl never ran there) —
		// create it, or the cursor write fails every tick and the whole range
		// is rescanned forever.
		if err := os.MkdirAll(filepath.Join(root, ".crctl"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, ".crctl", ".scan-cursor"), []byte(newCursor+"\n"), 0o644); err != nil {
			return err
		}
	}
	if snapshotSent {
		c.lastSnapshot[root] = time.Now()
	}
	return nil
}

// buildSnapshotEvent packages the local authority — HEAD sha plus the raw
// _backlog.yml — as one "snapshot" event for daemon-mode reconcile (TASK-07).
// Parsing happens server-side (governance.ParseBacklog) so both reconcile
// modes share one parser. Returns ok=false when the root has no backlog (not
// a crctl workspace yet) or git is unreadable — skip silently, retry next tick.
func buildSnapshotEvent(root string) (crOutboxEvent, bool) {
	raw, err := os.ReadFile(filepath.Join(root, "change-requests", "_backlog.yml"))
	if err != nil {
		return crOutboxEvent{}, false
	}
	// CR-2026-003 FR-2: ship archived CRs' final states too — absence is valid
	// (nothing archived yet), so a read error just degrades to empty.
	rawHist, err := os.ReadFile(filepath.Join(root, "change-requests", "_history.yml"))
	if err != nil {
		rawHist = nil
	}
	head, err := gitLine(root, "rev-parse", "HEAD")
	if err != nil {
		head = "" // snapshot still useful without the pointer
	}
	payload, err := json.Marshal(map[string]string{"head_sha": head, "backlog": string(raw), "history": string(rawHist)})
	if err != nil {
		return crOutboxEvent{}, false
	}
	name := "snapshot:" + head
	if head == "" {
		name = "snapshot:nohead"
	}
	return crOutboxEvent{
		V: 1, File: name, EventKind: "snapshot", Actor: "daemon-reconcile",
		Payload: payload, OccurredAt: time.Now(),
	}, true
}

func (c *crEventCollector) applyAck(outboxDir string, ack crEventsAck) {
	for _, file := range ack.Accepted {
		delete(c.rejectCounts, file)
		if strings.HasPrefix(file, "commit:") || strings.HasPrefix(file, "snapshot:") {
			continue // synthetic ids (commit-scan / reconcile snapshot); nothing on disk
		}
		if err := os.Remove(filepath.Join(outboxDir, file)); err != nil && !os.IsNotExist(err) {
			c.logger.Warn("could not delete acked outbox file", "file", file, "error", err)
		}
	}
	for _, rej := range ack.Rejected {
		if strings.HasPrefix(rej.File, "commit:") || strings.HasPrefix(rej.File, "snapshot:") {
			continue
		}
		c.rejectCounts[rej.File]++
		c.logger.Warn("cr event rejected by server", "file", rej.File, "code", rej.Code, "attempt", c.rejectCounts[rej.File])
		if c.rejectCounts[rej.File] >= 3 {
			deadDir := filepath.Join(outboxDir, "dead")
			_ = os.MkdirAll(deadDir, 0o755)
			if err := os.Rename(filepath.Join(outboxDir, rej.File), filepath.Join(deadDir, rej.File)); err != nil {
				c.logger.Warn("could not quarantine poisoned outbox file", "file", rej.File, "error", err)
			} else {
				c.logger.Warn("poisoned cr event moved to outbox/dead", "file", rej.File)
				delete(c.rejectCounts, rej.File)
			}
		}
	}
}

// scanOutboxDir reads events in filename order (timestamps sort lexically).
// Unparseable files are reported as synthetic BAD_EVENT rejections locally by
// returning them with V=0 — the server will reject them and the 3-strikes
// quarantine applies, so a corrupt file cannot wedge the loop.
func scanOutboxDir(dir string) ([]crOutboxEvent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".tmp-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	events := make([]crOutboxEvent, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var ev crOutboxEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			ev = crOutboxEvent{} // V=0 → server rejects BAD_EVENT → 3-strikes quarantine
		}
		ev.File = name
		events = append(events, ev)
	}
	return events, nil
}

// crGitGuard lazily loads the controlled-shell whitelist for the collector's
// read-only git calls (TASK-09 closed the TASK-06 TODO: the daemon guards its
// own git here too, caller=system-orchestrator). Unconfigured = direct exec
// (upstream fallback); configured-but-broken = fail closed.
var crGitGuard struct {
	once   sync.Once
	guard  *gitguard.Guard
	broken bool
}

func crGuardCheck(sub string, args ...string) error {
	crGitGuard.once.Do(func() {
		g, err := gitguard.FromEnv()
		if err != nil {
			crGitGuard.broken = true
			return
		}
		crGitGuard.guard = g
	})
	if crGitGuard.broken {
		return &gitguard.Error{Code: gitguard.CodeUnavailable, Caller: gitguard.SystemCaller, Sub: sub, Message: "controlled-shell rules unusable; daemon git denied"}
	}
	if crGitGuard.guard != nil {
		return crGitGuard.guard.Check(sub, args, gitguard.SystemCaller)
	}
	return nil
}

// scanCommitsSinceCursor is the fallback channel. Returns the parsed events and
// the new HEAD to persist after a successful report. Read-only git only
// (rev-parse/log), whitelist-checked when the rules are configured.
func scanCommitsSinceCursor(root string) ([]crOutboxEvent, string, error) {
	head, err := gitLine(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, "", err
	}
	cursorPath := filepath.Join(root, ".crctl", ".scan-cursor")
	cursor := ""
	if raw, err := os.ReadFile(cursorPath); err == nil {
		cursor = strings.TrimSpace(string(raw))
	}
	if cursor == head {
		return nil, "", nil
	}
	rangeArg := head
	if cursor != "" {
		rangeArg = cursor + ".." + head
	}
	if err := crGuardCheck("log", "--reverse", "--format=%H%x00%cI%x00%s", rangeArg); err != nil {
		return nil, "", err
	}
	out, err := exec.Command("git", "-C", root, "log", "--reverse", "--format=%H%x00%cI%x00%s", rangeArg).Output()
	if err != nil {
		// Unknown cursor (e.g. history rewritten): fall back to full scan next call.
		if cursor != "" {
			_ = os.Remove(cursorPath)
		}
		return nil, "", err
	}
	var events []crOutboxEvent
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		if ev, ok := parseCRCommitMessage(root, parts[0], parts[1], parts[2]); ok {
			events = append(events, ev)
		}
	}
	return events, head, nil
}

// parseCRCommitMessage matches one commit subject against the five stable
// [cr] contracts. root is only used by the review contract (it needs to read
// review-annotations/{stage}.yml off disk); the other four never touch it.
func parseCRCommitMessage(root, sha, isoTime, subject string) (crOutboxEvent, bool) {
	when, err := time.Parse(time.RFC3339, isoTime)
	if err != nil {
		when = time.Now()
	}
	base := crOutboxEvent{V: 1, File: "commit:" + sha, CommitSHA: sha, Actor: "commit-scan", OccurredAt: when}
	if m := crCommitStatusRe.FindStringSubmatch(subject); m != nil {
		base.EventKind, base.CRID, base.FromStatus, base.ToStatus = "status", m[1], m[2], m[3]
		return base, true // trigger unrecoverable from the message; server matches on (from, to) only
	}
	if m := crCommitMergeRe.FindStringSubmatch(subject); m != nil {
		base.EventKind, base.CRID = "merge", m[1]
		return base, true
	}
	if m := crCommitArchiveRe.FindStringSubmatch(subject); m != nil {
		base.EventKind, base.CRID = "archive", m[1]
		return base, true
	}
	if m := crCommitInboxRe.FindStringSubmatch(subject); m != nil {
		base.EventKind, base.CRID = "inbox", m[1]
		base.Payload = json.RawMessage(fmt.Sprintf(`{"event":%q}`, m[2]))
		return base, true
	}
	if m := crCommitReviewRe.FindStringSubmatch(subject); m != nil {
		stage, crID, verdict := m[1], m[2], m[3]
		payload, ok := buildReviewPayload(root, sha, crID, stage, verdict)
		if !ok {
			// Annotation file unreadable/unparsable: skip this event rather
			// than report a payload we can't stand behind. Best-effort,
			// matching the rest of the commit-scan fallback channel.
			return crOutboxEvent{}, false
		}
		base.EventKind, base.CRID, base.Payload = "review", crID, payload
		return base, true
	}
	return crOutboxEvent{}, false
}

// reviewAnnotationDoc is the subset of review-annotations/{stage}.yml this
// event needs. Blockers are kept as a raw yaml.Node because the canonical
// form is a scalar string list (SKILL.md) while historical annotations carry
// structured {id,location,issue,suggestion} objects — normalizeBlockers folds
// both into a []string (CR-2026-045 SDD B05).
type reviewAnnotationDoc struct {
	Verdict    string    `yaml:"verdict"`
	Reviewer   string    `yaml:"reviewer"`
	ReviewedAt string    `yaml:"reviewed-at"`
	Blockers   yaml.Node `yaml:"blockers"`
	SubjectSha string    `yaml:"subject-sha256"`
	ReviewLoop struct {
		CurrentAttempt int `yaml:"current-attempt"`
	} `yaml:"review-loop"`
}

// stageAnnotationFile maps a review stage key to its canonical annotation file
// name. tech-design writes sdd.yml, not tech-design.yml (CR-2026-045 B05).
func stageAnnotationFile(stage string) string {
	switch stage {
	case "requirement":
		return "requirement.yml"
	case "tech-design":
		return "sdd.yml"
	case "code":
		return "code.yml"
	default:
		return stage + ".yml"
	}
}

// normalizeBlockers folds a canonical scalar blocker list or historical
// structured blocker objects into a single []string.
func normalizeBlockers(n *yaml.Node) []string {
	if n == nil || n.Kind == 0 {
		return []string{}
	}
	var out []string
	if n.Kind == yaml.ScalarNode {
		if n.Value != "" {
			out = append(out, n.Value)
		}
		return out
	}
	if n.Kind == yaml.SequenceNode {
		for _, item := range n.Content {
			out = append(out, blockerText(item))
		}
	}
	return out
}

func blockerText(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.MappingNode:
		var id, issue string
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1].Value
			switch k {
			case "id":
				id = v
			case "issue":
				issue = v
			}
		}
		if issue != "" {
			return issue
		}
		return id
	}
	return ""
}

// buildReviewPayload reads the annotation from the exact reviewed commit.
// A working-tree read is incorrect when the commit-scan cursor catches up
// across multiple review attempts because every event would otherwise inherit
// the latest attempt's file. The controlled-shell rule admits only this fixed
// CR annotation object shape.
func buildReviewPayload(root, sha, crID, stage, verdict string) (json.RawMessage, bool) {
	fileName := stageAnnotationFile(stage)
	object := sha + ":" + path.Join("change-requests", crID, "review-annotations", fileName)
	if err := crGuardCheck("show", object); err != nil {
		return nil, false
	}
	raw, err := exec.Command("git", "-C", root, "show", object).Output()
	if err != nil {
		return nil, false
	}
	var doc reviewAnnotationDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	if doc.Verdict == "" {
		doc.Verdict = verdict // fall back to the commit subject if the file is mid-write
	}
	blockers := normalizeBlockers(&doc.Blockers)
	payload, err := json.Marshal(map[string]any{
		"stage":          stage,
		"verdict":        doc.Verdict,
		"blockers":       blockers,
		"attempt":        doc.ReviewLoop.CurrentAttempt,
		"reviewer":       doc.Reviewer,
		"reviewed_at":    doc.ReviewedAt,
		"subject_sha256": doc.SubjectSha,
	})
	if err != nil {
		return nil, false
	}
	return payload, true
}

// mergeCREvents dedupes the two channels by the server's idempotency key. The
// outbox event wins (richer: trigger, evidence, payload); commit-scan fills
// gaps only. Server-side ON CONFLICT is the safety net — this merge just
// avoids paying double payload on the wire.
func mergeCREvents(outbox, commits []crOutboxEvent) []crOutboxEvent {
	seen := make(map[string]bool, len(outbox))
	key := func(e crOutboxEvent) string { return e.CRID + "\x00" + e.CommitSHA + "\x00" + e.EventKind }
	merged := make([]crOutboxEvent, 0, len(outbox)+len(commits))
	for _, e := range outbox {
		seen[key(e)] = true
		merged = append(merged, e)
	}
	for _, e := range commits {
		if !seen[key(e)] {
			merged = append(merged, e)
		}
	}
	return merged
}

func gitLine(dir string, args ...string) (string, error) {
	if len(args) > 0 {
		if err := crGuardCheck(args[0], args[1:]...); err != nil {
			return "", err
		}
	}
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
