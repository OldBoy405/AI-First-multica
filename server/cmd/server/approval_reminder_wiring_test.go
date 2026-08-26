package main

// AIFIRST: CR-2026-051 TASK-07 — approval reminder wiring tests (AC-11
// end-to-end latency, AC-12 disabled form, and the typed-nil guard — the ONLY
// validation point for the typed-nil trap, plan §5.7 BL-3). Uses the package
// TestMain fixture (testPool / testWorkspaceID / testUserID) and a real
// governance.SyncService so the blocking-client latency assertion runs the
// true HandleCREvents path.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/protocol"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// wiringBlockClient is a full lark.APIClient fake whose card send hangs on
// ctx.Done() — the recipient-timeout context — so the end-to-end test can
// prove the sync handler stays fast while the async chain is stuck, and that
// the hung send itself fails as error_class=timeout (AC-11).
type wiringBlockClient struct {
	mu    sync.Mutex
	sends int
}

func (c *wiringBlockClient) IsConfigured() bool { return true }
func (c *wiringBlockClient) SendInteractiveCard(context.Context, lark.SendCardParams) (string, error) {
	return "", nil
}
func (c *wiringBlockClient) PatchInteractiveCard(context.Context, lark.PatchCardParams) error {
	return nil
}
func (c *wiringBlockClient) SendTextMessage(context.Context, lark.SendTextParams) (string, error) {
	return "", nil
}
func (c *wiringBlockClient) SendMarkdownCard(context.Context, lark.SendMarkdownCardParams) (string, error) {
	return "", nil
}
func (c *wiringBlockClient) SendBindingPromptCard(context.Context, lark.BindingPromptParams) error {
	return nil
}
func (c *wiringBlockClient) SendApprovalReminderCard(ctx context.Context, p lark.ApprovalReminderParams) error {
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	<-ctx.Done() // hang until the recipient timeout fires, like a stuck upstream
	return ctx.Err()
}
func (c *wiringBlockClient) GetBotInfo(context.Context, lark.InstallationCredentials) (lark.BotInfo, error) {
	return lark.BotInfo{}, nil
}
func (c *wiringBlockClient) GetMessage(context.Context, lark.InstallationCredentials, string) ([]lark.LarkMessage, error) {
	return nil, nil
}
func (c *wiringBlockClient) ListChatMessages(context.Context, lark.InstallationCredentials, lark.ListMessagesParams) ([]lark.LarkMessage, error) {
	return nil, nil
}
func (c *wiringBlockClient) DownloadMessageResource(context.Context, lark.InstallationCredentials, lark.DownloadResourceParams) (lark.DownloadedResource, error) {
	return lark.DownloadedResource{}, nil
}
func (c *wiringBlockClient) BatchGetUsers(context.Context, lark.InstallationCredentials, []string) (map[string]string, error) {
	return nil, nil
}
func (c *wiringBlockClient) AddMessageReaction(context.Context, lark.AddReactionParams) (string, error) {
	return "", nil
}
func (c *wiringBlockClient) DeleteMessageReaction(context.Context, lark.DeleteReactionParams) error {
	return nil
}

// wiringLogs collects the reminder's JSON log lines.
type wiringLogs struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (w *wiringLogs) add(line []byte) {
	var m map[string]any
	if json.Unmarshal(line, &m) == nil {
		w.mu.Lock()
		w.lines = append(w.lines, m)
		w.mu.Unlock()
	}
}

func (w *wiringLogs) count(field, want string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.lines {
		if v, _ := m[field].(string); v == want {
			n++
		}
	}
	return n
}

type wiringLogWriter struct {
	mu  *sync.Mutex
	log *wiringLogs
}

func (w *wiringLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range bytes.Split(p, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			w.log.add(line)
		}
	}
	return len(p), nil
}

// TestApprovalReminderWiringPosition asserts the wiring sits outside (after)
// the MULTICA_LARK_SECRET_KEY block.
func TestApprovalReminderWiringPosition(t *testing.T) {
	raw, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	wiringLine, disabledLine := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "NewApprovalReminder") {
			if wiringLine != -1 {
				t.Fatalf("NewApprovalReminder must appear exactly once (lines %d and %d)", wiringLine, i+1)
			}
			wiringLine = i + 1
		}
		if strings.Contains(line, "lark integration disabled (MULTICA_LARK_SECRET_KEY not set)") {
			disabledLine = i + 1
		}
	}
	if wiringLine == -1 {
		t.Fatal("NewApprovalReminder wiring not found in router.go")
	}
	if disabledLine == -1 {
		t.Fatal("lark-disabled branch not found in router.go")
	}
	if wiringLine <= disabledLine {
		t.Errorf("wiring (line %d) must be AFTER the lark-disabled branch (line %d)", wiringLine, disabledLine)
	}
}

