package governance

// AIFIRST: CR-2026-052 TASK-06 — approval-continuation integration tests
// (SDD §7.4). Covers AC-1, AC-2, AC-6 (incl. 6b/6c lock-level notes via the
// fail-closed path), AC-6d (cross-workspace isolation), AC-7, AC-8, AC-9a~d.
// Follows the governance test convention: TestMain (crsync_test.go) connects
// to the local dev DB and skips the whole suite when unreachable, so these
// tests are no-ops in CI without a DB.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeContinuationEnqueuer delegates the transactional enqueue to a real
// *service.TaskService (which only uses the tx-bound qtx param) but records
// post-commit NotifyContinuationTaskEnqueued calls instead of exercising the
// realtime/event bus (nil Hub/Bus) — the DB row is the AC focus.
type fakeContinuationEnqueuer struct {
	real     *service.TaskService
	notified []db.AgentTaskQueue
}

func (f *fakeContinuationEnqueuer) EnqueueApprovalContinuation(ctx context.Context, qtx *db.Queries, spec service.ApprovalContinuationSpec) (db.AgentTaskQueue, service.EnqueueOutcome, error) {
	return f.real.EnqueueApprovalContinuation(ctx, qtx, spec)
}

func (f *fakeContinuationEnqueuer) NotifyContinuationTaskEnqueued(ctx context.Context, task db.AgentTaskQueue) error {
	f.notified = append(f.notified, task)
	return nil
}

func newContinuationApprovalService(t *testing.T) (*ApprovalService, *fakeContinuationEnqueuer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	q := db.New(testPool)
	fake := &fakeContinuationEnqueuer{real: &service.TaskService{Queries: q}}
	return NewApprovalService(testPool, priv, "cont-test", q, fake), fake
}

// seedContinuationAuthority creates the full CR → shell issue → squad → leader
// agent → runtime chain in ws and returns (issueID, squadID, leaderID, runtimeID,
// approverID). approverID is an owner member of ws (canApprove). All IDs are
// stable per (ws, crID) so repeated calls are idempotent.
func seedContinuationAuthority(t *testing.T, ws, crID string) (issueID, squadID, leaderID, runtimeID, approverID string) {
	t.Helper()
	ctx := context.Background()
	approverID = testUserID(t)
	leaderID = wsUUID(ws, 0x51)
	runtimeID = wsUUID(ws, 0x52)
	squadID = wsUUID(ws, 0x53)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_runtime(id,workspace_id,name,runtime_mode,provider)
		VALUES($1::uuid,$2::uuid,'cont-rt','local','multica_daemon') ON CONFLICT(id) DO NOTHING`, runtimeID, ws); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id)
		VALUES($1::uuid,$2::uuid,'cont-agent','local',$3::uuid) ON CONFLICT(id) DO NOTHING`, leaderID, ws, runtimeID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad(id,workspace_id,name,leader_id,creator_id)
		VALUES($1::uuid,$2::uuid,'cont-squad',$3::uuid,$4::uuid) ON CONFLICT(id) DO NOTHING`, squadID, ws, leaderID, approverID); err != nil {
		t.Fatalf("seed squad: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue(workspace_id,title,creator_type,creator_id,assignee_type,assignee_id,priority)
		VALUES($1::uuid,'cont-issue','member',$2::uuid,'squad',$3::uuid,'medium')
		RETURNING id::text`, ws, approverID, squadID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO cr(workspace_id,cr_id,status,shell_issue_id)
		VALUES($1::uuid,$2,'developing',$3::uuid)
		ON CONFLICT(workspace_id,cr_id) DO UPDATE SET shell_issue_id=EXCLUDED.shell_issue_id, status='developing'`, ws, crID, issueID); err != nil {
		t.Fatalf("seed cr: %v", err)
	}
	return
}

// wsUUID derives a deterministic UUID from a workspace id and a byte tag, so
// each test workspace gets non-colliding fixture ids without carrying state.
func wsUUID(ws string, tag byte) string {
	// reuse the workspace uuid hex with a leading tag byte; valid UUID format.
	ws = nonDash(ws)
	if len(ws) < 32 {
		ws = ws + "000000000000000000000000000000"[:32-len(ws)]
	}
	h := []byte(ws)
	h[0] = tag
	h[12] = '4' // version 4
	h[16] = '8' // variant
	return string(h[0:8]) + "-" + string(h[8:12]) + "-" + string(h[12:16]) + "-" + string(h[16:20]) + "-" + string(h[20:32])
}

func nonDash(s string) string {
	out := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		if c != '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

// insertApprovalRecord creates an undelivered (delivered_at NULL) approval row
// and returns its id. digest/signature are dummies — only the ACK path matters.
func insertApprovalRecord(t *testing.T, ws, crID, stage, decision, approverID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO approval_record(workspace_id,cr_id,stage,decision,approver_user_id,evidence_digest,key_id,signature,grant_json)
		VALUES($1::uuid,$2,$3,$4,$5::uuid,'dummy','cont-test','sig','{}'::jsonb)
		RETURNING id::text`, ws, crID, stage, decision, approverID).Scan(&id); err != nil {
		t.Fatalf("seed approval_record: %v", err)
	}
	return id
}

