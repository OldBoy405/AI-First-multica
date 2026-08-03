// AIFIRST: integration tests for the gate-node projector (CR-2026-011 TASK-02).
// Follows crsync_test.go's convention: local DB via DATABASE_URL, skip when
// unreachable (TestMain there already handles connect/skip).
package governance

import (
	"context"
	"testing"
)

func resetGateProjection(t *testing.T, crID string) {
	t.Helper()
	ctx := context.Background()
	_, _ = testPool.Exec(ctx, `DELETE FROM pipeline_node_run WHERE run_id IN (SELECT id FROM pipeline_run WHERE cr_id = $1)`, crID)
	_, _ = testPool.Exec(ctx, `DELETE FROM pipeline_run WHERE cr_id = $1`, crID)
	_, _ = testPool.Exec(ctx, `DELETE FROM approval_record WHERE cr_id = $1`, crID)
	resetCR(t, crID)
	ensureTestWorkspaceOwner(t)
}

// ensureTestWorkspaceOwner gives the governance-tests workspace an owner
// member, which findOrCreateRun needs for pipeline_run.started_by — that
// column carries no real actor for a projector-created run (it runs off a
// crctl event, not a user request), so it falls back to the workspace's
// earliest owner rather than leaving the row unattributable. crsync_test.go's
// fixture never needed a member row before this.
func ensureTestWorkspaceOwner(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (email, name) VALUES ('gate-projection-owner@multica.ai', 'Gate Projection Owner')
		ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("upsert test owner user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1::uuid, $2::uuid, 'owner')
		ON CONFLICT (workspace_id, user_id) DO NOTHING`, testWorkspaceID, userID); err != nil {
		t.Fatalf("upsert test owner member: %v", err)
	}
}

func nodeRow(t *testing.T, crID, pipelineID, nodeID string) (status string, approvalID *string) {
	t.Helper()
	err := testPool.QueryRow(context.Background(), `
		SELECT pnr.status, approval_id::text FROM pipeline_node_run pnr
		JOIN pipeline_run pr ON pr.id = pnr.run_id
		WHERE pr.cr_id = $1 AND pr.pipeline_id = $2 AND pnr.node_id = $3::uuid`,
		crID, pipelineID, nodeID).Scan(&status, &approvalID)
	if err != nil {
		t.Fatalf("node_run row not found (cr=%s pipeline=%s node=%s): %v", crID, pipelineID, nodeID, err)
	}
	return
}

func runStatus(t *testing.T, crID, pipelineID string) string {
	t.Helper()
	var status string
	err := testPool.QueryRow(context.Background(), `
		SELECT status FROM pipeline_run WHERE cr_id = $1 AND pipeline_id = $2
		ORDER BY created_at DESC LIMIT 1`, crID, pipelineID).Scan(&status)
	if err != nil {
		t.Fatalf("pipeline_run row not found (cr=%s pipeline=%s): %v", crID, pipelineID, err)
	}
	return status
}

// TestGateProjectionRequirementApprovalFlow walks the requirement stage
// end-to-end: entering requirement-reviewing opens a requirement-authoring
// run and marks its human_approval node running (SDD §4.1 row 1); entering
// requirement-approved marks it passed and links the approval_record that
// unblocked it (row 2).
func TestGateProjectionRequirementApprovalFlow(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9002-001"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "s1", "f1.json"),
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "s2", "f2.json"),
	})

	node := ApprovalGateNodes["requirement"]
	status, approvalID := nodeRow(t, crID, node.PipelineID, node.NodeID)
	if status != "running" {
		t.Fatalf("expected requirement approval node running, got %s", status)
	}
	if approvalID != nil {
		t.Fatalf("expected no approval linked yet, got %v", *approvalID)
	}
	if got := runStatus(t, crID, node.PipelineID); got != "waiting_approval" {
		t.Fatalf("expected run waiting_approval, got %s", got)
	}

	// Simulate the approval API having issued a grant for this stage before
	// the CR advances (matches production ordering: approve, then advance).
	var recordID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO approval_record (workspace_id, cr_id, stage, decision, approver_user_id, evidence_digest, key_id, signature)
		VALUES ($1::uuid, $2, 'requirement', 'approve', (SELECT user_id FROM member WHERE workspace_id = $1::uuid LIMIT 1), 'sha256:test', 'k1', 'sig')
		RETURNING id::text`, testWorkspaceID, crID).Scan(&recordID); err != nil {
		t.Fatalf("insert approval_record fixture: %v", err)
	}

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "requirement-reviewing", "requirement-approved", "approve-requirement", "s3", "f3.json"),
	})

	status, approvalID = nodeRow(t, crID, node.PipelineID, node.NodeID)
	if status != "passed" {
		t.Fatalf("expected requirement approval node passed, got %s", status)
	}
	if approvalID == nil || *approvalID != recordID {
		t.Fatalf("expected node linked to approval_record %s, got %v", recordID, approvalID)
	}
	if got := runStatus(t, crID, node.PipelineID); got != "running" {
		t.Fatalf("expected run demoted back to running after gate clears, got %s", got)
	}
}

