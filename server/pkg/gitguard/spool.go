// AIFIRST: denial spooling (CR-2026-002 TASK-10, P1 design §C.5).
//
// A denial observed by the shim process has no network path of its own; it
// rides the existing crctl outbox → daemon collector → cr-events channel as an
// "audit" event (zero new probes). The payload carries {action, caller, sub,
// code} only — argument bodies never leave the failing process.
package gitguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ActionDenied is the activity_log action written server-side for one spooled
// denial. Must stay equal to governance.ActionGitguardDenied (asserted by a
// test in that package).
const ActionDenied = "aifirst.gitguard_denied"

var spoolSeq atomic.Int64

// SpoolDenial writes one audit outbox event under {workspaceRoot}/.crctl/outbox
// using the crctl event schema v1 (tmp+rename for atomic visibility). Best
// effort by contract: callers deny regardless of the spool outcome.
func SpoolDenial(workspaceRoot, caller, sub, code string) error {
	if workspaceRoot == "" {
		return fmt.Errorf("no workspace root")
	}
	dir := filepath.Join(workspaceRoot, ".crctl", "outbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gi := filepath.Join(workspaceRoot, ".crctl", ".gitignore")
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		_ = os.WriteFile(gi, []byte("*\n"), 0o644)
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]string{
		"action": ActionDenied, "caller": caller, "sub": sub, "code": code,
	})
	if err != nil {
		return err
	}
	event, err := json.Marshal(map[string]any{
		"v": 1, "event_kind": "audit", "cr_id": "",
		"from_status": "", "to_status": "", "trigger": "", "commit_sha": "",
		"actor": caller, "evidence": map[string]string{}, "payload": json.RawMessage(payload),
		"occurred_at": now.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	ts := strings.NewReplacer("-", "", ":", "", ".", "").Replace(now.Format("2006-01-02T15:04:05.000Z"))
	name := fmt.Sprintf("%s-audit-gitguard-%d-%d.json", ts, os.Getpid(), spoolSeq.Add(1))
	tmp := filepath.Join(dir, ".tmp-"+name)
	if err := os.WriteFile(tmp, append(event, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
