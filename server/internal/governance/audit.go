// AIFIRST: audit event ingestion (CR-2026-002 TASK-10, P1 design §C.5/§B.4).
//
// "audit" outbox events ride the existing daemon cr-events channel but never
// touch the cr_sync_event ledger or the cr projection: they carry no commit
// sha (the ledger's idempotency key), so they insert straight into
// activity_log. Append-only audit tolerates the resulting crash-window
// duplicates (report acked but outbox file not yet deleted).
//
// Details are minimal by design: gitguard denials carry {caller, sub, code}
// and evidence drift carries {cr_id, stage, expected_digest, actual_digest,
// detected_at} — never command argument bodies or evidence file contents.
package governance

import (
	"context"
	"encoding/json"
	"fmt"
)

// auditActions is the closed set of activity_log actions an audit event may
// write. Anything else is rejected (and dead-lettered by the daemon after 3
// strikes) so a forged outbox file cannot mint arbitrary audit rows.
var auditActions = map[string]bool{
	ActionGitguardDenied: true,
	ActionEvidenceDrift:  true,
}

// maxAuditDetailBytes bounds one details payload; audit facts are countable
// scalars, so anything bigger is malformed or smuggling content.
const maxAuditDetailBytes = 4096

func (s *SyncService) ingestAudit(ctx context.Context, workspaceID string, ev OutboxEvent) error {
	if len(ev.Payload) > maxAuditDetailBytes {
		return fmt.Errorf("audit payload too large: %d bytes", len(ev.Payload))
	}
	var details map[string]any
	if err := json.Unmarshal(ev.Payload, &details); err != nil {
		return fmt.Errorf("audit payload not an object: %w", err)
	}
	action, _ := details["action"].(string)
	if !auditActions[action] {
		return fmt.Errorf("unknown audit action %q", action)
	}
	delete(details, "action")
	if ev.CRID != "" {
		details["cr_id"] = ev.CRID
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		VALUES ($1::uuid, NULL, 'system', NULL, $2, $3, $4)`,
		workspaceID, action, detailsJSON, ev.OccurredAt)
	return err
}
