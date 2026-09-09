package handler

// AIFIRST: CR-2026-061 TASK-03 — promotion endpoint + bind-promotion-run
// endpoint tests (AC-2/AC-5/AC-6/AC-11/AC-13 error matrices). DB-backed tests
// run against the shared handler test database and skip wholesale when it is
// unreachable (TestMain in handler_test.go).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// ── pure shape validator ──

func promoReq() PromoteProjectDiscussionRequest {
	return PromoteProjectDiscussionRequest{
		SessionID:   "00000000-0000-0000-0000-0000000000b1",
		MessageIDs:  []string{"11111111-1111-1111-1111-111111111111"},
		UpgradeToCR: boolPtr(true),
	}
}

func boolPtr(b bool) *bool { return &b }

func TestValidatePromotionRequestShape(t *testing.T) {
	valid := promoReq()
	if code, ok := validatePromotionRequest(&valid); !ok {
		t.Fatalf("valid request rejected: %s", code)
	}

	cases := []struct {
		name   string
		mutate func(*PromoteProjectDiscussionRequest)
		code   string
	}{
		{"missing session", func(r *PromoteProjectDiscussionRequest) { r.SessionID = "" }, "invalid_promotion_selection"},
		{"bad session", func(r *PromoteProjectDiscussionRequest) { r.SessionID = "nope" }, "invalid_promotion_selection"},
		{"bad message uuid", func(r *PromoteProjectDiscussionRequest) { r.MessageIDs = []string{"x"} }, "invalid_promotion_selection"},
		{"duplicate message", func(r *PromoteProjectDiscussionRequest) {
			r.MessageIDs = []string{"11111111-1111-1111-1111-111111111111", "11111111-1111-1111-1111-111111111111"}
		}, "invalid_promotion_selection"},
		{"duplicate across kinds", func(r *PromoteProjectDiscussionRequest) {
			r.AttachmentIDs = []string{"11111111-1111-1111-1111-111111111111"}
		}, "invalid_promotion_selection"},
		{"both empty", func(r *PromoteProjectDiscussionRequest) { r.MessageIDs = nil }, "invalid_promotion_selection"},
		{"title too long", func(r *PromoteProjectDiscussionRequest) {
			r.Title = strPtr(strings.Repeat("t", 201))
		}, "invalid_promotion_title"},
		{"description too long", func(r *PromoteProjectDiscussionRequest) {
			r.Description = strPtr(strings.Repeat("d", 10001))
		}, "invalid_promotion_description"},
		{"attachment only ok", func(r *PromoteProjectDiscussionRequest) {
			r.MessageIDs = nil
			r.AttachmentIDs = []string{"33333333-3333-3333-3333-333333333333"}
		}, ""},
	}
	for _, tc := range cases {
		req := promoReq()
		tc.mutate(&req)
		code, ok := validatePromotionRequest(&req)
		if tc.code == "" {
			if !ok {
				t.Errorf("%s: rejected with %s, want ok", tc.name, code)
			}
			continue
		}
		if ok || code != tc.code {
			t.Errorf("%s: code=%q ok=%v, want code=%q ok=false", tc.name, code, ok, tc.code)
		}
	}
}

func TestParseIssueContextRefs(t *testing.T) {
	good := []byte(`[{"kind":"discussion_promotion","session_id":"00000000-0000-0000-0000-00000000000b","message_ids":["11111111-1111-1111-1111-111111111111"],"attachment_ids":[],"dedupe_key":"abc","promoted_by":"00000000-0000-0000-0000-00000000000a","promoted_at":"2026-09-08T00:00:00Z","pipeline_run_id":"22222222-2222-2222-2222-222222222222"}]`)
	entries := parseIssueContextRefs(good)
	if len(entries) != 1 || entries[0].Kind == nil || *entries[0].Kind != "discussion_promotion" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].PipelineRunID == nil || *entries[0].PipelineRunID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("pipeline_run_id = %v", entries[0].PipelineRunID)
	}
	if got := parseIssueContextRefs(nil); got != nil {
		t.Fatalf("nil raw = %v, want nil", got)
	}
	if got := parseIssueContextRefs([]byte(`{"not":"an array"}`)); got != nil {
		t.Fatalf("malformed raw = %v, want nil (degrade)", got)
	}
}

// ── DB-backed endpoint tests ──