// TestGateProjectionPipelineHandoff verifies that entering a new pipeline's
// territory (task-breakdown, i.e. code-implementation) completes the prior
// pipeline's still-open run (architecture-design) — the general rule that
// replaces a per-status "mark completed" special case (SDD §4.1 note).
func TestGateProjectionPipelineHandoff(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9002-002"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "s1", "f1.json"),
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "s2", "f2.json"),
		ev(crID, "status", "requirement-reviewing", "requirement-approved", "approve-requirement", "s3", "f3.json"),
		ev(crID, "status", "requirement-approved", "tech-designing", "write-tech-design", "s4", "f4.json"),
		ev(crID, "status", "tech-designing", "tech-design-review-pending", "write-tech-design-complete", "s5", "f5.json"),
		ev(crID, "status", "tech-design-review-pending", "tech-design-reviewed", "approve-tech-design", "s6", "f6.json"),
	})
	if got := runStatus(t, crID, PipelineIDs.ArchitectureDesign); got != "running" {
		t.Fatalf("expected architecture-design run still open before handoff, got %s", got)
	}

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "tech-design-reviewed", "task-breakdown", "write-dev-tasks", "s7", "f7.json"),
	})

	if got := runStatus(t, crID, PipelineIDs.ArchitectureDesign); got != "completed" {
		t.Fatalf("expected architecture-design run completed on handoff, got %s", got)
	}
	if got := runStatus(t, crID, PipelineIDs.CodeImplementation); got != "waiting_approval" {
		t.Fatalf("expected code-implementation run opened and waiting on dev-start, got %s", got)
	}
	node := ApprovalGateNodes["dev-start"]
	status, _ := nodeRow(t, crID, node.PipelineID, node.NodeID)
	if status != "running" {
		t.Fatalf("expected dev-start node running, got %s", status)
	}
}

// TestGateProjectionRejectedCancelsActiveRun covers the rejected/withdrawn
// path: the currently open run is cancelled and its running node fails.
func TestGateProjectionRejectedCancelsActiveRun(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9002-003"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "s1", "f1.json"),
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "s2", "f2.json"),
		ev(crID, "status", "requirement-reviewing", "rejected", "cr-review-record:reject", "s3", "f3.json"),
	})

	if got := runStatus(t, crID, PipelineIDs.RequirementAuthoring); got != "cancelled" {
		t.Fatalf("expected run cancelled on rejection, got %s", got)
	}
	node := ApprovalGateNodes["requirement"]
	status, _ := nodeRow(t, crID, node.PipelineID, node.NodeID)
	if status != "failed" {
		t.Fatalf("expected running node failed on rejection, got %s", status)
	}
}

// TestGateProjectionSkipsUntrustedFirstSighting is the counterpart to
// TestOutOfOrderFlagsReconcileWithoutCorruptingProjection: an untrusted first
// event (non-empty from_status, i.e. we missed history) must not seed a
// pipeline_run — the cr row itself is flagged needs_reconcile and the gate
// projection must not fabricate data off an unverified transition (DD-2).
func TestGateProjectionSkipsUntrustedFirstSighting(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9002-004"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "requirement-reviewing", "requirement-approved", "approve-requirement", "s1", "f1.json"),
	})

	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_run WHERE cr_id = $1`, crID).Scan(&n); err != nil {
		t.Fatalf("query pipeline_run count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no pipeline_run seeded from an untrusted first sighting, got %d", n)
	}
}
