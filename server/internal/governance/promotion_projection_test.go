// AIFIRST: CR-2026-061 (SDD §4.5/AC-13): after a promotion pre-built run is
// bound to a CR, the gate projector must REUSE that same run row when CR
// status events arrive — pipeline_run row count must not grow, the seq1
// (requirement-register) node must stay passed, and the seq5 approval node
// projects normally.
package governance

import (
	"context"
	"testing"
)

func TestGateProjectionReusesBoundPromotionRun(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9002-003"
	resetGateProjection(t, crID)
	ensureTestWorkspaceOwner(t)
	ctx := context.Background()

	var ownerID string
	if err := testPool.QueryRow(ctx,
		`SELECT user_id::text FROM member WHERE workspace_id = $1::uuid AND role = 'owner' ORDER BY created_at LIMIT 1`,
		testWorkspaceID).Scan(&ownerID); err != nil {
		t.Fatalf("owner member: %v", err)
	}

	// Pre-built promotion run, already bound (cr_id set, first node passed —
	// the exact post-bind shape BindPromotionRunToCR commits).
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number)
		VALUES ($1::uuid, 'promotion projection issue', 'todo', 'none', 'member', $2::uuid, 0,
		        (SELECT COALESCE(MAX(number),0)+1 FROM issue WHERE workspace_id = $1::uuid))
		RETURNING id::text`, testWorkspaceID, ownerID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	var runID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, issue_id, status, inputs, execution_context, started_by)
		VALUES ($1::uuid, 'requirement-authoring', $2, $3::uuid, 'running', '{}'::jsonb, '{}'::jsonb, $4::uuid)
		RETURNING id::text`, testWorkspaceID, crID, issueID, ownerID).Scan(&runID); err != nil {
		t.Fatalf("seed pipeline_run: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO pipeline_node_run (run_id, node_id, ref, kind, seq, status, attempt, started_at)
		VALUES ($1::uuid, '00000000-0000-0000-0011-000000000001', 'requirement-register', 'skill', 1, 'passed', 1, now())`,
		runID); err != nil {
		t.Fatalf("seed pipeline_node_run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM pipeline_node_run WHERE run_id = $1::uuid`, runID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM pipeline_run WHERE id = $1::uuid`, runID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1::uuid`, issueID)
	})

	svc := NewSyncService(testPool, nil)
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "", "drafting", "requirement-register", "s1", "f1.json"),
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "s2", "f2.json"),
	})

	// AC-13: the projector hit the bound row via findOrCreateRun and did not
	// create a second run for this CR+pipeline.
	var runs int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM pipeline_run
		WHERE workspace_id = $1::uuid AND cr_id = $2 AND pipeline_id = 'requirement-authoring'`,
		testWorkspaceID, crID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("pipeline_run rows for bound CR = %d, want 1 (projection must reuse the bound row)", runs)
	}

	// seq1 keeps its passed status — the projector never rewrites skill nodes
	// (D-5); the seq5 approval node projects normally.
	node := ApprovalGateNodes["requirement"]
	var seq1Status string
	if err := testPool.QueryRow(ctx, `
		SELECT status FROM pipeline_node_run
		WHERE run_id = $1::uuid AND node_id = '00000000-0000-0000-0011-000000000001'`,
		runID).Scan(&seq1Status); err != nil {
		t.Fatalf("seq1 node: %v", err)
	}
	if seq1Status != "passed" {
		t.Fatalf("seq1 node status = %q, want passed (projector must not touch it)", seq1Status)
	}
	status, _ := nodeRow(t, crID, node.PipelineID, node.NodeID)
	if status != "running" {
		t.Fatalf("approval node status = %q, want running", status)
	}
}
