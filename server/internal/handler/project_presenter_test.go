package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// createPresenterTestMember adds a fresh user as a member of the shared
// test workspace with the given role, cleaning up both rows on test end.
func createPresenterTestMember(t *testing.T, role string) string {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var userID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, fmt.Sprintf("Presenter Test %s", role), fmt.Sprintf("presenter-%s-%d@multica.ai", role, suffix)).Scan(&userID); err != nil {
		t.Fatalf("create %s user: %v", role, err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
	`, testWorkspaceID, userID, role); err != nil {
		t.Fatalf("create %s member: %v", role, err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		testPool.Exec(cleanupCtx, `DELETE FROM project_presenter_grant WHERE user_id = $1`, userID)
		testPool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
		testPool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

// createPresenterTestProject creates a project in the shared test workspace
// via the regular handler entry point (matching the precedent in
// TestCreateSubIssueInheritsParentProject).
func createPresenterTestProject(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title": fmt.Sprintf("Presenter Test Project %d", time.Now().UnixNano()),
	})
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateProject: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var project ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		testPool.Exec(cleanupCtx, `DELETE FROM project_presenter_grant WHERE project_id = $1`, project.ID)
		req := newRequest("DELETE", "/api/projects/"+project.ID, nil)
		req = withURLParam(req, "id", project.ID)
		testHandler.DeleteProject(httptest.NewRecorder(), req)
	})
	return project.ID
}

func requestPresenter(projectID, callerID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(callerID, "POST", "/api/projects/"+projectID+"/presenter/request", nil)
	req = withURLParam(req, "id", projectID)
	testHandler.RequestPresenter(w, req)
	return w
}

func approvePresenter(projectID, approverID, targetID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(approverID, "POST", "/api/projects/"+projectID+"/presenter/approve", map[string]any{"user_id": targetID})
	req = withURLParam(req, "id", projectID)
	testHandler.ApprovePresenter(w, req)
	return w
}

func rejectPresenter(projectID, approverID, targetID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(approverID, "POST", "/api/projects/"+projectID+"/presenter/reject", map[string]any{"user_id": targetID})
	req = withURLParam(req, "id", projectID)
	testHandler.RejectPresenter(w, req)
	return w
}

func transferPresenter(projectID, callerID, targetID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(callerID, "POST", "/api/projects/"+projectID+"/presenter/transfer", map[string]any{"user_id": targetID})
	req = withURLParam(req, "id", projectID)
	testHandler.TransferPresenter(w, req)
	return w
}

func revokePresenter(projectID, approverID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(approverID, "POST", "/api/projects/"+projectID+"/presenter/revoke", nil)
	req = withURLParam(req, "id", projectID)
	testHandler.RevokePresenter(w, req)
	return w
}

func releasePresenter(projectID, callerID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(callerID, "POST", "/api/projects/"+projectID+"/presenter/release", nil)
	req = withURLParam(req, "id", projectID)
	testHandler.ReleasePresenter(w, req)
	return w
}

func getPresenterState(projectID, callerID string) (*httptest.ResponseRecorder, PresenterStateResponse) {
	w := httptest.NewRecorder()
	req := newRequestAs(callerID, "GET", "/api/projects/"+projectID+"/presenter", nil)
	req = withURLParam(req, "id", projectID)
	testHandler.GetPresenterState(w, req)
	var resp PresenterStateResponse
	json.NewDecoder(w.Body).Decode(&resp)
	return w, resp
}

func decodeGrant(w *httptest.ResponseRecorder) PresenterGrantResponse {
	var g PresenterGrantResponse
	json.NewDecoder(w.Body).Decode(&g)
	return g
}

func decodeErrCode(w *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	json.NewDecoder(w.Body).Decode(&body)
	return body.Code
}

// TestPresenterRequestApproveFlow covers the base happy path: a plain member
// requests, the workspace owner approves, and the grant becomes active with
// the requester's original request time preserved.
func TestPresenterRequestApproveFlow(t *testing.T) {
	projectID := createPresenterTestProject(t)
	member := createPresenterTestMember(t, "member")

	w := requestPresenter(projectID, member)
	if w.Code != http.StatusCreated {
		t.Fatalf("RequestPresenter: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	requested := decodeGrant(w)
	if requested.Status != "pending" || requested.UserID != member {
		t.Fatalf("RequestPresenter: unexpected grant %+v", requested)
	}

	w = approvePresenter(projectID, testUserID, member)
	if w.Code != http.StatusOK {
		t.Fatalf("ApprovePresenter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	approved := decodeGrant(w)
	if approved.Status != "active" || approved.UserID != member || approved.GrantedBy != testUserID {
		t.Fatalf("ApprovePresenter: unexpected grant %+v", approved)
	}
	if approved.CreatedAt != requested.CreatedAt {
		t.Fatalf("ApprovePresenter: expected created_at to be preserved from the request (%s), got %s", requested.CreatedAt, approved.CreatedAt)
	}

	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM project_presenter_grant WHERE project_id = $1 AND user_id = $2`,
		projectID, member,
	).Scan(&status); err != nil {
		t.Fatalf("query grant status: %v", err)
	}
	if status != "active" {
		t.Fatalf("DB grant status = %q, want active", status)
	}
}

