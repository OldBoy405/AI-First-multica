package handler

// AIFIRST: CR-2026-053 TASK-05 — task-scoped CR→Issue binding endpoint tests
// (AC-B1~B7, FR-B3 semantics). Runs against the shared handler test database;
// skips when unreachable (TestMain).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// crBindFixture seeds the rows a successful bind needs: agent → task (with
// issue+project context), project → issue, and the cr projection row. It
// returns {agentID, taskID, projectID, issueID, crID}.
func crBindFixture(t *testing.T) map[string]string {
	t.Helper()
	projectID := dbfx.Project(t, "cr-bind-project")
	issueID := dbfx.Issue(t, "cr-bind-issue", testutil.Cols{"project_id": projectID})
	agentID := dbfx.Agent(t, "cr-bind-agent", "")
	// cr_bind_test.go fix (review-code BLOCK-④): migration 251's
	// agent_task_queue_active_requires_runtime CHECK requires every queued row
	// to carry a runtime_id (or a terminal completed_at); the shared dbfx.Task
	// fixture does not set either, so the fixture explicitly stamps the handler
	// test runtime here instead of touching dbfx.Task.
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"issue_id":   issueID,
		"project_id": projectID,
	})
	crID := "CR-BIND-TEST-01"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID)
	return map[string]string{
		"agentID":   agentID,
		"taskID":    taskID,
		"projectID": projectID,
		"issueID":   issueID,
		"crID":      crID,
	}
}

// bindRequest builds a task-token-style POST for HandleBindCurrentTask.
func bindRequest(t *testing.T, taskID, agentID, workspaceID, crID string) *http.Request {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/crs/"+crID+"/bind-current-task", map[string]any{})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Workspace-ID", workspaceID)
	return withURLParam(req, "crID", crID)
}

func decodeError(t *testing.T, body *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return strings.TrimSpace(out["error"].(string))
}

func TestBindCurrentTaskSuccess(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["cr_id"] != fx["crID"] || out["task_id"] != fx["taskID"] || out["issue_id"] != fx["issueID"] || out["project_id"] != fx["projectID"] || out["changed"] != true {
		t.Errorf("unexpected response: %v", out)
	}

	// AC-B2: all three writes landed — task.cr_id, cr.shell_issue_id, audit.
	var taskCR pgtype.Text
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, fx["taskID"]).Scan(&taskCR)
	if taskCR.String != fx["crID"] {
		t.Errorf("task.cr_id = %q, want %q", taskCR.String, fx["crID"])
	}
	var shellIssue pgtype.UUID
	dbfx.QueryRow(t, `SELECT shell_issue_id FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, fx["crID"]).Scan(&shellIssue)
	if !shellIssue.Valid || shellIssue.String() != fx["issueID"] {
		t.Errorf("cr.shell_issue_id = %v, want %s", shellIssue.String(), fx["issueID"])
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'cr_issue_bound' AND issue_id = $2::uuid`, testWorkspaceID, fx["issueID"]); n != 1 {
		t.Errorf("cr_issue_bound audit rows = %d, want 1", n)
	}
}