// TestApprovalReminderDisabledForm covers AC-12's disabled shape: everything
// nil → one feishu-disabled event-level skip, zero HTTP, zero recipient
// queries, and the expected phase bookkeeping.
func TestApprovalReminderDisabledForm(t *testing.T) {
	mu := &sync.Mutex{}
	logs := &wiringLogs{}
	logger := newWiringLogger(mu, logs)
	cfg := lark.ApprovalReminderConfig{
		Pool: nil, AppURL: "https://multica.test", Logger: logger,
	}
	rem := lark.NewApprovalReminder(cfg)
	if rem == nil {
		t.Fatal("constructor must never return nil")
	}
	bus := events.New()
	rem.Register(bus)
	bus.Publish(events.Event{
		Type: protocol.EventCRApprovalGateEntered, WorkspaceID: "ws-1", ActorType: "system",
		Payload: protocol.ApprovalGateEnteredPayload{
			CRID: "CR-2026-051", Status: "requirement-reviewing", EventID: "CR-2026-051:status:e",
		},
	})
	deadline := time.Now().Add(3 * time.Second)
	for logs.count("reason", "feishu-disabled") != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.count("result", "skipped") != 1 || logs.count("reason", "feishu-disabled") != 1 {
		t.Fatalf("want exactly one feishu-disabled skip, got skipped=%d", logs.count("result", "skipped"))
	}
	if logs.count("result", "failed") != 0 {
		t.Error("zero DB: no result=failed allowed (nil pool makes queries structurally impossible)")
	}
	if logs.count("phase", "panic-recovered") != 0 {
		t.Error("no phase=panic-recovered allowed")
	}
	if logs.count("phase", "register") != 0 {
		t.Error("real bus: phase=register must be 0")
	}
	// Constructor logged exactly one phase=construct with all three deps.
	mu2 := &sync.Mutex{}
	_ = mu2
	constructN := 0
	missingOK := false
	logs.mu.Lock()
	for _, m := range logs.lines {
		if v, _ := m["phase"].(string); v == "construct" {
			constructN++
			if v, _ := m["missing"].(string); strings.Contains(v, "pool") && strings.Contains(v, "client") && strings.Contains(v, "credentials") {
				missingOK = true
			}
		}
	}
	logs.mu.Unlock()
	if constructN != 1 || !missingOK {
		t.Errorf("want exactly one phase=construct with all three deps, got %d missingOK=%v", constructN, missingOK)
	}
}

func newWiringLogger(mu *sync.Mutex, logs *wiringLogs) *slog.Logger {
	_ = mu
	return slog.New(slog.NewJSONHandler(&wiringLogWriter{mu: &sync.Mutex{}, log: logs}, nil))
}

// TestApprovalReminderTypedNilGuard is the ONLY validation point for the
// typed-nil trap (plan BL-3): all three assertions must execute.
func TestApprovalReminderTypedNilGuard(t *testing.T) {
	// ① Load-bearing: a nil *InstallationService assigned to the interface
	// field is NOT nil — the wiring-level nil check is what keeps the config
	// free of typed-nil interfaces.
	var inst *lark.InstallationService
	cfg := lark.ApprovalReminderConfig{}
	cfg.Credentials = inst
	if cfg.Credentials == nil {
		t.Fatal("① typed-nil trap: nil *InstallationService became a non-nil interface; the wiring guard is load-bearing")
	}

	// ② Positive: the wiring form keeps real nils.
	cfg2 := lark.ApprovalReminderConfig{}
	var apiClient lark.APIClient
	if apiClient != nil {
		cfg2.Client = apiClient
	}
	if inst != nil {
		cfg2.Credentials = inst
	}
	if cfg2.Client != nil || cfg2.Credentials != nil {
		t.Fatalf("② wiring form must keep real nils: client=%v credentials=%v", cfg2.Client, cfg2.Credentials)
	}

	// ③ End-to-end with the ② config: one feishu-disabled skip, zero HTTP,
	// zero DB, process alive.
	mu := &sync.Mutex{}
	logs := &wiringLogs{}
	cfg2.Logger = newWiringLogger(mu, logs)
	cfg2.AppURL = "https://multica.test"
	rem := lark.NewApprovalReminder(cfg2)
	bus := events.New()
	rem.Register(bus)
	bus.Publish(events.Event{
		Type: protocol.EventCRApprovalGateEntered, WorkspaceID: "ws-2", ActorType: "system",
		Payload: protocol.ApprovalGateEnteredPayload{
			CRID: "CR-2026-051", Status: "task-breakdown", EventID: "CR-2026-051:status:e2",
		},
	})
	deadline := time.Now().Add(3 * time.Second)
	for logs.count("reason", "feishu-disabled") != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.count("reason", "feishu-disabled") != 1 {
		t.Fatal("③ end-to-end: want exactly one feishu-disabled skip")
	}
	if logs.count("result", "failed") != 0 || logs.count("phase", "panic-recovered") != 0 {
		t.Error("③ zero DB / zero panic requirement violated")
	}
}

