// AIFIRST: integration tests for the project-scoped gates endpoint
// (CR-2026-011 TASK-04). Follows crsync_test.go's/approval_test.go's
// convention: shared test DB, skip gracefully when unreachable.
package governance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// gateProjectFixture creates a project + a shell issue linking to it + a cr
// row (status defaults to requirement-reviewing, the AC-1 pending-approval
// state), all in workspaceID. Returns (projectID, crID).
func gateProjectFixture(t *testing.T, workspaceID, crID, status string) string {
	t.Helper()
	ctx := context.Background()

	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1::uuid, 'Gate Test Project')
		RETURNING id::text`, workspaceID).Scan(&projectID); err != nil {
		t.Fatalf("project fixture: %v", err)
	}

	var creatorID string
	if err := testPool.QueryRow(ctx, `SELECT user_id::text FROM member WHERE workspace_id = $1::uuid LIMIT 1`,
		workspaceID).Scan(&creatorID); err != nil {
		t.Fatalf("no member found for workspace (call testUserID first): %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, creator_type, creator_id, number)
		SELECT $1::uuid, $2::uuid, 'CR shell', 'member', $3::uuid,
		       (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1::uuid)
		RETURNING id::text`, workspaceID, projectID, creatorID).Scan(&issueID); err != nil {
		t.Fatalf("issue fixture: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, title, status, shell_issue_id, projected_commit)
		VALUES ($1::uuid, $2, 'Gate test CR', $3, $4::uuid, 'deadbeef')
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET status = $3, shell_issue_id = $4::uuid`,
		workspaceID, crID, status, issueID); err != nil {
		t.Fatalf("cr fixture: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM cr WHERE cr_id = $1`, crID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1::uuid`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1::uuid`, projectID)
	})
	return projectID
}

func gatesHTTP(t *testing.T, svc *ApprovalService, workspaceID, projectID, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/gates", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", projectID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = middleware.SetMemberContext(ctx, workspaceID, db.Member{})
	req = req.WithContext(ctx)
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	rec := httptest.NewRecorder()
	svc.HandleProjectGates(rec, req)
	return rec
}

// TestProjectGatesPendingApprovalAndCanApprove covers the core response
// shape: a CR sitting at requirement-reviewing shows pending_stage=requirement
// and can_approve reflects the caller's workspace role (SDD DD-5).
func TestProjectGatesPendingApprovalAndCanApprove(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ownerID := testUserID(t) // ensures an owner member exists in testWorkspaceID
	crID := "CR-9005-001"
	projectID := gateProjectFixture(t, testWorkspaceID, crID, "requirement-reviewing")
	svc, _ := newTestApprovalService(t)

	rec := gatesHTTP(t, svc, testWorkspaceID, projectID, ownerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("gates request failed: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.CRs) != 1 {
		t.Fatalf("expected 1 CR, got %d: %+v", len(body.CRs), body.CRs)
	}
	got := body.CRs[0]
	if got.CRID != crID || got.PendingStage != "requirement" {
		t.Fatalf("wrong CR/stage: %+v", got)
	}
	if !got.CanApprove {
		t.Fatalf("owner must be able to approve, got can_approve=false")
	}

	// A plain member sees the same CR but cannot approve.
	memberID := testMemberUserID(t)
	rec2 := gatesHTTP(t, svc, testWorkspaceID, projectID, memberID)
	var body2 struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if len(body2.CRs) != 1 || body2.CRs[0].CanApprove {
		t.Fatalf("plain member must see can_approve=false: %+v", body2.CRs)
	}
}

// TestProjectGatesPendingAdvance covers TSUG-001: after a grant has been
// issued (approval_record row exists for the current evidence_digest) but
// before the CR's status has actually advanced, pending_advance must be true
// so the UI shows "approved, waiting to advance" instead of a re-approvable
// card.
func TestProjectGatesPendingAdvance(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ownerID := testUserID(t)
	crID := "CR-9005-002"
	projectID := gateProjectFixture(t, testWorkspaceID, crID, "requirement-reviewing")
	svc, _ := newTestApprovalService(t)

	rec := gatesHTTP(t, svc, testWorkspaceID, projectID, ownerID)
	var before struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.CRs[0].PendingAdvance {
		t.Fatal("expected pending_advance=false before any approval_record exists")
	}

	// Empty evidence -> digest is "" (CanonicalDigestFromEvidence of {}).
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO approval_record (workspace_id, cr_id, stage, decision, approver_user_id, evidence_digest, key_id, signature)
		VALUES ($1::uuid, $2, 'requirement', 'approve', $3::uuid, '', 'k1', 'sig')`,
		testWorkspaceID, crID, ownerID); err != nil {
		t.Fatalf("approval_record fixture: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM approval_record WHERE cr_id = $1`, crID) })

	rec2 := gatesHTTP(t, svc, testWorkspaceID, projectID, ownerID)
	var after struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if !after.CRs[0].PendingAdvance {
		t.Fatal("expected pending_advance=true once a matching approval_record exists")
	}
}