func TestBindCurrentTaskReplayChangedFalse(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)

	// AC-B3 / review-code BLOCK-① regression: cr:updated refresh events must
	// be emitted only when the bind changed state (changed=true); the
	// same-value replay (changed=false) must publish no event.
	var updated []events.Event
	testHandler.Bus.Subscribe(protocol.EventCRUpdated, func(e events.Event) {
		updated = append(updated, e)
	})

	for i := 0; i < 2; i++ {
		updated = nil
		w := httptest.NewRecorder()
		testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
		if w.Code != http.StatusOK {
			t.Fatalf("replay %d: status = %d, body=%s", i, w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		// AC-B3: same-value replay → changed=false, binding intact.
		if i == 1 && out["changed"] != false {
			t.Errorf("replay changed = %v, want false", out["changed"])
		}
		// First bind changed=true → exactly one cr:updated refresh event.
		if i == 0 && len(updated) != 1 {
			t.Errorf("first bind published %d cr:updated events, want 1", len(updated))
		}
		// Replay changed=false → no refresh event (SDD §4.1 / AC-B3).
		if i == 1 && len(updated) != 0 {
			t.Errorf("replay published %d cr:updated events, want 0 (changed=false must not refresh)", len(updated))
		}
	}
	// No duplicate success audit on replay.
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'cr_issue_bound' AND issue_id = $2::uuid`, testWorkspaceID, fx["issueID"]); n != 1 {
		t.Errorf("cr_issue_bound audit rows after replay = %d, want 1", n)
	}
}

func TestBindCurrentTaskRequiresTaskToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	req := newRequest(http.MethodPost, "/api/crs/"+fx["crID"]+"/bind-current-task", map[string]any{})
	req.Header.Set("X-Task-ID", fx["taskID"])
	req.Header.Set("X-Agent-ID", fx["agentID"])
	req.Header.Set("X-Workspace-ID", testWorkspaceID) // no X-Actor-Source
	req = withURLParam(req, "crID", fx["crID"])

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := decodeError(t, w); got != "TASK_CONTEXT_REQUIRED" {
		t.Errorf("error = %q, want TASK_CONTEXT_REQUIRED", got)
	}
	var taskCR pgtype.Text
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, fx["taskID"]).Scan(&taskCR)
	if taskCR.Valid {
		t.Error("zero-binding-write violated: task.cr_id set without task token")
	}
}

func TestBindCurrentTaskTaskWithoutIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := dbfx.Agent(t, "cr-bind-agent-noissue", "")
	// runtime_id stamped explicitly (migration 251 CHECK; see crBindFixture).
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t)}) // issue_id NULL
	crID := "CR-BIND-TEST-NOISSUE"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID)

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, taskID, agentID, testWorkspaceID, crID))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if got := decodeError(t, w); got != "TASK_ISSUE_REQUIRED" {
		t.Errorf("error = %q, want TASK_ISSUE_REQUIRED", got)
	}
	// AC-B4: zero binding writes.
	var shellIssue pgtype.UUID
	dbfx.QueryRow(t, `SELECT shell_issue_id FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, crID).Scan(&shellIssue)
	if shellIssue.Valid {
		t.Error("zero-binding-write violated: cr.shell_issue_id set")
	}
}

func TestBindCurrentTaskProjectMismatch(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	otherProjectID := dbfx.Project(t, "cr-bind-other-project")
	dbfx.Exec(t, `UPDATE agent_task_queue SET project_id = $1::uuid WHERE id = $2::uuid`, otherProjectID, fx["taskID"])

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if got := decodeError(t, w); got != "TASK_PROJECT_MISMATCH" {
		t.Errorf("error = %q, want TASK_PROJECT_MISMATCH", got)
	}
	var taskCR pgtype.Text
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, fx["taskID"]).Scan(&taskCR)
	if taskCR.Valid {
		t.Error("zero-binding-write violated: task.cr_id set on project mismatch")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'cr_issue_bound' AND issue_id = $2::uuid`, testWorkspaceID, fx["issueID"]); n != 0 {
		t.Fatalf("cr_issue_bound audit rows = %d, want 0", n)
	}
}

func TestBindCurrentTaskAuditFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	suffix := strings.ReplaceAll(fx["issueID"], "-", "")[:12]
	functionName := "fail_cr_bind_audit_" + suffix
	triggerName := "fail_cr_bind_audit_" + suffix
	dbfx.Cleanup(t, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, functionName))
	dbfx.Cleanup(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON activity_log`, triggerName))
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected cr bind audit failure';
		END;
		$$`, functionName))
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE INSERT ON activity_log
		FOR EACH ROW WHEN (NEW.action = 'cr_issue_bound' AND NEW.issue_id = '%s'::uuid)
		EXECUTE FUNCTION %s()`, triggerName, fx["issueID"], functionName))

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if got := decodeError(t, w); got != "CR_BIND_FAILED" {
		t.Errorf("error = %q, want CR_BIND_FAILED", got)
	}
	var taskCR pgtype.Text
	var shellIssue pgtype.UUID
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, fx["taskID"]).Scan(&taskCR)
	dbfx.QueryRow(t, `SELECT shell_issue_id FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, fx["crID"]).Scan(&shellIssue)
	if taskCR.Valid || shellIssue.Valid {
		t.Fatalf("audit failure left partial binding: task_cr=%v shell_issue=%v", taskCR, shellIssue)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'cr_issue_bound' AND issue_id = $2::uuid`, testWorkspaceID, fx["issueID"]); n != 0 {
		t.Fatalf("cr_issue_bound audit rows = %d, want 0", n)
	}
}

