// AIFIRST: integration tests for review-event projection (CR-2026-011
// TASK-03). Follows crsync_test.go's/gate_projection_test.go's convention.
package governance

import (
	"context"
	"strconv"
	"testing"
)

func reviewEvent(crID, stage, verdict string, attempt int, blockers, sha, file string) OutboxEvent {
	payload := []byte(`{"stage":"` + stage + `","verdict":"` + verdict + `","attempt":` +
		strconv.Itoa(attempt) + `,"blockers":` + blockers + `,"reviewer":"ai-reviewer","reviewed_at":"2026-08-02T10:00:00+08:00"}`)
	e := ev(crID, "review", "", "", "", sha, file)
	e.Payload = payload
	return e
}

func reviewNodeRow(t *testing.T, crID, pipelineID, nodeID string, attempt int) (status string, detail []byte) {
	t.Helper()
	err := testPool.QueryRow(context.Background(), `
		SELECT pnr.status, pnr.detail FROM pipeline_node_run pnr
		JOIN pipeline_run pr ON pr.id = pnr.run_id
		WHERE pr.cr_id = $1 AND pr.pipeline_id = $2 AND pnr.node_id = $3::uuid AND pnr.attempt = $4`,
		crID, pipelineID, nodeID, attempt).Scan(&status, &detail)
	if err != nil {
		t.Fatalf("review node_run row not found (cr=%s pipeline=%s node=%s attempt=%d): %v", crID, pipelineID, nodeID, attempt, err)
	}
	return
}

// TestApplyReviewBlockedThenPassedKeepsBothAttempts verifies the reviewLoop
// history requirement (D7 FR-5, "attempt N/3"): a blocked round 1 and a
// passing round 2 must both remain queryable as distinct rows, not collapse
// into one — attempt is part of the table's uniqueness key precisely so a
// later round doesn't overwrite an earlier one's record.
func TestApplyReviewBlockedThenPassedKeepsBothAttempts(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9004-001"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		reviewEvent(crID, "requirement", "block", 1, `[{"id":"REQ-BLOCK-001","location":"FR-3","issue":"x","suggestion":"y"}]`, "r1", "review1.json"),
	})
	node := ReviewGateNodes["requirement"]
	status, detail := reviewNodeRow(t, crID, node.PipelineID, node.NodeID, 1)
	if status != "blocked" {
		t.Fatalf("expected attempt 1 blocked, got %s", status)
	}
	if len(detail) == 0 || string(detail) == "{}" {
		t.Fatalf("expected blocker detail persisted, got %s", detail)
	}
	// Runner owns detail.runner. A projector replay must merge review fields
	// at the top level rather than replace that namespace.
	if _, err := testPool.Exec(context.Background(), `
		UPDATE pipeline_node_run SET detail=jsonb_set(detail,'{runner}','{"wait_reason":"authority_postcondition"}'::jsonb,true)
		WHERE run_id IN (SELECT id FROM pipeline_run WHERE cr_id=$1) AND node_id=$2::uuid AND attempt=1`, crID, node.NodeID); err != nil {
		t.Fatalf("seed runner detail: %v", err)
	}
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		reviewEvent(crID, "requirement", "block", 1, `["still blocked"]`, "r1b", "review1b.json"),
	})
	var waitReason string
	if err := testPool.QueryRow(context.Background(), `
		SELECT detail->'runner'->>'wait_reason' FROM pipeline_node_run n JOIN pipeline_run r ON r.id=n.run_id
		WHERE r.cr_id=$1 AND n.node_id=$2::uuid AND n.attempt=1`, crID, node.NodeID).Scan(&waitReason); err != nil || waitReason != "authority_postcondition" {
		t.Fatalf("review replay lost detail.runner: reason=%q err=%v", waitReason, err)
	}

	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		reviewEvent(crID, "requirement", "pass", 2, `[]`, "r2", "review2.json"),
	})
	status2, _ := reviewNodeRow(t, crID, node.PipelineID, node.NodeID, 2)
	if status2 != "passed" {
		t.Fatalf("expected attempt 2 passed, got %s", status2)
	}

	// Attempt 1's row must still read "blocked" — a later round does not
	// retroactively rewrite history.
	status1Again, _ := reviewNodeRow(t, crID, node.PipelineID, node.NodeID, 1)
	if status1Again != "blocked" {
		t.Fatalf("expected attempt 1 to remain blocked after attempt 2 passed, got %s", status1Again)
	}
}

// TestApplyReviewUnknownStageIsNoOp covers dev-start (no review node) and any
// future/unexpected stage value: the event is ingested (ledger row written)
// but produces no pipeline_run/pipeline_node_run rows.
func TestApplyReviewUnknownStageIsNoOp(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	crID := "CR-9004-002"
	resetGateProjection(t, crID)
	svc := NewSyncService(testPool, nil)

	resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		reviewEvent(crID, "dev-start", "pass", 1, `[]`, "r1", "review1.json"),
	})
	if len(resp.Accepted) != 1 {
		t.Fatalf("expected the event itself to be accepted, got %+v", resp)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_run WHERE cr_id = $1`, crID).Scan(&n); err != nil {
		t.Fatalf("query pipeline_run count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no pipeline_run for a stage with no review node, got %d", n)
	}
}