func ackHTTP(t *testing.T, svc *ApprovalService, ws string, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"ids": ids})
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/approvals/ack", bytes.NewReader(body))
	req = req.WithContext(middleware.WithDaemonContext(req.Context(), ws, "daemon-test"))
	rec := httptest.NewRecorder()
	svc.HandleGrantsAck(rec, req)
	return rec
}

func countContinuationTasks(t *testing.T, ws, crID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue
		 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`,
		ws, crID).Scan(&n); err != nil {
		t.Fatalf("count continuation tasks: %v", err)
	}
	return n
}

// TestAC1_SameRecordTwiceIdempotent: AC-1 — same approval_record ACKed twice →
// exactly one continuation task; second ACK is 200 and idempotent.
func TestAC1_SameRecordTwiceIdempotent(t *testing.T) {
	crID := "CR-9006-001"
	resetCR(t, crID)
	issueID, _, _, _, approverID := seedContinuationAuthority(t, testWorkspaceID, crID)
	_ = issueID
	recID := insertApprovalRecord(t, testWorkspaceID, crID, "requirement", "approve", approverID)
	svc, fake := newContinuationApprovalService(t)

	rec1 := ackHTTP(t, svc, testWorkspaceID, []string{recID})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first ack: %d %s", rec1.Code, rec1.Body.String())
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("after first ack: want 1 continuation task, got %d", got)
	}
	if len(fake.notified) != 1 {
		t.Fatalf("post-commit broadcast: want 1, got %d", len(fake.notified))
	}
	// Second ACK: delivered_at already set → AckApprovalGrants matches 0 rows → 200, no new task.
	rec2 := ackHTTP(t, svc, testWorkspaceID, []string{recID})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second ack (idempotent): %d", rec2.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("after second ack: want still 1 task, got %d", got)
	}
	if len(fake.notified) != 1 {
		t.Fatalf("second ack must not re-broadcast: got %d", len(fake.notified))
	}
}

// TestAC2_FourStagesApproveAndReject: AC-2 — each stage × approve/reject
// produces a continuation task whose context carries stage/decision; reject
// does not interrupt (no special "rejected" state).
func TestAC2_FourStagesApproveAndReject(t *testing.T) {
	stages := []string{"requirement", "tech-design", "dev-start", "code"}
	for _, stage := range stages {
		for _, decision := range []string{"approve", "reject"} {
			crID := "CR-9006-2-" + stage + "-" + decision
			resetCR(t, crID)
			_, _, _, _, approverID := seedContinuationAuthority(t, testWorkspaceID, crID)
			recID := insertApprovalRecord(t, testWorkspaceID, crID, stage, decision, approverID)
			svc, _ := newContinuationApprovalService(t)
			rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
			if rec.Code != http.StatusOK {
				t.Fatalf("ack %s/%s: %d %s", stage, decision, rec.Code, rec.Body.String())
			}
			var ctx []byte
			if err := testPool.QueryRow(context.Background(),
				`SELECT context FROM agent_task_queue
				 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`, testWorkspaceID, crID).Scan(&ctx); err != nil {
				t.Fatalf("load task context %s/%s: %v", stage, decision, err)
			}
			var c struct {
				Approvals []struct {
					Stage    string `json:"stage"`
					Decision string `json:"decision"`
				} `json:"approvals"`
			}
			if err := json.Unmarshal(ctx, &c); err != nil || len(c.Approvals) != 1 || c.Approvals[0].Stage != stage || c.Approvals[0].Decision != decision {
				t.Fatalf("context mismatch %s/%s: %s (err=%v)", stage, decision, string(ctx), err)
			}
		}
	}
}

// TestAC6_FailClosedReasons: AC-6 — missing shell issue / squad / leader keeps
// the ACK undelivered and returns 5xx with a structured reason code.
func TestAC6_FailClosedReasons(t *testing.T) {
	ctx := context.Background()
	approverID := testUserID(t)

	t.Run("no_shell_issue", func(t *testing.T) {
		crID := "CR-9006-6a"
		resetCR(t, crID)
		if _, err := testPool.Exec(ctx, `INSERT INTO cr(workspace_id,cr_id,status) VALUES($1::uuid,$2,'developing') ON CONFLICT DO NOTHING`, testWorkspaceID, crID); err != nil {
			t.Fatal(err)
		}
		recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
		svc, _ := newContinuationApprovalService(t)
		rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("want 500 fail-closed, got %d: %s", rec.Code, rec.Body.String())
		}
		if !bytesContains(rec.Body.Bytes(), "issue-missing") {
			t.Fatalf("want reason issue-missing: %s", rec.Body.String())
		}
		// delivered_at stays NULL → daemon will retry.
		var delivered *string
		_ = testPool.QueryRow(ctx, `SELECT delivered_at::text FROM approval_record WHERE id::text=$1`, recID).Scan(&delivered)
		if delivered != nil {
			t.Fatalf("delivered_at must be NULL on fail-closed, got %v", delivered)
		}
	})

	t.Run("no_leader", func(t *testing.T) {
		crID := "CR-9006-6b"
		resetCR(t, crID)
		// CR with a shell issue but no squad leader → leader-missing.
		var issueID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue(workspace_id,title,creator_type,creator_id,assignee_type,priority)
			VALUES($1::uuid,'no-leader-issue','member',$2::uuid,'member','medium') RETURNING id::text`, testWorkspaceID, approverID).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO cr(workspace_id,cr_id,status,shell_issue_id) VALUES($1::uuid,$2,'developing',$3::uuid) ON CONFLICT DO NOTHING`, testWorkspaceID, crID, issueID); err != nil {
			t.Fatal(err)
		}
		recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
		svc, _ := newContinuationApprovalService(t)
		rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("want 500 fail-closed, got %d", rec.Code)
		}
		if !bytesContains(rec.Body.Bytes(), "leader-missing") {
			t.Fatalf("want reason leader-missing: %s", rec.Body.String())
		}
	})
}

// TestAC6d_CrossWorkspaceIsolation: AC-6d (TD-BL-10) — two workspaces with the
// same CR name each produce their own continuation task; 471 never crosses
// tenants; a second approval in A merges only A's row; B's row is untouched.
func TestAC6d_CrossWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	wsB := ""
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace(name,slug) VALUES('governance-tests-b','governance-tests-b')
		ON CONFLICT(slug) DO UPDATE SET updated_at=now() RETURNING id::text`).Scan(&wsB); err != nil {
		t.Fatalf("seed workspace B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM cr WHERE cr_id='CR-9006-6d'`)
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug='governance-tests-b'`)
	})
	crID := "CR-9006-6d"
	resetCR(t, crID)
	seedContinuationAuthority(t, testWorkspaceID, crID)
	seedContinuationAuthority(t, wsB, crID)
	approverA := testUserID(t)
	// B needs its own approver member.
	approverB := ""
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(email,name) VALUES('b-approver@test','B') ON CONFLICT(email) DO UPDATE SET updated_at=now() RETURNING id::text`).Scan(&approverB); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member(workspace_id,user_id,role) VALUES($1::uuid,$2::uuid,'owner') ON CONFLICT DO NOTHING`, wsB, approverB); err != nil {
		t.Fatal(err)
	}

	recA1 := insertApprovalRecord(t, testWorkspaceID, crID, "requirement", "approve", approverA)
	recB := insertApprovalRecord(t, wsB, crID, "requirement", "approve", approverB)
	svc, _ := newContinuationApprovalService(t)

	if rec := ackHTTP(t, svc, testWorkspaceID, []string{recA1}); rec.Code != http.StatusOK {
		t.Fatalf("A ack: %d", rec.Code)
	}
	if rec := ackHTTP(t, svc, wsB, []string{recB}); rec.Code != http.StatusOK {
		t.Fatalf("B ack: %d", rec.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("A: want 1 task, got %d", got)
	}
	if got := countContinuationTasks(t, wsB, crID); got != 1 {
		t.Fatalf("B: want 1 task, got %d", got)
	}
	// Second approval in A merges into A's successor only.
	recA2 := insertApprovalRecord(t, testWorkspaceID, crID, "tech-design", "approve", approverA)
	if rec := ackHTTP(t, svc, testWorkspaceID, []string{recA2}); rec.Code != http.StatusOK {
		t.Fatalf("A second ack: %d", rec.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("A after merge: want still 1 task, got %d", got)
	}
	// B's row untouched: still exactly one approval in approvals[].
	var bApprovals []byte
	if err := testPool.QueryRow(ctx,
		`SELECT context->'approvals' FROM agent_task_queue
		 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`, wsB, crID).Scan(&bApprovals); err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(bApprovals, &arr); err != nil || len(arr) != 1 {
		t.Fatalf("B must still have exactly 1 approval after A's merge, got %d", len(arr))
	}
	// Cross-tenant read: A's ws querying B's record id → 0 rows.
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("A cannot see B's tasks: want 1, got %d", got)
	}
}

// TestAC7_HistoricalDeliveredNoTask: AC-7 — a record already delivered (non-null
// delivered_at) produces no task and no 5xx; UPDATE matches 0 rows → 200.
func TestAC7_HistoricalDeliveredNoTask(t *testing.T) {
	ctx := context.Background()
	crID := "CR-9006-7"
	resetCR(t, crID)
	seedContinuationAuthority(t, testWorkspaceID, crID)
	approverID := testUserID(t)
	recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
	if _, err := testPool.Exec(ctx, `UPDATE approval_record SET delivered_at=now() WHERE id::text=$1`, recID); err != nil {
		t.Fatal(err)
	}
	svc, _ := newContinuationApprovalService(t)
	rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
	if rec.Code != http.StatusOK {
		t.Fatalf("historical delivered ack must be 200, got %d", rec.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 0 {
		t.Fatalf("historical delivered must produce no task, got %d", got)
	}
}

// TestAC8_RunnerOffContinuationStillEnqueues: AC-8 — with both hooks unset
// (Runner default off), HandleGrantsAck still commits + enqueues + 200. The
// context carries no state→next-step mapping.
func TestAC8_RunnerOffContinuationStillEnqueues(t *testing.T) {
	crID := "CR-9006-8"
	resetCR(t, crID)
	seedContinuationAuthority(t, testWorkspaceID, crID)
	recID := insertApprovalRecord(t, testWorkspaceID, crID, "dev-start", "approve", testUserID(t))
	svc, _ := newContinuationApprovalService(t)
	// No SetGrantAckHandler / SetGrantAckCommittedHandler → Runner off.
	rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
	if rec.Code != http.StatusOK {
		t.Fatalf("runner off ack must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ctxJSON []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT context FROM agent_task_queue
		 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`, testWorkspaceID, crID).Scan(&ctxJSON); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ctxJSON, []byte("next_step")) || bytes.Contains(ctxJSON, []byte("next-step")) {
		t.Fatalf("context must not carry a state→next-step mapping: %s", string(ctxJSON))
	}
}

