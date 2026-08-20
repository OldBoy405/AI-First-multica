// AIFIRST: reconcile — the projection safety net (CR-2026-002 TASK-07, P1
// design §A.4 / SDD C7, FR-3).
//
// Git stays the authority: reconcile reads the knowledge-base repo's HEAD and
// change-requests/_backlog.yml and overwrites any drifted cr projection row
// (including rows flagged needs_reconcile by out-of-order sync). It NEVER
// writes back to git.
//
// Two ways an authority snapshot arrives:
//   - server mode: a sys_cron job polls the GitHub API (read-only PAT,
//     Contents scope) — see reconcile_github.go;
//   - daemon mode: the daemon ships {HEAD, raw _backlog.yml} as a "snapshot"
//     event over the existing cr-events channel and ApplySnapshot runs here.
//
// Both modes converge on ApplySnapshot, so drift healing has one
// implementation.
package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// snapshotPayload is the daemon-mode wire format (event_kind "snapshot" on the
// cr-events channel): local HEAD plus the raw _backlog.yml text. Parsing stays
// server-side so both modes share one parser.
type snapshotPayload struct {
	HeadSHA string `json:"head_sha"`
	Backlog string `json:"backlog"`
	// History is the raw _history.yml text (CR-2026-003 FR-2): archived CRs
	// leave the backlog, so without it they could never self-heal. Optional —
	// an older daemon that doesn't send it degrades to pre-fix behavior.
	History string `json:"history,omitempty"`
}

// maxSnapshotBytes bounds one snapshot payload (a backlog is a list of ids and
// statuses; even hundreds of CRs stay far below this).
const maxSnapshotBytes = 1 << 20

func (s *SyncService) ingestSnapshot(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	if len(ev.Payload) > maxSnapshotBytes {
		return fmt.Errorf("snapshot payload too large: %d bytes", len(ev.Payload))
	}
	var p snapshotPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("snapshot payload: %w", err)
	}
	statuses, err := ParseBacklog([]byte(p.Backlog))
	if err != nil {
		return err
	}
	history, err := ParseHistory([]byte(p.History))
	if err != nil {
		return err
	}
	_, err = s.ApplySnapshot(ctx, workspaceID, AuthoritySnapshot{HeadSHA: p.HeadSHA, Statuses: mergeAuthority(statuses, history)})
	return err
}

// AuthoritySnapshot is one observation of the knowledge-base authority.
type AuthoritySnapshot struct {
	HeadSHA  string
	Statuses map[string]string // cr_id -> status from _backlog.yml
}

// ParseBacklog extracts {cr_id: status} from a raw _backlog.yml. Line endings
// are normalized first (AGENTS.md discipline: CRLF checkouts must never change
// parse results); a file that fails to parse is an error, never an empty map.
func ParseBacklog(raw []byte) (map[string]string, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var doc struct {
		ChangeRequests []struct {
			ID     string `yaml:"id"`
			Status string `yaml:"status"`
		} `yaml:"change-requests"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("_backlog.yml parse: %w", err)
	}
	out := make(map[string]string, len(doc.ChangeRequests))
	for _, e := range doc.ChangeRequests {
		if e.ID != "" && e.Status != "" {
			out[e.ID] = e.Status
		}
	}
	return out, nil
}

// ParseHistory extracts {cr_id: final-status} from a raw _history.yml
// (CR-2026-003 FR-2). Same discipline as ParseBacklog: line endings normalized
// first, and a file that fails to parse is an error, never an empty map. An
// empty/absent file is valid (a workspace that never archived anything).
func ParseHistory(raw []byte) (map[string]string, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var doc struct {
		History []struct {
			ID          string `yaml:"id"`
			FinalStatus string `yaml:"final-status"`
		} `yaml:"history"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("_history.yml parse: %w", err)
	}
	out := make(map[string]string, len(doc.History))
	for _, e := range doc.History {
		if e.ID != "" && e.FinalStatus != "" {
			out[e.ID] = e.FinalStatus
		}
	}
	return out, nil
}

