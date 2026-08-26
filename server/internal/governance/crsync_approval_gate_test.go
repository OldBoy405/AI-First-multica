package governance

// AIFIRST: CR-2026-051 TASK-03 (AC-1 / AC-2) — real-DB tests for the
// approval-gate publication point. AC-1: each of the four human-approval
// gates (plus the gate re-entry transition) publishes exactly one
// cr:approval-gate-entered event with a fully-populated payload. AC-2: every
// non-gate path publishes zero gate events, each proven with a path-appropriate
// liveness probe (cr:updated > 0 on publish paths; cr_sync_event.processed_at
// on ledger-only paths; healed >= 1 on reconcile; pipeline_* rows on gate
// projection). Reuses the package TestMain / testPool / testWorkspaceID /
// resetCR / postEvents / ev helpers unchanged (CUSTOM.md C6: never loosen the
// TestMain skip logic).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type approvalGateCollector struct {
	mu       sync.Mutex
	gate     []events.Event
	updated  []events.Event
	overflow map[string]int
}

func newApprovalGateCollector(bus *events.Bus) *approvalGateCollector {
	c := &approvalGateCollector{overflow: map[string]int{}}
	bus.Subscribe(EventCRApprovalGateEntered, func(e events.Event) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.gate = append(c.gate, e)
	})
	bus.Subscribe(EventCRUpdated, func(e events.Event) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.updated = append(c.updated, e)
	})
	bus.SubscribeAll(func(e events.Event) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.overflow[e.Type]++
	})
	return c
}

func (c *approvalGateCollector) gateN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.gate)
}

func (c *approvalGateCollector) updatedN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.updated)
}

// seedGateCR plants a cr row with a chosen status (and optional shell_issue_id)
// so the target transition can be exercised without replaying the whole chain.
func seedGateCR(t *testing.T, crID, status string, shellIssueID *string) {
	t.Helper()
	ctx := context.Background()
	if shellIssueID == nil {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO cr (workspace_id, cr_id, status)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (workspace_id, cr_id) DO UPDATE SET status = $3, shell_issue_id = NULL`,
			testWorkspaceID, crID, status); err != nil {
			t.Fatalf("seed cr: %v", err)
		}
		return
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, status, shell_issue_id)
		VALUES ($1::uuid, $2, $3, $4::uuid)
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET status = $3, shell_issue_id = $4::uuid`,
		testWorkspaceID, crID, status, *shellIssueID); err != nil {
		t.Fatalf("seed cr with shell issue: %v", err)
	}
}

func gatePayload(t *testing.T, e events.Event) protocol.ApprovalGateEnteredPayload {
	t.Helper()
	p, ok := e.Payload.(protocol.ApprovalGateEnteredPayload)
	if !ok {
		t.Fatalf("payload is %T, want protocol.ApprovalGateEnteredPayload", e.Payload)
	}
	return p
}

// TestApprovalGateAC1FourGatesPlusReEntry asserts exactly one gate event per
// legal gate transition, with full payload/event_id shape checks.
func TestApprovalGateAC1FourGatesPlusReEntry(t *testing.T) {
	cases := []struct {
		name   string
		crID   string
		seed   string
		from   string
		to     string
		trigger string
	}{
		{"requirement gate", "CR-9051-001", "drafting", "drafting", "requirement-reviewing", "review-requirement"},
		{"tech-design gate", "CR-9051-002", "tech-designing", "tech-designing", "tech-design-review-pending", "write-tech-design-complete"},
		{"dev-start gate", "CR-9051-003", "tech-design-reviewed", "tech-design-reviewed", "task-breakdown", "write-dev-tasks"},
		{"code gate", "CR-9051-004", "developing", "developing", "code-reviewing", "review-code"},
		{"gate re-entry", "CR-9051-005", "task-breakdown", "task-breakdown", "tech-design-review-pending", "review-dev-plan:upstream-design-blocker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCR(t, tc.crID)
			defer resetCR(t, tc.crID)
			seedGateCR(t, tc.crID, tc.seed, nil)

			bus := events.New()
			c := newApprovalGateCollector(bus)
			svc := NewSyncService(testPool, bus)
			sha := "ag-sha-" + tc.crID
			postEvents(t, svc, testWorkspaceID, []OutboxEvent{
				ev(tc.crID, "status", tc.from, tc.to, tc.trigger, sha, tc.crID+".json"),
			})

			if got := c.gateN(); got != 1 {
				t.Fatalf("gate events = %d, want 1", got)
			}
			c.mu.Lock()
			e := c.gate[0]
			c.mu.Unlock()
			if e.Type != EventCRApprovalGateEntered {
				t.Errorf("type = %q", e.Type)
			}
			if e.WorkspaceID != testWorkspaceID {
				t.Errorf("workspace = %q", e.WorkspaceID)
			}
			if e.ActorType != "system" {
				t.Errorf("actor = %q", e.ActorType)
			}
			p := gatePayload(t, e)
			if p.CRID != tc.crID || p.Status != tc.to {
				t.Errorf("payload cr/status = %q/%q, want %q/%q", p.CRID, p.Status, tc.crID, tc.to)
			}
			if want := tc.crID + ":status:" + sha; p.EventID != want {
				t.Errorf("event_id = %q, want %q", p.EventID, want)
			}
			if p.ShellIssueID != nil {
				t.Errorf("shell_issue_id = %v, want nil", *p.ShellIssueID)
			}
			// The legacy broadcast surface stays intact: the gate transition
			// also produced the usual cr:updated.
			if c.updatedN() == 0 {
				t.Error("cr:updated must still be published on a gate transition")
			}
		})
	}
}