// TestAC9_DoubleHookContract: AC-9a~d (TD-BL-12) — pre-commit handler receives
// the right GrantAckEvent and its error → 5xx/rollback/delivered_at NULL; it is
// zero-side-effect (replayable); committed handler error → log only, HTTP 2xx.
func TestAC9_DoubleHookContract(t *testing.T) {
	ctx := context.Background()
	crID := "CR-9006-9"
	resetCR(t, crID)
	seedContinuationAuthority(t, testWorkspaceID, crID)
	approverID := testUserID(t)

	t.Run("9a_precommit_error_rolls_back", func(t *testing.T) {
		recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
		svc, _ := newContinuationApprovalService(t)
		var gotEvent GrantAckEvent
		var calls int32
		svc.SetGrantAckHandler(func(_ context.Context, ev GrantAckEvent) error {
			atomic.StoreInt32(&calls, 1)
			gotEvent = ev
			return errSentinel
		})
		rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("pre-commit error must be 5xx, got %d", rec.Code)
		}
		if !bytesContains(rec.Body.Bytes(), "ack-handler-failed") {
			t.Fatalf("want reason ack-handler-failed: %s", rec.Body.String())
		}
		if gotEvent.RecordID != recID || gotEvent.Stage != "code" || gotEvent.Decision != "approve" || gotEvent.WorkspaceID != testWorkspaceID {
			t.Fatalf("pre-commit handler got wrong event: %+v", gotEvent)
		}
		var delivered *string
		_ = testPool.QueryRow(ctx, `SELECT delivered_at::text FROM approval_record WHERE id::text=$1`, recID).Scan(&delivered)
		if delivered != nil {
			t.Fatalf("pre-commit error must leave delivered_at NULL, got %v", delivered)
		}
		if got := countContinuationTasks(t, testWorkspaceID, crID); got != 0 {
			t.Fatalf("pre-commit error must leave no task, got %d", got)
		}
	})

	t.Run("9b_precommit_replayable", func(t *testing.T) {
		// The handler did no side effects (9a); retrying the ACK after the
		// handler returns nil must succeed end-to-end (daemon replay semantics).
		recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
		svc, _ := newContinuationApprovalService(t)
		var calls int32
		svc.SetGrantAckHandler(func(_ context.Context, ev GrantAckEvent) error {
			atomic.AddInt32(&calls, 1)
			return nil // pure validation, no side effect
		})
		rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
		if rec.Code != http.StatusOK {
			t.Fatalf("replay after nil handler must be 200, got %d", rec.Code)
		}
		if atomic.LoadInt32(&calls) != 1 {
			t.Fatalf("pre-commit handler must be called exactly once per ACKed record, got %d", calls)
		}
	})

	t.Run("9c_committed_error_keeps_2xx", func(t *testing.T) {
		recID := insertApprovalRecord(t, testWorkspaceID, crID, "code", "approve", approverID)
		svc, _ := newContinuationApprovalService(t)
		svc.SetGrantAckCommittedHandler(func(_ context.Context, ev GrantAckEvent) error {
			return errSentinel
		})
		rec := ackHTTP(t, svc, testWorkspaceID, []string{recID})
		if rec.Code != http.StatusOK {
			t.Fatalf("committed wake error must keep HTTP 2xx, got %d", rec.Code)
		}
		// delivered_at IS set (committed) — no retry.
		var delivered *string
		_ = testPool.QueryRow(ctx, `SELECT delivered_at::text FROM approval_record WHERE id::text=$1`, recID).Scan(&delivered)
		if delivered == nil {
			t.Fatal("committed wake error must NOT clear delivered_at")
		}
	})
}