func TestBindCurrentTaskTaskCRConflict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	otherCR := "CR-BIND-TEST-OTHER"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, otherCR)
	// Task already bound to another CR (AC-B5).
	dbfx.Exec(t, `UPDATE agent_task_queue SET cr_id = $1 WHERE id = $2::uuid`, otherCR, fx["taskID"])

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := decodeError(t, w); got != "TASK_CR_CONFLICT" {
		t.Errorf("error = %q, want TASK_CR_CONFLICT", got)
	}
	// Zero overwrite + rejected audit (FR-B4/AC-B7).
	var taskCR pgtype.Text
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, fx["taskID"]).Scan(&taskCR)
	if taskCR.String != otherCR {
		t.Errorf("task.cr_id = %q, want unchanged %q", taskCR.String, otherCR)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE workspace_id = $1::uuid AND action = 'cr_issue_bind_rejected'`, testWorkspaceID); n == 0 {
		t.Error("expected cr_issue_bind_rejected audit row")
	}
}

func TestBindCurrentTaskCRIssueConflict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	otherIssue := dbfx.Issue(t, "cr-bind-other-issue", testutil.Cols{"project_id": fx["projectID"]})
	// CR already bound to another issue (AC-B5).
	dbfx.Exec(t, `UPDATE cr SET shell_issue_id = $1::uuid WHERE workspace_id = $2::uuid AND cr_id = $3`,
		otherIssue, testWorkspaceID, fx["crID"])

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := decodeError(t, w); got != "CR_ISSUE_CONFLICT" {
		t.Errorf("error = %q, want CR_ISSUE_CONFLICT", got)
	}
	var shellIssue pgtype.UUID
	dbfx.QueryRow(t, `SELECT shell_issue_id FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, fx["crID"]).Scan(&shellIssue)
	if !shellIssue.Valid || shellIssue.String() != otherIssue {
		t.Errorf("cr.shell_issue_id = %v, want unchanged %s", shellIssue.String(), otherIssue)
	}
}

func TestBindCurrentTaskCRNotFound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := crBindFixture(t)
	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, "CR-NO-SUCH-CR"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := decodeError(t, w); got != "CR_NOT_FOUND" {
		t.Errorf("error = %q, want CR_NOT_FOUND", got)
	}
}

func TestBindCurrentTaskCrossWorkspaceToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// Agent lives in a different workspace than the token claims (AC-B4).
	otherWS := dbfx.Workspace(t, "cr-bind-other-ws", "cr-bind-other-ws")
	projectID := dbfx.Project(t, "cr-bind-project-xws", testutil.Cols{"workspace_id": otherWS})
	issueID := dbfx.Issue(t, "cr-bind-issue-xws", testutil.Cols{"workspace_id": otherWS, "project_id": projectID})
	agentID := dbfx.Agent(t, "cr-bind-agent-xws", "", testutil.Cols{"workspace_id": otherWS})
	// runtime_id stamped explicitly (migration 251 CHECK; see crBindFixture).
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t), "issue_id": issueID, "project_id": projectID})
	crID := "CR-BIND-TEST-XWS"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, otherWS, crID)

	// Token claims testWorkspaceID but the agent row belongs to otherWS.
	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, taskID, agentID, testWorkspaceID, crID))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var taskCR pgtype.Text
	dbfx.QueryRow(t, `SELECT cr_id FROM agent_task_queue WHERE id = $1::uuid`, taskID).Scan(&taskCR)
	if taskCR.Valid {
		t.Error("zero-binding-write violated on cross-workspace token")
	}
}

func TestBindCurrentTaskPartialBoundChangedTrue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// task already has cr_id but the CR lacks shell_issue_id → still binds the
	// other half with changed=true (SDD §4.1 partial-combination coverage).
	fx := crBindFixture(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET cr_id = $1 WHERE id = $2::uuid`, fx["crID"], fx["taskID"])

	w := httptest.NewRecorder()
	testHandler.HandleBindCurrentTask(w, bindRequest(t, fx["taskID"], fx["agentID"], testWorkspaceID, fx["crID"]))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["changed"] != true {
		t.Errorf("changed = %v, want true (cr.shell_issue_id was still NULL)", out["changed"])
	}
	var shellIssue pgtype.UUID
	dbfx.QueryRow(t, `SELECT shell_issue_id FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, fx["crID"]).Scan(&shellIssue)
	if !shellIssue.Valid || shellIssue.String() != fx["issueID"] {
		t.Errorf("cr.shell_issue_id = %v, want %s", shellIssue.String(), fx["issueID"])
	}
}