// TestProjectGatesCrossWorkspace404 verifies a project in workspace B is not
// visible when the caller's session is bound to workspace A (SDD §4.3).
func TestProjectGatesCrossWorkspace404(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	testUserID(t) // ensure testWorkspaceID has at least one member (fixture dependency)
	crID := "CR-9005-003"
	projectID := gateProjectFixture(t, testWorkspaceID, crID, "requirement-reviewing")
	svc, _ := newTestApprovalService(t)

	rec := gatesHTTP(t, svc, "00000000-0000-0000-0000-000000000000", projectID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace project access must be 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestProjectGatesDetailIsEmbeddedJSONNotBase64 catches the mistake of typing
// gateNodeView.Detail as []byte, which encoding/json base64-encodes — the
// frontend needs the blocker detail as a plain embedded JSON object.
func TestProjectGatesDetailIsEmbeddedJSONNotBase64(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ownerID := testUserID(t)
	crID := "CR-9005-004"
	projectID := gateProjectFixture(t, testWorkspaceID, crID, "requirement-reviewing")
	svc := NewSyncService(testPool, nil)
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		reviewEvent(crID, "requirement", "block", 1, `[{"id":"REQ-BLOCK-001","location":"FR-1","issue":"x","suggestion":"y"}]`, "r1", "review1.json"),
	})

	approvalSvc, _ := newTestApprovalService(t)
	rec := gatesHTTP(t, approvalSvc, testWorkspaceID, projectID, ownerID)
	var body struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.CRs) != 1 || len(body.CRs[0].GateNodes) == 0 {
		t.Fatalf("expected at least one gate node with detail, got %+v", body.CRs)
	}
	var detail struct {
		Blockers []struct {
			ID string `json:"id"`
		} `json:"blockers"`
	}
	if err := json.Unmarshal(body.CRs[0].GateNodes[0].Detail, &detail); err != nil {
		t.Fatalf("detail must be embedded JSON, not a base64 string: %v (raw: %s)", err, body.CRs[0].GateNodes[0].Detail)
	}
	if len(detail.Blockers) != 1 || detail.Blockers[0].ID != "REQ-BLOCK-001" {
		t.Fatalf("expected 1 blocker REQ-BLOCK-001, got %+v", detail.Blockers)
	}
}

// TestProjectGatesNodeStageIsIndependentOfPendingStage is TASK-06's
// regression: a PASSED node's stage must reflect the node itself, not
// cr.pending_stage (which moves on to the next gate once the first passes —
// labeling a passed requirement node with the CR's now-current tech-design
// stage would be a real UI bug, not cosmetic).
func TestProjectGatesNodeStageIsIndependentOfPendingStage(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ownerID := testUserID(t)
	crID := "CR-9005-005"
	projectID := gateProjectFixture(t, testWorkspaceID, crID, "requirement-reviewing")
	svc := NewSyncService(testPool, nil)

	// Advance past requirement-approved (node "requirement" passes) into
	// tech-design-review-pending — the actual gate-pending state for the
	// tech-design stage (gates.json#approvalStages.tech-design.expect).
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "requirement-reviewing", "requirement-approved", "approve-requirement", "s1", "f1.json"),
		ev(crID, "status", "requirement-approved", "tech-designing", "write-tech-design", "s2", "f2.json"),
		ev(crID, "status", "tech-designing", "tech-design-review-pending", "write-tech-design-complete", "s3", "f3.json"),
	})

	approvalSvc, _ := newTestApprovalService(t)
	rec := gatesHTTP(t, approvalSvc, testWorkspaceID, projectID, ownerID)
	var body struct {
		CRs []projectGateCR `json:"crs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.CRs) != 1 {
		t.Fatalf("expected 1 CR, got %d", len(body.CRs))
	}
	if body.CRs[0].PendingStage != "tech-design" {
		t.Fatalf("expected CR to now be pending tech-design, got %s", body.CRs[0].PendingStage)
	}

	var requirementNode *gateNodeView
	for i := range body.CRs[0].GateNodes {
		n := &body.CRs[0].GateNodes[i]
		if n.NodeID == ApprovalGateNodes["requirement"].NodeID {
			requirementNode = n
		}
	}
	if requirementNode == nil {
		t.Fatal("expected the passed requirement approval node to be present")
	}
	if requirementNode.Stage != "requirement" {
		t.Fatalf("expected the passed node's own stage to stay 'requirement' (not the CR's current pending_stage 'tech-design'), got %q", requirementNode.Stage)
	}
}