// TestApprovalGateAC2ZeroPublishMatrix covers the mis-trigger isolation matrix;
// every case carries its own liveness probe.
func TestApprovalGateAC2ZeroPublishMatrix(t *testing.T) {
	newHarness := func(crID string) (*SyncService, *approvalGateCollector) {
		bus := events.New()
		c := newApprovalGateCollector(bus)
		return NewSyncService(testPool, bus), c
	}

	t.Run("non-gate target", func(t *testing.T) {
		crID := "CR-9051-010"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "requirement-reviewing", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "requirement-reviewing", "requirement-approved", "approve-requirement", "sha-nongate", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("self-loop task-breakdown", func(t *testing.T) {
		crID := "CR-9051-011"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "task-breakdown", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "task-breakdown", "task-breakdown", "write-dev-tasks", "sha-loop1", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("self-loop requirement-reviewing", func(t *testing.T) {
		crID := "CR-9051-012"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "requirement-reviewing", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "requirement-reviewing", "requirement-reviewing", "review-requirement", "sha-loop2", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("first-sight fresh registration", func(t *testing.T) {
		crID := "CR-9051-013"
		resetCR(t, crID)
		defer resetCR(t, crID)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "", "drafting", "requirement-register", "sha-fresh", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("first-sight mid-flight best-effort", func(t *testing.T) {
		// The easiest one to miss: the target IS a gate status, but a
		// first-sighting is never a trusted transition.
		crID := "CR-9051-014"
		resetCR(t, crID)
		defer resetCR(t, crID)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "tech-designing", "tech-design-review-pending", "write-tech-design-complete", "sha-midflight", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
		st, nr, _ := crRow(t, crID)
		if st != "tech-design-review-pending" || !nr {
			t.Errorf("mid-flight first sight: status=%q needs_reconcile=%v", st, nr)
		}
	})

	t.Run("out-of-order illegal towards gate", func(t *testing.T) {
		crID := "CR-9051-015"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "developing", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "tech-designing", "tech-design-review-pending", "write-tech-design-complete", "sha-ooo", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		_, nr, _ := crRow(t, crID)
		if !nr {
			t.Error("out-of-order must flag needs_reconcile")
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("checkpoint event", func(t *testing.T) {
		crID := "CR-9051-016"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "developing", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "checkpoint", "", "", "", "sha-checkpoint", "f.json"),
		})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("review event with gate stage", func(t *testing.T) {
		crID := "CR-9051-017"
		resetCR(t, crID)
		defer resetCR(t, crID)
		testUserID(t) // findOrCreateRun needs an owner member for started_by
		seedGateCR(t, crID, "requirement-reviewing", nil)
		svc, c := newHarness(crID)
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{{
			V: 1, File: "f.json", EventKind: "review", CRID: crID,
			CommitSHA: "sha-review", Actor: "tester", OccurredAt: time.Now(),
			Payload: json.RawMessage(`{"stage":"requirement","verdict":"pass","attempt":1}`),
		}})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() == 0 {
			t.Error("liveness probe: cr:updated must be > 0")
		}
	})

	t.Run("review event non-gate stage", func(t *testing.T) {
		// applyReview returns early for stage=dev-start and never publishes;
		// the ledger row + processed_at is the only valid liveness probe.
		crID := "CR-9051-018"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "task-breakdown", nil)
		svc, c := newHarness(crID)
		reviewSHA := "sha-review-devstart-" + fmt.Sprint(time.Now().UnixNano())
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{{
			V: 1, File: "f.json", EventKind: "review", CRID: crID,
			CommitSHA: reviewSHA, Actor: "tester", OccurredAt: time.Now(),
			Payload: json.RawMessage(`{"stage":"dev-start","verdict":"pass","attempt":1}`),
		}})
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		var processedAt *string
		if err := testPool.QueryRow(context.Background(), `
			SELECT processed_at::text FROM cr_sync_event
			WHERE workspace_id = $1::uuid AND cr_id = $2 AND commit_sha = $3 AND event_kind = 'review'`,
			testWorkspaceID, crID, reviewSHA).Scan(&processedAt); err != nil {
			t.Fatalf("liveness probe: review ledger row missing: %v", err)
		}
		if processedAt == nil {
			t.Error("liveness probe: review ledger row not processed")
		}
	})

	t.Run("trace event ledger-only", func(t *testing.T) {
		crID := "CR-9051-019"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "developing", nil)
		svc, c := newHarness(crID)
		resp := postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			traceEvent(crID, "sha-trace-ag", "trace.json", validTracePayload(crID)),
		})
		if len(resp.Accepted) != 1 {
			t.Fatalf("trace not accepted: %+v", resp)
		}
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0", c.gateN())
		}
		if c.updatedN() != 0 {
			t.Error("trace must never publish cr:updated (ingestTrace is ledger-only)")
		}
		var processedAt *string
		if err := testPool.QueryRow(context.Background(), `
			SELECT processed_at::text FROM cr_sync_event
			WHERE workspace_id = $1::uuid AND cr_id = $2 AND commit_sha = $3 AND event_kind = 'trace'`,
			testWorkspaceID, crID, "sha-trace-ag").Scan(&processedAt); err != nil {
			t.Fatalf("liveness probe: trace ledger row missing: %v", err)
		}
		if processedAt == nil {
			t.Error("liveness probe: trace ledger row not processed")
		}
	})

	t.Run("reconcile heals to gate status", func(t *testing.T) {
		crID := "CR-9051-020"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "drafting", nil)
		svc, c := newHarness(crID)
		healed, err := svc.ApplySnapshot(context.Background(), testWorkspaceID, AuthoritySnapshot{
			HeadSHA:  "snap-sha-ag",
			Statuses: map[string]string{crID: "task-breakdown"},
		})
		if err != nil {
			t.Fatalf("ApplySnapshot: %v", err)
		}
		if healed < 1 {
			t.Errorf("liveness probe: healed = %d, want >= 1", healed)
		}
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0 (reconcile must never publish the gate event)", c.gateN())
		}
	})

	t.Run("gate projection", func(t *testing.T) {
		// projectGateTransition is called from the trusted branch and from
		// reconcile; calling it directly (package-internal, same process as
		// the production path) proves the projection path itself publishes
		// nothing. A legal "drafting" -> "requirement-reviewing" projection
		// maps to a pipeline, so the pipeline_* rows double as the liveness
		// probe that the projection really ran.
		crID := "CR-9051-021"
		resetCR(t, crID)
		defer resetCR(t, crID)
		testUserID(t) // findOrCreateRun needs an owner member for started_by
		seedGateCR(t, crID, "drafting", nil)
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM pipeline_node_run WHERE run_id IN (
				SELECT id FROM pipeline_run WHERE workspace_id = $1::uuid AND cr_id = $2)`,
			testWorkspaceID, crID); err != nil {
			t.Fatalf("clean pipeline runs: %v", err)
		}
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM pipeline_run WHERE workspace_id = $1::uuid AND cr_id = $2`,
			testWorkspaceID, crID); err != nil {
			t.Fatalf("clean pipeline runs: %v", err)
		}
		svc, c := newHarness(crID)
		svc.projectGateTransition(context.Background(), testWorkspaceID, crID, "drafting", "requirement-reviewing")
		if c.gateN() != 0 {
			t.Fatalf("gate events = %d, want 0 (gate projection must not publish)", c.gateN())
		}
		var runCount int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM pipeline_run WHERE workspace_id = $1::uuid AND cr_id = $2`,
			testWorkspaceID, crID).Scan(&runCount); err != nil {
			t.Fatalf("liveness probe: pipeline_run query failed: %v", err)
		}
		if runCount == 0 {
			t.Error("liveness probe: pipeline_run row must exist after gate projection")
		}
	})
}

// TestApprovalGateSubscriptionSurface asserts the new event does not leak into
// the cr:updated subscription surface and the constants stay distinct.
func TestApprovalGateSubscriptionSurface(t *testing.T) {
	if EventCRApprovalGateEntered == EventCRUpdated {
		t.Fatal("gate event constant must differ from cr:updated")
	}
	crID := "CR-9051-030"
	resetCR(t, crID)
	defer resetCR(t, crID)
	seedGateCR(t, crID, "drafting", nil)
	bus := events.New()
	c := newApprovalGateCollector(bus)
	svc := NewSyncService(testPool, bus)
	postEvents(t, svc, testWorkspaceID, []OutboxEvent{
		ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", "sha-surf", "f.json"),
	})
	if c.gateN() != 1 {
		t.Fatalf("gate events = %d, want 1", c.gateN())
	}
	c.mu.Lock()
	gateOverflow := c.overflow[EventCRApprovalGateEntered]
	updatedOverflow := c.overflow[EventCRUpdated]
	c.mu.Unlock()
	// cr:updated subscribers received only cr:updated; the gate event reached
	// its own type-specific subscribers (plus SubscribeAll).
	if gateOverflow != 1 {
		t.Errorf("gate event via SubscribeAll = %d, want 1", gateOverflow)
	}
	if updatedOverflow != 1 {
		t.Errorf("cr:updated via SubscribeAll = %d, want 1", updatedOverflow)
	}
	if len(c.updated) != 1 {
		t.Errorf("cr:updated collector = %d, want 1", len(c.updated))
	}
}

// TestApprovalGateShellIssueIDTwoStates covers BL-3's producer half: the
// payload carries shell_issue_id when present, nil when NULL, and the
// marshaled shape matches TASK-01's canonical golden JSON.
func TestApprovalGateShellIssueIDTwoStates(t *testing.T) {
	newShellIssue := func(t *testing.T) string {
		t.Helper()
		var issueID string
		if err := testPool.QueryRow(context.Background(), `
			INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
			SELECT $1::uuid, 'ag shell', 'member', u.id,
			       (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1::uuid)
			FROM "user" u
			WHERE u.id = (SELECT user_id FROM member WHERE workspace_id = $1::uuid LIMIT 1)
			RETURNING id::text`, testWorkspaceID).Scan(&issueID); err != nil {
			t.Fatalf("issue fixture: %v", err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1::uuid`, issueID)
		})
		return issueID
	}

	t.Run("with shell issue id", func(t *testing.T) {
		crID := "CR-9051-040"
		resetCR(t, crID)
		defer resetCR(t, crID)
		issueID := newShellIssue(t)
		seedGateCR(t, crID, "drafting", &issueID)
		bus := events.New()
		c := newApprovalGateCollector(bus)
		svc := NewSyncService(testPool, bus)
		sha := "sha-shell"
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", sha, "f.json"),
		})
		if c.gateN() != 1 {
			t.Fatalf("gate events = %d, want 1", c.gateN())
		}
		c.mu.Lock()
		p := gatePayload(t, c.gate[0])
		c.mu.Unlock()
		if p.ShellIssueID == nil || *p.ShellIssueID != issueID {
			t.Errorf("shell_issue_id = %v, want %q", p.ShellIssueID, issueID)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		want := fmt.Sprintf(`{"cr_id":%q,"status":"requirement-reviewing","event_id":%q,"shell_issue_id":%q}`,
			crID, crID+":status:"+sha, issueID)
		if string(raw) != want {
			t.Errorf("producer golden JSON:\n got %s\nwant %s", raw, want)
		}
	})

	t.Run("null shell issue id", func(t *testing.T) {
		crID := "CR-9051-041"
		resetCR(t, crID)
		defer resetCR(t, crID)
		seedGateCR(t, crID, "drafting", nil)
		bus := events.New()
		c := newApprovalGateCollector(bus)
		svc := NewSyncService(testPool, bus)
		sha := "sha-null"
		postEvents(t, svc, testWorkspaceID, []OutboxEvent{
			ev(crID, "status", "drafting", "requirement-reviewing", "review-requirement", sha, "f.json"),
		})
		if c.gateN() != 1 {
			t.Fatalf("gate events = %d, want 1", c.gateN())
		}
		c.mu.Lock()
		p := gatePayload(t, c.gate[0])
		c.mu.Unlock()
		if p.ShellIssueID != nil {
			t.Errorf("shell_issue_id = %q, want nil", *p.ShellIssueID)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		want := fmt.Sprintf(`{"cr_id":%q,"status":"requirement-reviewing","event_id":%q,"shell_issue_id":null}`,
			crID, crID+":status:"+sha)
		if string(raw) != want {
			t.Errorf("producer golden JSON (null form):\n got %s\nwant %s", raw, want)
		}
	})
}