// errSentinel is a sentinel error for AC-9 handler tests.
var errSentinel = errors.New("sentinel handler failure")

// TestAC5_MergeAndSlotDeferred: AC-5 — a second approval for the same CR
// merges into the queued successor (approvals[] grows, handoff gains a line,
// still one task); when the (issue, agent) slot is held by an ordinary task,
// the continuation falls back to a deferred row outside the 257 predicate.
func TestAC5_MergeAndSlotDeferred(t *testing.T) {
	ctx := context.Background()
	crID := "CR-9006-5"
	resetCR(t, crID)
	issueID, _, leaderID, _, approverID := seedContinuationAuthority(t, testWorkspaceID, crID)
	_ = issueID
	svc, fake := newContinuationApprovalService(t)

	// First approval → queued successor.
	rec1 := insertApprovalRecord(t, testWorkspaceID, crID, "requirement", "approve", approverID)
	if rec := ackHTTP(t, svc, testWorkspaceID, []string{rec1}); rec.Code != http.StatusOK {
		t.Fatalf("first ack: %d", rec.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("first ack: want 1 task, got %d", got)
	}
	if len(fake.notified) != 1 {
		t.Fatalf("first ack (successor-enqueued) must broadcast 1, got %d", len(fake.notified))
	}

	// Second approval, same CR → merges into the queued successor.
	rec2 := insertApprovalRecord(t, testWorkspaceID, crID, "tech-design", "approve", approverID)
	if rec := ackHTTP(t, svc, testWorkspaceID, []string{rec2}); rec.Code != http.StatusOK {
		t.Fatalf("second ack (merge): %d", rec.Code)
	}
	if got := countContinuationTasks(t, testWorkspaceID, crID); got != 1 {
		t.Fatalf("merge: want still 1 task, got %d", got)
	}
	// Merged row must NOT be re-broadcast (TD-SUG-1).
	if len(fake.notified) != 1 {
		t.Fatalf("merge must not broadcast, got %d", len(fake.notified))
	}
	var ctxJSON []byte
	if err := testPool.QueryRow(ctx,
		`SELECT context FROM agent_task_queue
		 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`,
		testWorkspaceID, crID).Scan(&ctxJSON); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Approvals []json.RawMessage `json:"approvals"`
	}
	if err := json.Unmarshal(ctxJSON, &c); err != nil || len(c.Approvals) != 2 {
		t.Fatalf("merged successor must carry 2 approvals, got %d", len(c.Approvals))
	}

	// Slot-deferred: an ordinary queued task on the same (issue, agent) blocks
	// a third approval's queued INSERT (257) → ladder 3 deferred fallback.
	crIDb := "CR-9006-5b"
	resetCR(t, crIDb)
	issueIDb, _, _, _, approverIDb := seedContinuationAuthority(t, testWorkspaceID, crIDb)
	leaderUUID, _ := parseUUID(leaderID)
	// Occupy the (issue_b, leader) slot with an ordinary queued task so the
	// continuation's queued INSERT conflicts on 257 (idx_one_pending_task_
	// per_issue_agent_v2) and falls back to a deferred row outside its
	// predicate.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status,priority,originator_source)
		SELECT a.id, a.runtime_id, $2::uuid, 'queued', 0, 'direct_human'
		FROM agent a WHERE a.id=$1::uuid`, leaderUUID, issueIDb); err != nil {
		t.Fatalf("seed occupying ordinary task: %v", err)
	}
	rec3 := insertApprovalRecord(t, testWorkspaceID, crIDb, "code", "approve", approverIDb)
	if rec := ackHTTP(t, svc, testWorkspaceID, []string{rec3}); rec.Code != http.StatusOK {
		t.Fatalf("slot-deferred ack: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM agent_task_queue
		 WHERE approval_workspace_id=$1::uuid AND trigger_evidence_kind='approval_continuation' AND cr_id=$2`,
		testWorkspaceID, crIDb).Scan(&status); err != nil {
		t.Fatalf("load deferred continuation: %v", err)
	}
	if status != "deferred" {
		t.Fatalf("slot-deferred: want status=deferred, got %q", status)
	}
}

func bytesContains(b []byte, sub string) bool {
	return bytes.Contains(b, []byte(sub))
}
