package handler

// AIFIRST: CR-2026-048 TASK-07/08: publish gate wiring, appeal flow, market read.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func marketFrontmatter(name, extra string) string {
	return fmt.Sprintf(`---
name: %s
description: Skill for market tests.
applicable-scenarios: "testing"
context-dependencies: "none"
permission-declaration: "read specs/**"
failure-handling: "fail -> blockers[]"
%s---
# body
`, name, extra)
}

func createSkillViaHandler(t *testing.T, name, content string) string {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/skills", CreateSkillRequest{
		Name:    name,
		Content: content,
	})
	rec := httptest.NewRecorder()
	testHandler.CreateSkill(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create skill: %d: %s", rec.Code, rec.Body.String())
	}
	var resp SkillWithFilesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, resp.ID) })
	return resp.ID
}

func updateSkillVisibility(t *testing.T, skillID, visibility string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPut, "/api/skills/"+skillID, UpdateSkillRequest{Visibility: strPtr(visibility)})
	req = withURLParam(req, "id", skillID)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	return rec
}

func TestUpdateSkillRejectsBlockedPublishAndKeepsVisibility(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	content := marketFrontmatter("secret-skill", "") + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("A", 40) + "\n"
	id := createSkillViaHandler(t, "market-secret2-"+time.Now().Format("150405"), content)

	req := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{
		Visibility: strPtr("org"),
		OwnerActor: strPtr("Ray"),
	})
	req = withURLParam(req, "id", id)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code     string `json:"code"`
		Findings []struct {
			File      string `json:"file"`
			Line      int    `json:"line"`
			PatternID string `json:"pattern_id"`
			AppealID  string `json:"appeal_id"`
			Excerpt   string `json:"excerpt"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "skill_publish_blocked" || len(body.Findings) != 1 {
		t.Fatalf("body = %+v", body)
	}
	if body.Findings[0].PatternID != "github_token" || body.Findings[0].Line < 1 {
		t.Fatalf("finding = %+v", body.Findings[0])
	}
	if strings.Contains(body.Findings[0].Excerpt, "ghp_") {
		t.Fatal("finding excerpt leaks plaintext secret")
	}
	var vis string
	if err := testPool.QueryRow(context.Background(), `SELECT visibility FROM skill WHERE id = $1`, id).Scan(&vis); err != nil {
		t.Fatalf("read visibility: %v", err)
	}
	if vis != "private" {
		t.Fatalf("visibility = %q, want private (failed publish must not partially update)", vis)
	}
}

func TestUpdateSkillPublishesCleanSkill(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	id := createSkillViaHandler(t, "market-clean-"+time.Now().Format("150405"), marketFrontmatter("clean-skill", ""))

	req := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{
		Visibility: strPtr("org"),
		OwnerActor: strPtr("Ray"),
		Version:    strPtr("0.2.0"),
	})
	req = withURLParam(req, "id", id)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var vis, version string
	if err := testPool.QueryRow(context.Background(), `SELECT visibility, version FROM skill WHERE id = $1`, id).Scan(&vis, &version); err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if vis != "org" || version != "0.2.0" {
		t.Fatalf("visibility/version = %q/%q, want org/0.2.0", vis, version)
	}
}

func TestUpdateSkillRescansOrgContentAfterPublish(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	id := createSkillViaHandler(t, "market-rescan-"+time.Now().Format("150405"), marketFrontmatter("rescan-skill", ""))
	req0 := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{Visibility: strPtr("org"), OwnerActor: strPtr("Ray")})
	req0 = withURLParam(req0, "id", id)
	rec0 := httptest.NewRecorder()
	testHandler.UpdateSkill(rec0, req0)
	if rec0.Code != http.StatusOK {
		t.Fatalf("publish: %d: %s", rec0.Code, rec0.Body.String())
	}
	// Post-publish content update carrying a secret: visibility stays org,
	// no visibility field in the request, yet the gate must still fire.
	req := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{
		Content: strPtr(marketFrontmatter("rescan-skill", "") + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("A", 40) + "\n"),
	})
	req = withURLParam(req, "id", id)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("post-publish rescan: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppealFlowUnblocksPublishAndRejectsNonOwners(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	ctx := context.Background()
	content := marketFrontmatter("appeal-skill", "") + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("A", 40) + "\n"
	id := createSkillViaHandler(t, "market-appeal-"+time.Now().Format("150405"), content)

	// 1. Publish blocked; capture the finding's appeal id.
	req := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{Visibility: strPtr("org"), OwnerActor: strPtr("Ray")})
	req = withURLParam(req, "id", id)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("initial publish: %d: %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		Findings []struct {
			File      string `json:"file"`
			Line      int    `json:"line"`
			PatternID string `json:"pattern_id"`
			AppealID  string `json:"appeal_id"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &blocked); err != nil || len(blocked.Findings) != 1 {
		t.Fatalf("blocked body: %v err=%v", blocked, err)
	}
	appealID := blocked.Findings[0].AppealID

	// 2. Non-owner decide -> 403.
	memberID := createPermissionTestMember(t, fmt.Sprintf("appeal-member-%d@multica.test", time.Now().UnixNano()))
	decideReq := newRequest(http.MethodPost, "/api/skills/"+id+"/appeals/decide", decideSkillAppealRequest{AppealID: appealID, Approve: true})
	decideReq.Header.Set("X-User-ID", memberID)
	decideReq = withURLParam(decideReq, "id", id)
	decideRec := httptest.NewRecorder()
	testHandler.DecideSkillAppeal(decideRec, decideReq)
	if decideRec.Code != http.StatusForbidden {
		t.Fatalf("non-owner decide: expected 403, got %d: %s", decideRec.Code, decideRec.Body.String())
	}

	// 3. Author submits appeal -> 201; duplicate -> 200 no-op.
	submit := func() *httptest.ResponseRecorder {
		req := newRequest(http.MethodPost, "/api/skills/"+id+"/appeals", submitSkillAppealRequest{
			AppealID: appealID, File: blocked.Findings[0].File,
			Line: blocked.Findings[0].Line, PatternID: blocked.Findings[0].PatternID,
		})
		req = withURLParam(req, "id", id)
		rec := httptest.NewRecorder()
		testHandler.SubmitSkillAppeal(rec, req)
		return rec
	}
	if rec := submit(); rec.Code != http.StatusCreated {
		t.Fatalf("submit appeal: %d: %s", rec.Code, rec.Body.String())
	}
	if rec := submit(); rec.Code != http.StatusOK {
		t.Fatalf("duplicate submit: %d: %s", rec.Code, rec.Body.String())
	}
	var rows int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE action='skill_appeal_submitted' AND details->>'appeal_id'=$1`, appealID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("submitted rows = %d err=%v, want 1", rows, err)
	}

	// 4. Owner approves -> 200; republish passes.
	approveReq := newRequest(http.MethodPost, "/api/skills/"+id+"/appeals/decide", decideSkillAppealRequest{AppealID: appealID, Approve: true})
	approveReq = withURLParam(approveReq, "id", id)
	approveRec := httptest.NewRecorder()
	testHandler.DecideSkillAppeal(approveRec, approveReq)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("owner decide: %d: %s", approveRec.Code, approveRec.Body.String())
	}
	republishReq := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{Visibility: strPtr("org"), OwnerActor: strPtr("Ray")})
	republishReq = withURLParam(republishReq, "id", id)
	republishRec := httptest.NewRecorder()
	testHandler.UpdateSkill(republishRec, republishReq)
	if republishRec.Code != http.StatusOK {
		t.Fatalf("republish after approval: %d: %s", republishRec.Code, republishRec.Body.String())
	}
}

func TestGetSkillMarketScopesAndDedupes(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	ctx := context.Background()
	orgID := createSkillViaHandler(t, "market-list-"+time.Now().Format("150405"), marketFrontmatter("org-skill", ""))
	if rec := updateSkillVisibility(t, orgID, "org"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish without owner should be blocked, got %d", rec.Code)
	}
	req := newRequest(http.MethodPut, "/api/skills/"+orgID, UpdateSkillRequest{Visibility: strPtr("org"), OwnerActor: strPtr("Ray")})
	req = withURLParam(req, "id", orgID)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish org skill: %d: %s", rec.Code, rec.Body.String())
	}
	// Seed usage: one completed task claimed twice (dedupe -> 1), one failed task (excluded).
	agentID := createHandlerTestAgent(t, "market-usage-agent-"+time.Now().Format("150405"), nil)
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'market usage issue', 'in_progress', 'none', $2, 'member',
			(SELECT COALESCE(MAX(number), 82649) + 1 FROM issue WHERE workspace_id = $1), 0)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })
	var taskID, failedTaskID string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, issue_id, status, completed_at) VALUES ($1, $2, 'completed', now()) RETURNING id`, agentID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, issue_id, status, completed_at) VALUES ($1, $2, 'cancelled', now()) RETURNING id`, agentID, issueID).Scan(&failedTaskID); err != nil {
		t.Fatalf("seed failed task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM skill_usage_event WHERE task_id IN ($1, $2)`, taskID, failedTaskID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id IN ($1, $2)`, taskID, failedTaskID)
	})
	for i := 0; i < 2; i++ {
		if _, err := testPool.Exec(ctx, `INSERT INTO skill_usage_event (workspace_id, skill_ref, task_id) VALUES ($1, $2, $3)`, testWorkspaceID, orgID, taskID); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO skill_usage_event (workspace_id, skill_ref, task_id) VALUES ($1, $2, $3)`, testWorkspaceID, orgID, failedTaskID); err != nil {
		t.Fatalf("seed failed usage: %v", err)
	}

	marketReq := newRequest(http.MethodGet, "/api/skills/market", nil)
	marketRec := httptest.NewRecorder()
	testHandler.GetSkillMarket(marketRec, marketReq)
	if marketRec.Code != http.StatusOK {
		t.Fatalf("market: %d: %s", marketRec.Code, marketRec.Body.String())
	}
	var body SkillMarketResponse
	if err := json.Unmarshal(marketRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, s := range body.Workspace {
		if s.ID == orgID {
			found = true
			if s.UsageCount != 1 {
				t.Fatalf("usage = %d, want 1 (completed dedupe, failed excluded)", s.UsageCount)
			}
		}
	}
	if !found {
		t.Fatalf("org skill %s missing from market workspace list", orgID)
	}
	if len(body.Builtin) == 0 {
		t.Fatal("builtin list empty")
	}
}

