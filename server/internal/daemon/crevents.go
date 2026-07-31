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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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

// The four stable [cr] commit-message contracts (P1 design §4.3 / SDD §4.3).
var (
	crCommitStatusRe  = regexp.MustCompile(`^\[cr\] status (CR-\d{4}-\d{3}) (\S+) -> (\S+)$`)
	crCommitMergeRe   = regexp.MustCompile(`^\[cr\] merge metadata (CR-\d{4}-\d{3})`)
	crCommitArchiveRe = regexp.MustCompile(`^\[cr\] archive (CR-\d{4}-\d{3})`)
	crCommitInboxRe   = regexp.MustCompile(`^\[cr\] inbox-emit (CR-\d{4}-\d{3}) event=(\S+)`)
)

type crEventCollector struct {
	roots    []string
	reporter crEventReporter
	logger   *slog.Logger
	// rejectCounts tracks per-file consecutive server rejections; at 3 the file
	// moves to outbox/dead/ so one poisoned event cannot wedge the channel.
	rejectCounts map[string]int
}

func newCREventCollector(roots []string, reporter crEventReporter, logger *slog.Logger) *crEventCollector {
	return &crEventCollector{roots: roots, reporter: reporter, logger: logger, rejectCounts: map[string]int{}}
}

// crEventsLoop is launched from Daemon.Run when CRWorkspaceRoots is non-empty.
func (d *Daemon) crEventsLoop(ctx context.Context) {
	col := newCREventCollector(d.cfg.CRWorkspaceRoots, d.client, d.logger)
	d.logger.Info("cr event collector started", "roots", d.cfg.CRWorkspaceRoots, "interval", d.cfg.HeartbeatInterval)
	ticker := time.NewTicker(d.cfg.HeartbeatInterval)
	defer ticker.Stop()
	col.collectAll(ctx) // first pass immediately, not one interval late
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			col.collectAll(ctx)
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
		if err := os.WriteFile(filepath.Join(root, ".crctl", ".scan-cursor"), []byte(newCursor+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *crEventCollector) applyAck(outboxDir string, ack crEventsAck) {
	for _, file := range ack.Accepted {
		delete(c.rejectCounts, file)
		if strings.HasPrefix(file, "commit:") {
			continue // synthetic id from the commit-scan channel; nothing on disk
		}
		if err := os.Remove(filepath.Join(outboxDir, file)); err != nil && !os.IsNotExist(err) {
			c.logger.Warn("could not delete acked outbox file", "file", file, "error", err)
		}
	}
	for _, rej := range ack.Rejected {
		if strings.HasPrefix(rej.File, "commit:") {
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

// scanCommitsSinceCursor is the fallback channel. Returns the parsed events and
// the new HEAD to persist after a successful report. TODO(CR-2026-002 TASK-09):
// route through pkg/gitguard once it exists; until then this is a direct,
// read-only git invocation (log/rev-parse only).
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
		if ev, ok := parseCRCommitMessage(parts[0], parts[1], parts[2]); ok {
			events = append(events, ev)
		}
	}
	return events, head, nil
}

func parseCRCommitMessage(sha, isoTime, subject string) (crOutboxEvent, bool) {
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
	return crOutboxEvent{}, false
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