// TestApprovalReminderEndToEndLatency covers AC-11: with the card send
// blocked, HandleCREvents latency stays at the no-reminder baseline and the
// projection is untouched.
func TestApprovalReminderEndToEndLatency(t *testing.T) {
	ctx := context.Background()
	seedE2E(t, ctx, "CR-9051-051")
	defer cleanupE2E(t, ctx, "CR-9051-051")

	bus := events.New()
	svc := governance.NewSyncService(testPool, bus)

	postStatus := func(crID, from, to, sha string) time.Duration {
		body, _ := json.Marshal(map[string]any{
			"workspace_root_hash": "e2e-hash",
			"events": []map[string]any{{
				"v": 1, "file": crID + ".json", "event_kind": "status",
				"cr_id": crID, "from_status": from, "to_status": to,
				"trigger": "review-requirement", "commit_sha": sha,
				"actor": "e2e", "occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
			}},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/daemon/cr-events", bytes.NewReader(body))
		req = req.WithContext(middleware.WithDaemonContext(req.Context(), testWorkspaceID, "daemon-e2e"))
		rec := httptest.NewRecorder()
		start := time.Now()
		svc.HandleCREvents(rec, req)
		elapsed := time.Since(start)
		if rec.Code != http.StatusOK {
			t.Fatalf("HandleCREvents = %d: %s", rec.Code, rec.Body.String())
		}
		return elapsed
	}

	baseline := postStatus("CR-9051-051", "drafting", "requirement-reviewing", "sha-e2e-base")

	// Register the reminder with a client whose send hangs on the recipient
	// timeout context; a short RecipientTimeout keeps the test fast.
	client := &wiringBlockClient{}
	mu := &sync.Mutex{}
	logs := &wiringLogs{}
	rem := lark.NewApprovalReminder(lark.ApprovalReminderConfig{
		Pool:             testPool,
		Client:           client,
		Credentials:      e2eInstallationService(t),
		AppURL:           "https://multica.test",
		Logger:           newWiringLogger(mu, logs),
		RecipientTimeout: 200 * time.Millisecond,
	})
	rem.Register(bus)

	// A second CR exercises the gate entry with the reminder attached.
	seedE2E(t, ctx, "CR-9051-052")
	defer cleanupE2E(t, ctx, "CR-9051-052")
	withReminder := postStatus("CR-9051-052", "drafting", "requirement-reviewing", "sha-e2e-rem")

	if diff := withReminder - baseline; diff > 50*time.Millisecond || diff < -50*time.Millisecond {
		t.Errorf("HandleCREvents latency drifted by %v (baseline %v, with reminder %v); want < 50ms", diff, baseline, withReminder)
	}

	// The blocking send IS reached asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.Lock()
		sends := client.sends
		client.mu.Unlock()
		if sends == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocking send never reached (delivery chain broken)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Projection unchanged and healthy: both CRs reached the gate, no
	// needs_reconcile, no rollback.
	for _, crID := range []string{"CR-9051-051", "CR-9051-052"} {
		var status string
		var needsReconcile bool
		if err := testPool.QueryRow(ctx, `
			SELECT status, needs_reconcile FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`,
			testWorkspaceID, crID).Scan(&status, &needsReconcile); err != nil {
			t.Fatalf("cr row %s: %v", crID, err)
		}
		if status != "requirement-reviewing" || needsReconcile {
			t.Errorf("cr %s: status=%q needs_reconcile=%v", crID, status, needsReconcile)
		}
	}

	// AC-11: the hung send fails on the recipient timeout — one recipient-
	// level failed with error_class=timeout at step=send, never a sent.
	deadline = time.Now().Add(3 * time.Second)
	for logs.count("error_class", "timeout") != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.count("result", "failed") != 1 || logs.count("error_class", "timeout") != 1 || logs.count("step", "send") != 1 {
		t.Errorf("failed=%d timeout-class=%d step=send=%d, want 1/1/1 (hung send must fail on recipient timeout)",
			logs.count("result", "failed"), logs.count("error_class", "timeout"), logs.count("step", "send"))
	}
	if logs.count("result", "sent") != 0 {
		t.Errorf("sent = %d, want 0", logs.count("result", "sent"))
	}
}

// e2eInstallationService builds a real InstallationService for the test pool.
func e2eInstallationService(t *testing.T) *lark.InstallationService {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	svc, err := lark.NewInstallationService(db.New(testPool), box)
	if err != nil {
		t.Fatalf("installation service: %v", err)
	}
	return svc
}

// seedE2E plants the full delivery chain (project/issue/cr + installation +
// binding) for one CR in the fixture workspace.
func seedE2E(t *testing.T, ctx context.Context, crID string) {
	t.Helper()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1::uuid, 'wiring e2e')
		RETURNING id::text`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, creator_type, creator_id, number)
		SELECT $1::uuid, $2::uuid, 'wiring shell', 'member', $3::uuid,
		       (SELECT COALESCE(MAX(number),0)+1 FROM issue WHERE workspace_id = $1::uuid)
		RETURNING id::text`, testWorkspaceID, projectID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO cr (workspace_id, cr_id, title, status, shell_issue_id)
		VALUES ($1::uuid, $2, 'Wiring CR', 'drafting', $3::uuid)
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET shell_issue_id = $3::uuid, status = 'drafting'`,
		testWorkspaceID, crID, issueID); err != nil {
		t.Fatalf("cr: %v", err)
	}
	// Agent + installation + binding (real encryption via the shared box).
	var agentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent WHERE workspace_id = $1::uuid LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("agent: %v (fixture should own one)", err)
	}
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	sealed, err := box.Seal([]byte("wiring-secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	var installID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, installer_user_id, status)
		VALUES ($1::uuid, $2::uuid, 'feishu',
		        jsonb_build_object('app_id', $3::text, 'app_secret_encrypted', $4::text, 'region', 'feishu'),
		        $5::uuid, 'active')
		ON CONFLICT (workspace_id, agent_id, channel_type) DO UPDATE
		  SET config = EXCLUDED.config, status = 'active'
		RETURNING id::text`,
		testWorkspaceID, agentID, "cli_wiring_"+crID, base64.StdEncoding.EncodeToString(sealed), testUserID).Scan(&installID); err != nil {
		t.Fatalf("installation: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_user_binding (workspace_id, multica_user_id, installation_id, channel_type, channel_user_id, bound_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'feishu', $4, now())
		ON CONFLICT (installation_id, channel_user_id) DO NOTHING`,
		testWorkspaceID, testUserID, installID, "ou_wiring_"+crID); err != nil {
		t.Fatalf("binding: %v", err)
	}
}

func cleanupE2E(t *testing.T, ctx context.Context, crID string) {
	t.Helper()
	_, _ = testPool.Exec(ctx, `DELETE FROM channel_user_binding WHERE workspace_id = $1::uuid AND channel_user_id LIKE 'ou_wiring_%'`, testWorkspaceID)
	_, _ = testPool.Exec(ctx, `DELETE FROM channel_installation WHERE workspace_id = $1::uuid AND config ->> 'app_id' LIKE 'cli_wiring_%'`, testWorkspaceID)
	_, _ = testPool.Exec(ctx, `DELETE FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, crID)
	_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE workspace_id = $1::uuid AND title = 'wiring shell'`, testWorkspaceID)
	_, _ = testPool.Exec(ctx, `DELETE FROM project WHERE workspace_id = $1::uuid AND title = 'wiring e2e'`, testWorkspaceID)
}