// AIFIRST: CR-2026-048 review attempt 1 blocker 4: a later rejection must
// revoke an earlier approval, otherwise a released secret can never be pulled
// back (PRD AC-11).
func TestDecideSkillAppealRejectionRevokesEarlierApproval(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	content := marketFrontmatter("revoke-skill", "") + "\nexport GITHUB_TOKEN=ghp_" + strings.Repeat("B", 40) + "\n"
	id := createSkillViaHandler(t, "market-revoke-"+time.Now().Format("150405"), content)

	appealID := blockedAppealID(t, id)
	decideAppeal(t, id, appealID, true)
	if rec := publishOrg(t, id); rec.Code != http.StatusOK {
		t.Fatalf("publish after approval: %d: %s", rec.Code, rec.Body.String())
	}
	// Flip back to private so the publish gate runs again on the next attempt.
	if _, err := testPool.Exec(context.Background(), `UPDATE skill SET visibility = 'private' WHERE id = $1`, id); err != nil {
		t.Fatalf("reset visibility: %v", err)
	}
	decideAppeal(t, id, appealID, false)
	if rec := publishOrg(t, id); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish after rejection: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// AIFIRST: CR-2026-048 review attempt 1 blocker 5: a finding raised by the
// post-publish rescan is on content that is not in the database yet, so the
// appeal must be keyed by the id the gate handed out, not by a server-side
// rehash of the stored row.
func TestAppealReleasesRescanFindingOnUnsavedContent(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	id := createSkillViaHandler(t, "market-rescan-appeal-"+time.Now().Format("150405"), marketFrontmatter("rescan-appeal", ""))
	if rec := publishOrg(t, id); rec.Code != http.StatusOK {
		t.Fatalf("initial publish: %d: %s", rec.Code, rec.Body.String())
	}
	// New content with a false positive; the row still holds the clean body.
	newContent := marketFrontmatter("rescan-appeal", "") + "\ndocs live under C:\\Users\\alice\\notes\n"
	update := func() *httptest.ResponseRecorder {
		req := newRequest(http.MethodPut, "/api/skills/"+id, UpdateSkillRequest{Content: strPtr(newContent)})
		req = withURLParam(req, "id", id)
		rec := httptest.NewRecorder()
		testHandler.UpdateSkill(rec, req)
		return rec
	}
	rec := update()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rescan: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		Findings []struct {
			File      string `json:"file"`
			Line      int    `json:"line"`
			PatternID string `json:"pattern_id"`
			AppealID  string `json:"appeal_id"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &blocked); err != nil || len(blocked.Findings) != 1 {
		t.Fatalf("blocked body: %+v err=%v", blocked, err)
	}
	f := blocked.Findings[0]
	submitReq := newRequest(http.MethodPost, "/api/skills/"+id+"/appeals", submitSkillAppealRequest{
		AppealID: f.AppealID, File: f.File, Line: f.Line, PatternID: f.PatternID,
	})
	submitReq = withURLParam(submitReq, "id", id)
	submitRec := httptest.NewRecorder()
	testHandler.SubmitSkillAppeal(submitRec, submitReq)
	if submitRec.Code != http.StatusCreated {
		t.Fatalf("submit appeal on unsaved content: %d: %s", submitRec.Code, submitRec.Body.String())
	}
	// The ledger row must carry the very id the gate issued, otherwise the
	// owner decides on an id no submission is linked to.
	var rows int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM activity_log WHERE action='skill_appeal_submitted' AND details->>'appeal_id'=$1`, f.AppealID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("submitted rows for gate appeal id = %d err=%v, want 1", rows, err)
	}
	decideAppeal(t, id, f.AppealID, true)
	if rec := update(); rec.Code != http.StatusOK {
		t.Fatalf("rescan after approval: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// AIFIRST: CR-2026-048 TASK-01 acceptance 4 (AC-14): fixed fixture EXPLAIN
// showing each of the three new indexes is the access path its query needs.
// enable_seqscan is disabled for the duration because the fixture tables are
// too small for the planner to bother otherwise.
func TestSkillMarketQueriesUseNewIndexes(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	ctx := context.Background()
	cases := []struct {
		name  string
		query string
		index string
	}{
		{
			"usage ranking",
			`SELECT e.skill_ref, COUNT(DISTINCT e.task_id) FROM skill_usage_event e
			 JOIN agent_task_queue t ON t.id = e.task_id
			 WHERE e.workspace_id = '00000000-0000-0000-0000-000000000001' AND t.status = 'completed'
			 GROUP BY e.skill_ref`,
			"skill_usage_event_scope_idx",
		},
		{
			"completed task join",
			`SELECT 1 FROM skill_usage_event e JOIN agent_task_queue t ON t.id = e.task_id WHERE t.status = 'completed'`,
			"skill_usage_event_task_id_idx",
		},
		{
			"appeal lookup",
			`SELECT id FROM activity_log
			 WHERE workspace_id = '00000000-0000-0000-0000-000000000001'
			   AND action IN ('skill_appeal_approved', 'skill_appeal_rejected')
			   AND details->>'appeal_id' = 'deadbeef'
			 ORDER BY created_at DESC LIMIT 1`,
			"skill_appeal_activity_idx",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
				t.Fatalf("disable seqscan: %v", err)
			}
			var plan []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+tc.query).Scan(&plan); err != nil {
				t.Fatalf("explain: %v", err)
			}
			if !strings.Contains(string(plan), tc.index) {
				t.Fatalf("plan does not use %s:\n%s", tc.index, plan)
			}
		})
	}
}

// blockedAppealID publishes the skill, expects a single blocking finding and
// returns its appeal id.
func blockedAppealID(t *testing.T, skillID string) string {
	t.Helper()
	rec := publishOrg(t, skillID)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected blocked publish, got %d: %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		Findings []struct {
			AppealID string `json:"appeal_id"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &blocked); err != nil || len(blocked.Findings) != 1 {
		t.Fatalf("blocked body: %+v err=%v", blocked, err)
	}
	return blocked.Findings[0].AppealID
}

func publishOrg(t *testing.T, skillID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPut, "/api/skills/"+skillID, UpdateSkillRequest{Visibility: strPtr("org"), OwnerActor: strPtr("Ray")})
	req = withURLParam(req, "id", skillID)
	rec := httptest.NewRecorder()
	testHandler.UpdateSkill(rec, req)
	return rec
}

func decideAppeal(t *testing.T, skillID, appealID string, approve bool) {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/skills/"+skillID+"/appeals/decide", decideSkillAppealRequest{AppealID: appealID, Approve: approve})
	req = withURLParam(req, "id", skillID)
	rec := httptest.NewRecorder()
	testHandler.DecideSkillAppeal(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide(approve=%v): %d: %s", approve, rec.Code, rec.Body.String())
	}
}