// promoFixture seeds project + shared session + one authored message and
// returns the ids as strings.
type promoHandlerFixture struct {
	ProjectID string
	SessionID string
	MessageID string
}

func seedPromoHandlerFixture(t *testing.T) promoHandlerFixture {
	t.Helper()
	projectID := dbfx.Project(t, "promotion-handler-project")
	sessionID := dbfx.ChatSession(t, "", testutil.Cols{
		"agent_id":   testutil.Raw("NULL"),
		"project_id": projectID,
		"kind":       "project_shared",
	})
	messageID := dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": sessionID,
		"role":            "user",
		"content":         "promotion source message",
		"author_type":     "member",
		"author_id":       testUserID,
	})
	return promoHandlerFixture{ProjectID: projectID, SessionID: sessionID, MessageID: messageID}
}

func promoRequest(t *testing.T, fx promoHandlerFixture, body map[string]any, idempotencyKey string) *http.Request {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/projects/"+fx.ProjectID+"/discussion/promote", body)
	req = withURLParam(req, "id", fx.ProjectID)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return out
}

func decodeResponseCode(t *testing.T, w *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	out := decodeResponse(t, w)
	return w.Code, fmt.Sprint(out["code"])
}

func TestPromoteProjectDiscussionCreatesIssueAndRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := seedPromoHandlerFixture(t)
	body := map[string]any{
		"session_id":    fx.SessionID,
		"message_ids":   []string{fx.MessageID},
		"upgrade_to_cr": true,
	}
	w := httptest.NewRecorder()
	testHandler.HandlePromoteProjectDiscussion(w, promoRequest(t, fx, body, "promo-h-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	out := decodeResponse(t, w)
	if out["created"] != true || out["upgrade_to_cr"] != true || out["run_id"] == nil {
		t.Fatalf("response = %v", out)
	}
	issueID := fmt.Sprint(out["issue_id"])
	runID := fmt.Sprint(out["run_id"])

	// AC-1/AC-3: one issue with the promotion entry.
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1::uuid`, issueID); n != 1 {
		t.Fatalf("issue rows = %d, want 1", n)
	}
	var refs []byte
	dbfx.QueryRow(t, `SELECT context_refs FROM issue WHERE id = $1::uuid`, issueID).Scan(&refs)
	if !strings.Contains(string(refs), `"kind": "discussion_promotion"`) && !strings.Contains(string(refs), `"kind":"discussion_promotion"`) {
		t.Fatalf("context_refs missing promotion entry: %s", refs)
	}
	if !strings.Contains(string(refs), runID) {
		t.Fatalf("context_refs missing pipeline_run_id %s: %s", runID, refs)
	}

	// AC-8: pre-built run + first node, no agent_task_queue.
	var runCR, runStatus, runPipeline string
	dbfx.QueryRow(t, `SELECT COALESCE(cr_id::text,''), status, pipeline_id FROM pipeline_run WHERE id = $1::uuid`, runID).
		Scan(&runCR, &runStatus, &runPipeline)
	if runCR != "" || runStatus != "running" || runPipeline != "requirement-authoring" {
		t.Fatalf("run = cr=%q status=%q pipeline=%q", runCR, runStatus, runPipeline)
	}
	var nodeID, nodeStatus string
	var seq int
	dbfx.QueryRow(t, `SELECT node_id::text, status, seq FROM pipeline_node_run WHERE run_id = $1::uuid`, runID).
		Scan(&nodeID, &nodeStatus, &seq)
	if nodeID != "00000000-0000-0000-0011-000000000001" || nodeStatus != "running" || seq != 1 {
		t.Fatalf("node = id=%q status=%q seq=%d", nodeID, nodeStatus, seq)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE pipeline_node_run_id IN (SELECT id FROM pipeline_node_run WHERE run_id = $1::uuid)`, runID); n != 0 {
		t.Fatalf("agent_task_queue rows = %d, want 0", n)
	}

	// AC-5 replay: same key + same body → created=false, same issue/run.
	w2 := httptest.NewRecorder()
	testHandler.HandlePromoteProjectDiscussion(w2, promoRequest(t, fx, body, "promo-h-1"))
	if w2.Code != http.StatusCreated {
		t.Fatalf("replay status = %d; body=%s", w2.Code, w2.Body.String())
	}
	out2 := decodeResponse(t, w2)
	if out2["created"] != false || fmt.Sprint(out2["issue_id"]) != issueID || fmt.Sprint(out2["run_id"]) != runID {
		t.Fatalf("replay response = %v", out2)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1::uuid`, issueID); n != 1 {
		t.Fatalf("issue rows after replay = %d, want 1", n)
	}
}

func TestPromoteProjectDiscussionSelectionAndErrorMatrix(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := seedPromoHandlerFixture(t)
	before := dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1::uuid`, testWorkspaceID)

	cases := []struct {
		name string
		body map[string]any
		key  string
		code int
		want string
	}{
		{"missing key", map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID}}, "", 400, "idempotency_key_required"},
		{"both empty", map[string]any{"session_id": fx.SessionID}, "k-2", 400, "invalid_promotion_selection"},
		{"duplicate message", map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID, fx.MessageID}}, "k-3", 400, "invalid_promotion_selection"},
		{"session mismatch", map[string]any{"session_id": "99999999-9999-9999-9999-999999999999", "message_ids": []string{fx.MessageID}}, "k-4", 404, "chat_session_not_found"},
		{"draft attachment", map[string]any{"session_id": fx.SessionID, "attachment_ids": []string{"55555555-5555-5555-5555-555555555555"}}, "k-5", 400, "invalid_promotion_selection"},
		{"title too long", map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID}, "title": strings.Repeat("t", 201)}, "k-6", 400, "invalid_promotion_title"},
		{"description too long", map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID}, "description": strings.Repeat("d", 10001)}, "k-7", 400, "invalid_promotion_description"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		testHandler.HandlePromoteProjectDiscussion(w, promoRequest(t, fx, tc.body, tc.key))
		if w.Code != tc.code {
			t.Errorf("%s: status = %d, want %d; body=%s", tc.name, w.Code, tc.code, w.Body.String())
			continue
		}
		if tc.want != "" {
			_, code := decodeResponseCode(t, w)
			if code != tc.want {
				t.Errorf("%s: code = %q, want %q", tc.name, code, tc.want)
			}
		}
	}

	// project not found
	w := httptest.NewRecorder()
	req := promoRequest(t, fx, map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID}}, "k-8")
	req = withURLParam(req, "id", "00000000-0000-0000-0000-00000000dead")
	testHandler.HandlePromoteProjectDiscussion(w, req)
	if w.Code != 404 {
		t.Errorf("project not found: status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	_, code := decodeResponseCode(t, w)
	if code != "project_not_found" {
		t.Errorf("project not found: code = %q", code)
	}

	// invalid JSON body → invalid_request_body
	w = httptest.NewRecorder()
	badReq := newRequest(http.MethodPost, "/api/projects/"+fx.ProjectID+"/discussion/promote", "not-json")
	badReq = withURLParam(badReq, "id", fx.ProjectID)
	badReq.Header.Set("Idempotency-Key", "k-9")
	testHandler.HandlePromoteProjectDiscussion(w, badReq)
	if w.Code != 400 {
		t.Errorf("bad json: status = %d, want 400", w.Code)
	}
	_, code = decodeResponseCode(t, w)
	if code != "invalid_request_body" {
		t.Errorf("bad json: code = %q", code)
	}

	// upgrade_to_cr type error → invalid_request_body
	w = httptest.NewRecorder()
	typeErrReq := newRequest(http.MethodPost, "/api/projects/"+fx.ProjectID+"/discussion/promote",
		map[string]any{"session_id": fx.SessionID, "message_ids": []string{fx.MessageID}, "upgrade_to_cr": "yes"})
	typeErrReq = withURLParam(typeErrReq, "id", fx.ProjectID)
	typeErrReq.Header.Set("Idempotency-Key", "k-10")
	testHandler.HandlePromoteProjectDiscussion(w, typeErrReq)
	if w.Code != 400 {
		t.Errorf("upgrade type error: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	_, code = decodeResponseCode(t, w)
	if code != "invalid_request_body" {
		t.Errorf("upgrade type error: code = %q", code)
	}

	// AC-11: all the above wrote nothing.
	if after := dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1::uuid`, testWorkspaceID); after != before {
		t.Errorf("issue rows changed %d → %d on error matrix", before, after)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_idempotency WHERE scope_type = 'discussion_promotion' AND scope_id = $1::uuid`, fx.ProjectID); n != 0 {
		t.Errorf("idempotency rows = %d, want 0", n)
	}
}

func TestPromoteProjectDiscussionForbiddenNonMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := seedPromoHandlerFixture(t)
	outsiderID := dbfx.User(t, "Promotion Outsider", "promotion-outsider@multica.test")
	before := dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1::uuid`, testWorkspaceID)

	w := httptest.NewRecorder()
	req := newRequestAs(outsiderID, http.MethodPost, "/api/projects/"+fx.ProjectID+"/discussion/promote", map[string]any{
		"session_id":  fx.SessionID,
		"message_ids": []string{fx.MessageID},
	})
	req = withURLParam(req, "id", fx.ProjectID)
	req.Header.Set("Idempotency-Key", "promo-out-1")
	testHandler.HandlePromoteProjectDiscussion(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	_, code := decodeResponseCode(t, w)
	if code != "forbidden_promotion" {
		t.Fatalf("code = %q, want forbidden_promotion", code)
	}
	if after := dbfx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1::uuid`, testWorkspaceID); after != before {
		t.Fatalf("issue rows changed %d → %d (zero writes)", before, after)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_idempotency WHERE scope_type = 'discussion_promotion' AND scope_id = $1::uuid`, fx.ProjectID); n != 0 {
		t.Fatalf("idempotency rows = %d, want 0", n)
	}
}

// ── bind-promotion-run endpoint ──

type bindPromoFixture struct {
	IssueID string
	RunID   string
	NodeID  string
	CRID    string
}

func seedBindPromoFixture(t *testing.T, crID string) bindPromoFixture {
	t.Helper()
	issueID := dbfx.Issue(t, "bind-promotion-issue", testutil.Cols{})
	runID := dbfx.Insert(t, "pipeline_run", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"pipeline_id":       "requirement-authoring",
		"issue_id":          issueID,
		"status":            "running",
		"inputs":            "{}",
		"execution_context": "{}",
		"started_by":        testUserID,
	})
	nodeID := dbfx.Insert(t, "pipeline_node_run", testutil.Cols{
		"run_id":  runID,
		"node_id": "00000000-0000-0000-0011-000000000001",
		"ref":     "requirement-register",
		"kind":    "skill",
		"seq":     1,
		"status":  "running",
		"attempt": 1,
	})
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID)
	return bindPromoFixture{IssueID: issueID, RunID: runID, NodeID: nodeID, CRID: crID}
}

func bindPromoRequest(t *testing.T, crID, runID string) *http.Request {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/crs/"+crID+"/bind-promotion-run", map[string]any{"run_id": runID})
	req.Header.Set("X-Actor-Source", "task_token")
	return withURLParam(req, "crID", crID)
}

func TestBindPromotionRunEndpoint(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	// Non-task-token → 401 with the bind-family {"error":...} shape.
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/crs/CR-PROMO-01/bind-promotion-run", map[string]any{"run_id": "11111111-1111-1111-1111-111111111111"})
	req = withURLParam(req, "crID", "CR-PROMO-01")
	testHandler.HandleBindPromotionRun(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-task-token status = %d, want 401", w.Code)
	}
	if errCode := decodeError(t, w); errCode != "TASK_CONTEXT_REQUIRED" {
		t.Fatalf("non-task-token error = %q", errCode)
	}

	// Invalid run_id → 400 INVALID_RUN_ID.
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, "CR-PROMO-01", "not-a-uuid"))
	if w.Code != http.StatusBadRequest || decodeError(t, w) != "INVALID_RUN_ID" {
		t.Fatalf("invalid run id: status=%d error=%q", w.Code, decodeError(t, w))
	}

	// Unknown run → 404 RUN_NOT_FOUND.
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, "CR-PROMO-01", "11111111-1111-1111-1111-111111111111"))
	if w.Code != http.StatusNotFound || decodeError(t, w) != "RUN_NOT_FOUND" {
		t.Fatalf("unknown run: status=%d error=%q", w.Code, decodeError(t, w))
	}

	// Run exists but CR missing → 404 CR_NOT_FOUND.
	fx := seedBindPromoFixture(t, "CR-PROMO-01")
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, "CR-PROMO-NOPE", fx.RunID))
	if w.Code != http.StatusNotFound || decodeError(t, w) != "CR_NOT_FOUND" {
		t.Fatalf("missing cr: status=%d error=%q", w.Code, decodeError(t, w))
	}

	// Success.
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, fx.CRID, fx.RunID))
	if w.Code != http.StatusOK {
		t.Fatalf("bind status = %d; body=%s", w.Code, w.Body.String())
	}
	out := decodeResponse(t, w)
	if out["cr_id"] != fx.CRID || fmt.Sprint(out["run_id"]) != fx.RunID || fmt.Sprint(out["issue_id"]) != fx.IssueID || out["changed"] != true {
		t.Fatalf("bind response = %v", out)
	}
	var runCR string
	dbfx.QueryRow(t, `SELECT cr_id FROM pipeline_run WHERE id = $1::uuid`, fx.RunID).Scan(&runCR)
	if runCR != fx.CRID {
		t.Fatalf("run.cr_id = %q, want %q", runCR, fx.CRID)
	}
	var shellIssue string
	dbfx.QueryRow(t, `SELECT COALESCE(shell_issue_id::text,'') FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, fx.CRID).Scan(&shellIssue)
	if shellIssue != fx.IssueID {
		t.Fatalf("cr.shell_issue_id = %q, want %q", shellIssue, fx.IssueID)
	}
	var nodeStatus string
	dbfx.QueryRow(t, `SELECT status FROM pipeline_node_run WHERE id = $1::uuid`, fx.NodeID).Scan(&nodeStatus)
	if nodeStatus != "passed" {
		t.Fatalf("first node status = %q, want passed (D-5)", nodeStatus)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE action = 'promotion_run_bound' AND issue_id = $1::uuid`, fx.IssueID); n != 1 {
		t.Fatalf("promotion_run_bound audit rows = %d, want 1", n)
	}

	// Replay → changed=false.
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, fx.CRID, fx.RunID))
	if w.Code != http.StatusOK {
		t.Fatalf("replay status = %d; body=%s", w.Code, w.Body.String())
	}
	out = decodeResponse(t, w)
	if out["changed"] != false {
		t.Fatalf("replay changed = %v, want false", out["changed"])
	}

	// Second bind to a DIFFERENT CR → 409 RUN_CR_CONFLICT.
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, "CR-PROMO-02")
	w = httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, "CR-PROMO-02", fx.RunID))
	if w.Code != http.StatusConflict || decodeError(t, w) != "RUN_CR_CONFLICT" {
		t.Fatalf("second bind: status=%d error=%q", w.Code, decodeError(t, w))
	}
}

func TestBindPromotionRunCRIssueConflict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	otherIssue := dbfx.Issue(t, "bind-promotion-other-issue", testutil.Cols{})
	fx := seedBindPromoFixture(t, "CR-PROMO-03")
	dbfx.Exec(t, `UPDATE cr SET shell_issue_id = $1::uuid WHERE workspace_id = $2::uuid AND cr_id = $3`,
		otherIssue, testWorkspaceID, fx.CRID)
	w := httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, fx.CRID, fx.RunID))
	if w.Code != http.StatusConflict || decodeError(t, w) != "CR_ISSUE_CONFLICT" {
		t.Fatalf("status=%d error=%q, want 409 CR_ISSUE_CONFLICT", w.Code, decodeError(t, w))
	}
	// Zero writes.
	var runCR string
	dbfx.QueryRow(t, `SELECT COALESCE(cr_id::text,'') FROM pipeline_run WHERE id = $1::uuid`, fx.RunID).Scan(&runCR)
	if runCR != "" {
		t.Fatalf("run.cr_id = %q, want empty on conflict", runCR)
	}
}

func TestBindPromotionRunNonPromotionShapeNotFound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// A run for a different pipeline is indistinguishable from absence.
	otherRun := dbfx.Insert(t, "pipeline_run", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"pipeline_id":       "code-implementation",
		"issue_id":          dbfx.Issue(t, "bind-promotion-shape-issue", testutil.Cols{}),
		"status":            "running",
		"inputs":            "{}",
		"execution_context": "{}",
		"started_by":        testUserID,
	})
	w := httptest.NewRecorder()
	testHandler.HandleBindPromotionRun(w, bindPromoRequest(t, "CR-PROMO-04", otherRun))
	if w.Code != http.StatusNotFound || decodeError(t, w) != "RUN_NOT_FOUND" {
		t.Fatalf("status=%d error=%q, want 404 RUN_NOT_FOUND", w.Code, decodeError(t, w))
	}
}