// mergeAuthority combines the in-flight backlog with archived history into one
// authority snapshot. The two sources cannot overlap (cr-archive moves entries
// atomically), but backlog wins defensively: an in-flight status must never be
// masked by a stale history record.
func mergeAuthority(backlog, history map[string]string) map[string]string {
	merged := make(map[string]string, len(backlog)+len(history))
	for id, st := range history {
		merged[id] = st
	}
	for id, st := range backlog {
		merged[id] = st
	}
	return merged
}

// ApplySnapshot replays the authority over one workspace's projection rows.
// Returns how many rows were healed (status fixed, flag cleared, or row
// inserted). Idempotent: applying the same snapshot twice heals zero rows the
// second time.
//
// Rows absent from the snapshot are left untouched: _backlog.yml drops
// archived CRs (they move to _history), so absence is not evidence of drift.
func (s *SyncService) ApplySnapshot(ctx context.Context, workspaceID string, snap AuthoritySnapshot) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT cr_id, status, needs_reconcile FROM cr WHERE workspace_id = $1::uuid`,
		workspaceID)
	if err != nil {
		return 0, err
	}
	existing := map[string]struct {
		status         string
		needsReconcile bool
	}{}
	for rows.Next() {
		var id, st string
		var nr bool
		if err := rows.Scan(&id, &st, &nr); err != nil {
			rows.Close()
			return 0, err
		}
		existing[id] = struct {
			status         string
			needsReconcile bool
		}{st, nr}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	activeRuns := map[string]struct{}{}
	activeRows, err := s.pool.Query(ctx, `
		SELECT cr_id FROM pipeline_run
		WHERE workspace_id = $1::uuid AND pipeline_id = $2
		  AND cr_id IS NOT NULL AND status IN ('running', 'waiting_approval')`,
		workspaceID, PipelineIDs.ArchitectureDesign)
	if err != nil {
		return 0, err
	}
	for activeRows.Next() {
		var crID string
		if err := activeRows.Scan(&crID); err != nil {
			activeRows.Close()
			return 0, err
		}
		activeRuns[crID] = struct{}{}
	}
	activeRows.Close()
	if err := activeRows.Err(); err != nil {
		return 0, err
	}

	healed := 0
	for crID, authStatus := range snap.Statuses {
		if !KnownStatuses[authStatus] {
			continue // never project a status outside the state machine enum
		}
		cur, found := existing[crID]
		if _, active := activeRuns[crID]; active {
			// Live Runner/status events are the authority while an architecture
			// run is active; the installation-root snapshot may still be stale.
			continue
		}
		switch {
		case !found:
			// Projection missed the registration entirely — insert from authority.
			if _, err := s.pool.Exec(ctx, `
				INSERT INTO cr (workspace_id, cr_id, status, projected_commit, needs_reconcile)
				VALUES ($1::uuid, $2, $3, $4, FALSE)
				ON CONFLICT (workspace_id, cr_id) DO NOTHING`,
				workspaceID, crID, authStatus, snap.HeadSHA); err != nil {
				return healed, err
			}
		case cur.status != authStatus || cur.needsReconcile:
			unlock := s.lockCR(workspaceID, crID)
			if _, err := s.pool.Exec(ctx, `
				UPDATE cr SET status = $3, needs_reconcile = FALSE,
				              projected_commit = CASE WHEN $4 <> '' THEN $4 ELSE projected_commit END,
				              updated_at = now()
				WHERE workspace_id = $1::uuid AND cr_id = $2`,
				workspaceID, crID, authStatus, snap.HeadSHA); err != nil {
				unlock()
				return healed, err
			}
			unlock()
		default:
			continue
		}
		healed++
		s.publish(ctx, workspaceID, crID)
	}
	return healed, nil
}