// TestPresenterRequestRejectFlow: owner rejects instead of approving; no
// active presenter results.
func TestPresenterRequestRejectFlow(t *testing.T) {
	projectID := createPresenterTestProject(t)
	member := createPresenterTestMember(t, "member")

	if w := requestPresenter(projectID, member); w.Code != http.StatusCreated {
		t.Fatalf("RequestPresenter: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	w := rejectPresenter(projectID, testUserID, member)
	if w.Code != http.StatusOK {
		t.Fatalf("RejectPresenter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	rejected := decodeGrant(w)
	if rejected.Status != "rejected" {
		t.Fatalf("RejectPresenter: expected status rejected, got %+v", rejected)
	}

	_, state := getPresenterState(projectID, testUserID)
	if state.Presenter != nil {
		t.Fatalf("expected no active presenter after rejection, got %+v", state.Presenter)
	}
}

// TestPresenterTransferFlow: the active presenter hands control directly to
// another member without a new request/approve cycle.
func TestPresenterTransferFlow(t *testing.T) {
	projectID := createPresenterTestProject(t)
	memberA := createPresenterTestMember(t, "member")
	memberB := createPresenterTestMember(t, "member")

	requestPresenter(projectID, memberA)
	approvePresenter(projectID, testUserID, memberA)

	w := transferPresenter(projectID, memberA, memberB)
	if w.Code != http.StatusOK {
		t.Fatalf("TransferPresenter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	transferred := decodeGrant(w)
	if transferred.Status != "active" || transferred.UserID != memberB || transferred.GrantedBy != memberA {
		t.Fatalf("TransferPresenter: unexpected grant %+v", transferred)
	}

	var oldStatus string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM project_presenter_grant WHERE project_id = $1 AND user_id = $2`,
		projectID, memberA,
	).Scan(&oldStatus); err != nil {
		t.Fatalf("query old grant status: %v", err)
	}
	if oldStatus != "transferred" {
		t.Fatalf("old presenter's grant status = %q, want transferred", oldStatus)
	}

	var activeCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_presenter_grant WHERE project_id = $1 AND status = 'active'`,
		projectID,
	).Scan(&activeCount); err != nil {
		t.Fatalf("count active grants: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly 1 active grant after transfer, got %d", activeCount)
	}
}

// TestPresenterRevokeFlow: owner forcibly ends the active presenter's
// control.
func TestPresenterRevokeFlow(t *testing.T) {
	projectID := createPresenterTestProject(t)
	member := createPresenterTestMember(t, "member")

	requestPresenter(projectID, member)
	approvePresenter(projectID, testUserID, member)

	w := revokePresenter(projectID, testUserID)
	if w.Code != http.StatusOK {
		t.Fatalf("RevokePresenter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	revoked := decodeGrant(w)
	if revoked.Status != "revoked" {
		t.Fatalf("RevokePresenter: expected status revoked, got %+v", revoked)
	}

	_, state := getPresenterState(projectID, testUserID)
	if state.Presenter != nil {
		t.Fatalf("expected no active presenter after revoke, got %+v", state.Presenter)
	}
}

// TestPresenterReleaseFlow: the presenter voluntarily gives up control.
func TestPresenterReleaseFlow(t *testing.T) {
	projectID := createPresenterTestProject(t)
	member := createPresenterTestMember(t, "member")

	requestPresenter(projectID, member)
	approvePresenter(projectID, testUserID, member)

	w := releasePresenter(projectID, member)
	if w.Code != http.StatusOK {
		t.Fatalf("ReleasePresenter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	released := decodeGrant(w)
	if released.Status != "released" {
		t.Fatalf("ReleasePresenter: expected status released, got %+v", released)
	}
}

// TestPresenterIllegalTransitions is the AC-1 rejection matrix: every
// non-owner / non-presenter / wrong-role / duplicate / missing-target attempt
// must fail with a structured code and touch no state.
func TestPresenterIllegalTransitions(t *testing.T) {
	t.Run("non-owner cannot approve", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		admin := createPresenterTestMember(t, "admin")
		member := createPresenterTestMember(t, "member")
		requestPresenter(projectID, member)

		w := approvePresenter(projectID, admin, member)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "insufficient_permissions" {
			t.Fatalf("expected code insufficient_permissions, got %q", code)
		}
	})

	t.Run("non-presenter cannot transfer", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		memberA := createPresenterTestMember(t, "member")
		memberB := createPresenterTestMember(t, "member")
		memberC := createPresenterTestMember(t, "member")
		requestPresenter(projectID, memberA)
		approvePresenter(projectID, testUserID, memberA)

		w := transferPresenter(projectID, memberB, memberC)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "not_presenter" {
			t.Fatalf("expected code not_presenter, got %q", code)
		}
	})

	t.Run("owner/admin cannot request", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		admin := createPresenterTestMember(t, "admin")

		w := requestPresenter(projectID, testUserID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("owner request: expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "role_cannot_request" {
			t.Fatalf("owner request: expected code role_cannot_request, got %q", code)
		}

		w = requestPresenter(projectID, admin)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("admin request: expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "role_cannot_request" {
			t.Fatalf("admin request: expected code role_cannot_request, got %q", code)
		}
	})

	t.Run("duplicate request is rejected", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		member := createPresenterTestMember(t, "member")
		if w := requestPresenter(projectID, member); w.Code != http.StatusCreated {
			t.Fatalf("first request: expected 201, got %d: %s", w.Code, w.Body.String())
		}

		w := requestPresenter(projectID, member)
		if w.Code != http.StatusConflict {
			t.Fatalf("duplicate request: expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "request_already_pending" {
			t.Fatalf("expected code request_already_pending, got %q", code)
		}
	})

	t.Run("revoke with no active presenter", func(t *testing.T) {
		projectID := createPresenterTestProject(t)

		w := revokePresenter(projectID, testUserID)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "no_active_presenter" {
			t.Fatalf("expected code no_active_presenter, got %q", code)
		}
	})

	t.Run("approve with no pending request", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		member := createPresenterTestMember(t, "member")

		w := approvePresenter(projectID, testUserID, member)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "no_pending_request" {
			t.Fatalf("expected code no_pending_request, got %q", code)
		}
	})

	t.Run("approve while a presenter is already active", func(t *testing.T) {
		projectID := createPresenterTestProject(t)
		memberA := createPresenterTestMember(t, "member")
		memberB := createPresenterTestMember(t, "member")
		requestPresenter(projectID, memberA)
		approvePresenter(projectID, testUserID, memberA)
		requestPresenter(projectID, memberB)

		w := approvePresenter(projectID, testUserID, memberB)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if code := decodeErrCode(w); code != "presenter_already_active" {
			t.Fatalf("expected code presenter_already_active, got %q", code)
		}
	})
}

// TestGetPresenterStateVisibility is TSUG-003: pending_requests is
// owner/admin-only, my_request is visible to any caller with a request of
// their own, and an unrelated member sees neither.
func TestGetPresenterStateVisibility(t *testing.T) {
	projectID := createPresenterTestProject(t)
	requester := createPresenterTestMember(t, "member")
	bystander := createPresenterTestMember(t, "member")
	requestPresenter(projectID, requester)

	_, ownerView := getPresenterState(projectID, testUserID)
	if len(ownerView.PendingRequests) != 1 || ownerView.PendingRequests[0].UserID != requester {
		t.Fatalf("owner should see the pending request in pending_requests, got %+v", ownerView.PendingRequests)
	}

	_, requesterView := getPresenterState(projectID, requester)
	if len(requesterView.PendingRequests) != 0 {
		t.Fatalf("non-owner/admin should not see the full pending_requests list, got %+v", requesterView.PendingRequests)
	}
	if requesterView.MyRequest == nil || requesterView.MyRequest.UserID != requester {
		t.Fatalf("requester should see their own request via my_request, got %+v", requesterView.MyRequest)
	}

	_, bystanderView := getPresenterState(projectID, bystander)
	if len(bystanderView.PendingRequests) != 0 {
		t.Fatalf("bystander should not see pending_requests, got %+v", bystanderView.PendingRequests)
	}
	if bystanderView.MyRequest != nil {
		t.Fatalf("bystander has no request of their own, my_request should be nil, got %+v", bystanderView.MyRequest)
	}
}

// TestPresenterMemberRemovalLinkage: removing a member who holds an active
// grant (or a pending request) on one project must close it — an active
// grant is revoked, a pending request is rejected — without touching grants
// belonging to other users. Exercises revokeAndRemoveMember directly (same
// package), matching the existing precedent in agent_permission_test.go.
func TestPresenterMemberRemovalLinkage(t *testing.T) {
	ctx := context.Background()
	projectID := createPresenterTestProject(t)
	activeHolder := createPresenterTestMember(t, "member")
	pendingHolder := createPresenterTestMember(t, "member")
	untouchedHolder := createPresenterTestMember(t, "member")

	requestPresenter(projectID, activeHolder)
	approvePresenter(projectID, testUserID, activeHolder)
	requestPresenter(projectID, untouchedHolder)

	// pendingHolder requests on a second project so a member can be removed
	// with only a pending (never active) grant.
	secondProjectID := createPresenterTestProject(t)
	requestPresenter(secondProjectID, pendingHolder)

	var activeHolderMemberRowID string
	if err := testPool.QueryRow(ctx,
		`SELECT id FROM member WHERE workspace_id = $1 AND user_id = $2`,
		testWorkspaceID, activeHolder,
	).Scan(&activeHolderMemberRowID); err != nil {
		t.Fatalf("load active holder member row: %v", err)
	}
	if _, err := testHandler.revokeAndRemoveMember(ctx,
		util.MustParseUUID(testWorkspaceID),
		util.MustParseUUID(activeHolder),
		util.MustParseUUID(activeHolderMemberRowID),
		util.MustParseUUID(testUserID),
	); err != nil {
		t.Fatalf("revokeAndRemoveMember(activeHolder): %v", err)
	}

	var activeHolderStatus string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM project_presenter_grant WHERE project_id = $1 AND user_id = $2`,
		projectID, activeHolder,
	).Scan(&activeHolderStatus); err != nil {
		t.Fatalf("query active holder grant: %v", err)
	}
	if activeHolderStatus != "revoked" {
		t.Fatalf("active grant status after member removal = %q, want revoked", activeHolderStatus)
	}

	var pendingHolderMemberRowID string
	if err := testPool.QueryRow(ctx,
		`SELECT id FROM member WHERE workspace_id = $1 AND user_id = $2`,
		testWorkspaceID, pendingHolder,
	).Scan(&pendingHolderMemberRowID); err != nil {
		t.Fatalf("load pending holder member row: %v", err)
	}
	if _, err := testHandler.revokeAndRemoveMember(ctx,
		util.MustParseUUID(testWorkspaceID),
		util.MustParseUUID(pendingHolder),
		util.MustParseUUID(pendingHolderMemberRowID),
		util.MustParseUUID(testUserID),
	); err != nil {
		t.Fatalf("revokeAndRemoveMember(pendingHolder): %v", err)
	}

	var pendingHolderStatus string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM project_presenter_grant WHERE project_id = $1 AND user_id = $2`,
		secondProjectID, pendingHolder,
	).Scan(&pendingHolderStatus); err != nil {
		t.Fatalf("query pending holder grant: %v", err)
	}
	if pendingHolderStatus != "rejected" {
		t.Fatalf("pending grant status after member removal = %q, want rejected", pendingHolderStatus)
	}

	// untouchedHolder's unrelated pending request on the first project must
	// survive both removals above — scoping must be per-user, not per-grant-table.
	var untouchedStatus string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM project_presenter_grant WHERE project_id = $1 AND user_id = $2`,
		projectID, untouchedHolder,
	).Scan(&untouchedStatus); err != nil {
		t.Fatalf("query untouched holder grant: %v", err)
	}
	if untouchedStatus != "pending" {
		t.Fatalf("unrelated member's grant status = %q, want pending (untouched)", untouchedStatus)
	}
}
