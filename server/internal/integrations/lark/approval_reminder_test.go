package lark

// AIFIRST: CR-2026-051 TASK-05 — reminder skeleton tests (AC-11/AC-12/AC-13
// + parsePayload + zero-value defaults + the read-only map static assertion).
// All log assertions parse slog JSON lines line-by-line (lockedWriter, same
// shape as internal/realtime/hub_test.go) — never strings.Contains on the
// formatted output. No real database: Pool is either nil or a real-but-
// unreachable *pgxpool.Pool (pgxpool.New never dials at construction; a
// query attempt against it would deterministically fail and leave a
// result=failed line, so "no failed line" is the positive zero-DB proof).
//
// TASK-06 appends the real-DB delivery-chain tests below (AC-3/AC-4/AC-5/
// AC-7/AC-10 + BL-1/BL-2 regressions + chooseEffective unit tests).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/protocol"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeReminderCredentials satisfies installationCredentialSource without a DB.
type fakeReminderCredentials struct{}

func (fakeReminderCredentials) DecryptAppSecret(Installation) (string, error) { return "", nil }
func (fakeReminderCredentials) GetInWorkspace(context.Context, pgtype.UUID, pgtype.UUID) (Installation, error) {
	return Installation{}, nil
}

// reminderTestClient is a configurable APIClient for the reminder tests.
type reminderTestClient struct {
	mu            sync.Mutex
	configured    bool
	blockOnConfig chan struct{} // non-nil: IsConfigured blocks until closed
	panicOnConfig bool
	sendCalls     int
	sendErr       error
	sendBlock     chan struct{} // non-nil: SendApprovalReminderCard blocks until closed
	sentParams    []ApprovalReminderParams
}

func (f *reminderTestClient) IsConfigured() bool {
	if f.blockOnConfig != nil {
		<-f.blockOnConfig
	}
	if f.panicOnConfig {
		panic("boom: IsConfigured")
	}
	return f.configured
}
func (f *reminderTestClient) SendInteractiveCard(context.Context, SendCardParams) (string, error) {
	return "", nil
}
func (f *reminderTestClient) PatchInteractiveCard(context.Context, PatchCardParams) error { return nil }
func (f *reminderTestClient) SendTextMessage(context.Context, SendTextParams) (string, error) {
	return "", nil
}
func (f *reminderTestClient) SendMarkdownCard(context.Context, SendMarkdownCardParams) (string, error) {
	return "", nil
}
func (f *reminderTestClient) SendBindingPromptCard(context.Context, BindingPromptParams) error { return nil }
func (f *reminderTestClient) SendApprovalReminderCard(ctx context.Context, p ApprovalReminderParams) error {
	f.mu.Lock()
	f.sendCalls++
	f.sentParams = append(f.sentParams, p)
	f.mu.Unlock()
	if f.sendBlock != nil {
		<-f.sendBlock
	}
	return f.sendErr
}
func (f *reminderTestClient) GetBotInfo(context.Context, InstallationCredentials) (BotInfo, error) {
	return BotInfo{}, nil
}
func (f *reminderTestClient) BatchGetUsers(context.Context, InstallationCredentials, []string) (map[string]string, error) {
	return nil, nil
}
func (f *reminderTestClient) GetMessage(context.Context, InstallationCredentials, string) ([]LarkMessage, error) {
	return nil, nil
}
func (f *reminderTestClient) ListChatMessages(context.Context, InstallationCredentials, ListMessagesParams) ([]LarkMessage, error) {
	return nil, nil
}
func (f *reminderTestClient) DownloadMessageResource(context.Context, InstallationCredentials, DownloadResourceParams) (DownloadedResource, error) {
	return DownloadedResource{}, nil
}
func (f *reminderTestClient) AddMessageReaction(context.Context, AddReactionParams) (string, error) {
	return "", nil
}
func (f *reminderTestClient) DeleteMessageReaction(context.Context, DeleteReactionParams) error { return nil }

// lockedWriter mirrors internal/realtime/hub_test.go's lockedWriter: a
// mutex-guarded buffer so the slog JSON handler can write from the deliver
// goroutine while tests read.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// capturedLogs collects parsed JSON log lines.
type capturedLogs struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (c *capturedLogs) addLine(line []byte) {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return
	}
	c.mu.Lock()
	c.lines = append(c.lines, m)
	c.mu.Unlock()
}

func (c *capturedLogs) count(field, want string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.lines {
		if v, _ := m[field].(string); v == want {
			n++
		}
	}
	return n
}

func (c *capturedLogs) exists(field, want string) bool { return c.count(field, want) > 0 }

func (c *capturedLogs) phaseMissing(phase string) (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var missing string
	n := 0
	for _, m := range c.lines {
		if v, _ := m["phase"].(string); v == phase {
			n++
			if v, ok := m["missing"].(string); ok {
				missing = v
			}
		}
	}
	return missing, n
}

func (c *capturedLogs) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.lines))
	copy(out, c.lines)
	return out
}

// newReminderHarness wires logger capture + an unreachable real pool + healthy
// deps; callers override individual deps per sub-case.
func newReminderHarness(t *testing.T) (*capturedLogs, *ApprovalReminder, *reminderTestClient, *events.Bus) {
	t.Helper()
	logs := &capturedLogs{}
	logger := newRemLogger(logs)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://x:x@127.0.0.1:1/x")
	if err != nil {
		t.Fatalf("unreachable pool construction must succeed (no dial): %v", err)
	}
	t.Cleanup(pool.Close)

	client := &reminderTestClient{configured: true}
	bus := events.New()
	r := NewApprovalReminder(ApprovalReminderConfig{
		Pool: pool, Client: client, Credentials: fakeReminderCredentials{},
		AppURL: "https://multica.test", Logger: logger, MaxInFlight: 8,
	})
	return logs, r, client, bus
}

type parseWriter struct {
	buf  *bytes.Buffer
	mu   *sync.Mutex
	logs *capturedLogs
}

func (w *parseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range bytes.Split(p, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			w.logs.addLine(line)
		}
	}
	return w.buf.Write(p)
}

func newRemLogger(logs *capturedLogs) *slog.Logger {
	var buf bytes.Buffer
	var mu sync.Mutex
	return slog.New(slog.NewJSONHandler(&parseWriter{buf: &buf, mu: &mu, logs: logs}, nil))
}

func gateEvent(workspaceID string) events.Event {
	return events.Event{
		Type:        protocol.EventCRApprovalGateEntered,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		Payload: protocol.ApprovalGateEnteredPayload{
			CRID: "CR-2026-051", Status: "requirement-reviewing",
			EventID: "CR-2026-051:status:abc",
		},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestApprovalReminderDependencyIsolation covers acceptance 1 (a)-(e):
// per-dependency isolation, phase=construct/register bookkeeping, and the
// feishu-disabled skip with zero DB.
func TestApprovalReminderDependencyIsolation(t *testing.T) {
	unreachablePool := func(t *testing.T) *pgxpool.Pool {
		pool, err := pgxpool.New(context.Background(), "postgres://x:x@127.0.0.1:1/x")
		if err != nil {
			t.Fatalf("unreachable pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}

	subcases := []struct {
		name        string
		client      APIClient
		credentials installationCredentialSource
		wantMissing string
		constructN  int
	}{
		{"credentials nil", &reminderTestClient{configured: true}, nil, "credentials", 1},
		{"client nil", nil, fakeReminderCredentials{}, "client", 1},
		{"client not configured", &reminderTestClient{configured: false}, fakeReminderCredentials{}, "", 0},
		{"pool nil", &reminderTestClient{configured: true}, fakeReminderCredentials{}, "pool", 1},
	}
	for _, tc := range subcases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &capturedLogs{}
			var pool *pgxpool.Pool
			if tc.wantMissing != "pool" {
				pool = unreachablePool(t)
			}
			cfg := ApprovalReminderConfig{
				Pool: pool, Client: tc.client, Credentials: tc.credentials,
				AppURL: "https://multica.test", Logger: newRemLogger(logs),
			}
			r := NewApprovalReminder(cfg)
			if r == nil {
				t.Fatal("constructor must never return nil")
			}
			bus := events.New()
			r.Register(bus)
			bus.Publish(gateEvent("ws-1"))

			waitFor(t, "feishu-disabled skip", func() bool {
				return logs.count("reason", reasonFeishuDisabled) == 1
			})

			missing, n := logs.phaseMissing(phaseConstruct)
			if n != tc.constructN {
				t.Errorf("phase=construct lines = %d, want %d", n, tc.constructN)
			}
			if tc.wantMissing != "" && missing != tc.wantMissing {
				t.Errorf("construct missing = %q, want %q", missing, tc.wantMissing)
			}
			if logs.exists("result", "failed") {
				t.Error("no result=failed allowed (zero DB positive proof)")
			}
			if logs.exists("phase", phasePanicRecovered) {
				t.Error("no phase=panic-recovered allowed")
			}
			if logs.count("phase", phaseRegister) != 0 {
				t.Error("Register got a real bus: phase=register must be 0")
			}
			if logs.count("result", "skipped") != 1 {
				t.Errorf("want exactly one feishu-disabled skip, got skipped=%d disabled=%d",
					logs.count("result", "skipped"), logs.count("reason", reasonFeishuDisabled))
			}
		})
	}

	t.Run("nil bus", func(t *testing.T) {
		logs := &capturedLogs{}
		pool := unreachablePool(t)
		cfg := ApprovalReminderConfig{
			Pool: pool, Client: &reminderTestClient{configured: true},
			Credentials: fakeReminderCredentials{}, AppURL: "https://multica.test",
			Logger: newRemLogger(logs),
		}
		r := NewApprovalReminder(cfg)
		r.Register(nil) // must not panic

		if logs.count("phase", phaseRegister) != 1 {
			t.Errorf("phase=register lines = %d, want 1", logs.count("phase", phaseRegister))
		}
		if missing, _ := logs.phaseMissing(phaseRegister); missing != "bus" {
			t.Errorf("register missing = %q, want bus", missing)
		}
		if logs.count("phase", phaseConstruct) != 0 {
			t.Error("deps healthy: phase=construct must be 0")
		}
		// Not subscribed: publishing to a separate real bus yields nothing.
		other := events.New()
		other.Publish(gateEvent("ws-1"))
		time.Sleep(50 * time.Millisecond)
		for _, result := range []string{"sent", "failed", "skipped"} {
			if logs.count("result", result) != 0 {
				t.Errorf("unsubscribed reminder produced result=%s", result)
			}
		}
	})
}

// TestApprovalReminderZeroIOCallback covers AC-11: a blocking IsConfigured
// must not delay the synchronous publish.
func TestApprovalReminderZeroIOCallback(t *testing.T) {
	_, r, client, bus := newReminderHarness(t)
	client.blockOnConfig = make(chan struct{})
	r.Register(bus)

	start := time.Now()
	bus.Publish(gateEvent("ws-1"))
	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Errorf("sync callback took %v, want < 50ms (must be zero-I/O)", elapsed)
	}
	close(client.blockOnConfig)
}

// TestApprovalReminderOverloadDiscard covers AC-13 overload: one in-flight
// slot, second event → overloaded skip, no second goroutine.
func TestApprovalReminderOverloadDiscard(t *testing.T) {
	logs, r, client, bus := newReminderHarness(t)
	r2 := NewApprovalReminder(ApprovalReminderConfig{
		Pool: r.pool, Client: client, Credentials: r.credentials,
		AppURL: r.appURL, Logger: r.logger, MaxInFlight: 1,
	})
	client.blockOnConfig = make(chan struct{})
	r2.Register(bus)

	bus.Publish(gateEvent("ws-1")) // occupies the slot
	bus.Publish(gateEvent("ws-1")) // must be dropped

	waitFor(t, "overloaded skip", func() bool {
		return logs.count("reason", reasonOverloaded) == 1
	})
	close(client.blockOnConfig)
	// The in-flight deliver proceeds to the (unreachable) pool and fails —
	// that is the healthy-dependency path, not the dropped one.
	waitFor(t, "in-flight settle", func() bool {
		return logs.count("result", "failed") == 1
	})
	client.mu.Lock()
	calls := client.sendCalls
	client.mu.Unlock()
	if calls != 0 {
		t.Errorf("stub send calls = %d (deliver must fail at the pool, not the client)", calls)
	}
}

// TestApprovalReminderPanicRecovery covers AC-13: a panic inside the async
// body is recovered, logged with phase=panic-recovered, and the semaphore is
// released so later events still run.
func TestApprovalReminderPanicRecovery(t *testing.T) {
	logs, r, client, bus := newReminderHarness(t)
	r.Register(bus)

	client.panicOnConfig = true
	bus.Publish(gateEvent("ws-1"))
	waitFor(t, "panic recovery", func() bool {
		return logs.count("phase", phasePanicRecovered) == 1
	})
	if logs.count("phase", phaseConstruct) != 0 {
		t.Error("deps healthy: phase=construct must be 0")
	}
	if logs.count("phase", phaseRegister) != 0 {
		t.Error("real bus: phase=register must be 0")
	}

	// Semaphore released: a healthy follow-up event is processed.
	client.panicOnConfig = false
	client.configured = true
	bus.Publish(gateEvent("ws-1"))
	waitFor(t, "follow-up reaches the pool", func() bool {
		return logs.count("result", "failed") == 1
	})
}

// TestApprovalReminderPreflightOrder covers the availability→base-URL order:
// both absent → feishu-disabled wins; only appURL absent → app-url-missing.
func TestApprovalReminderPreflightOrder(t *testing.T) {
	t.Run("disabled wins over missing appURL", func(t *testing.T) {
		logs, r, _, bus := newReminderHarness(t)
		r.appURL = ""
		r.client = &reminderTestClient{configured: false}
		r.Register(bus)
		bus.Publish(gateEvent("ws-1"))
		waitFor(t, "feishu-disabled", func() bool {
			return logs.count("reason", reasonFeishuDisabled) == 1
		})
		if logs.count("reason", reasonAppURLMissing) != 0 {
			t.Error("app-url-missing must not appear when feishu is disabled")
		}
	})

	t.Run("app url missing", func(t *testing.T) {
		logs, r, _, bus := newReminderHarness(t)
		r.appURL = ""
		r.Register(bus)
		bus.Publish(gateEvent("ws-1"))
		waitFor(t, "app-url-missing", func() bool {
			return logs.count("reason", reasonAppURLMissing) == 1
		})
		if logs.exists("result", "failed") {
			t.Error("zero DB: no result=failed allowed")
		}
	})
}

// TestApprovalReminderParsePayload covers the payload validation rules.
func TestApprovalReminderParsePayload(t *testing.T) {
	good := protocol.ApprovalGateEnteredPayload{CRID: "CR-1", Status: "s", EventID: "e"}
	if _, ok := parsePayload(good); !ok {
		t.Error("valid payload must parse")
	}
	if _, ok := parsePayload(map[string]any{"cr_id": "CR-1"}); ok {
		t.Error("map payload must be rejected (typed assertion)")
	}
	if _, ok := parsePayload(struct{ X int }{1}); ok {
		t.Error("foreign type must be rejected")
	}
	for name, p := range map[string]protocol.ApprovalGateEnteredPayload{
		"empty cr_id":    {Status: "s", EventID: "e"},
		"empty status":   {CRID: "c", EventID: "e"},
		"empty event_id": {CRID: "c", Status: "s"},
	} {
		if _, ok := parsePayload(p); ok {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// TestApprovalReminderZeroValueDefaults covers the documented zero-value
// degradation and explicit values (observable via the overload boundary).
func TestApprovalReminderZeroValueDefaults(t *testing.T) {
	r := NewApprovalReminder(ApprovalReminderConfig{
		Client: &reminderTestClient{configured: true},
		Credentials: fakeReminderCredentials{},
	})
	if cap(r.sem) != 8 {
		t.Errorf("default MaxInFlight = %d, want 8", cap(r.sem))
	}
	if r.eventTimeout != 60*time.Second {
		t.Errorf("default EventTimeout = %v, want 60s", r.eventTimeout)
	}
	if r.recipientTimeout != 10*time.Second {
		t.Errorf("default RecipientTimeout = %v, want 10s", r.recipientTimeout)
	}

	explicit := NewApprovalReminder(ApprovalReminderConfig{
		Client: &reminderTestClient{configured: true},
		Credentials: fakeReminderCredentials{},
		MaxInFlight: 3, EventTimeout: 5 * time.Second, RecipientTimeout: 1 * time.Second,
	})
	if cap(explicit.sem) != 3 {
		t.Errorf("explicit MaxInFlight = %d, want 3", cap(explicit.sem))
	}
	if explicit.eventTimeout != 5*time.Second || explicit.recipientTimeout != 1*time.Second {
		t.Errorf("explicit timeouts = %v/%v", explicit.eventTimeout, explicit.recipientTimeout)
	}
}

// TestApprovalReminderStageLabelsReadOnly is the static assertion for the
// unexported read-only map (architecture review cycle 2, suggestion 3).
func TestApprovalReminderStageLabelsReadOnly(t *testing.T) {
	raw, err := os.ReadFile("approval_reminder.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "var approvalGateStageLabels = map[string]string{") {
		t.Fatal("approvalGateStageLabels must be a single initialized declaration")
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "approvalGateStageLabels[") && strings.Contains(trimmed, "] =") {
			t.Error("static violation: index write to approvalGateStageLabels")
		}
		if strings.Contains(trimmed, "approvalGateStageLabels") && strings.Contains(trimmed, " = map[string]string") && !strings.HasPrefix(trimmed, "var ") {
			t.Error("static violation: reassignment of approvalGateStageLabels")
		}
	}
	if strings.Contains(src, "delete(approvalGateStageLabels") {
		t.Error("static violation: delete on approvalGateStageLabels")
	}
	if strings.Contains(src, "ApprovalGateStageLabels") {
		t.Error("map must stay unexported")
	}
}

// TestStageLabel covers the display mapping and the raw-status fallback.
func TestStageLabel(t *testing.T) {
	if got := stageLabel("requirement-reviewing"); got != "需求审批" {
		t.Errorf("stageLabel(requirement-reviewing) = %q", got)
	}
	if got := stageLabel("developing"); got != "developing" {
		t.Errorf("stageLabel fallback = %q", got)
	}
	if len(approvalGateStageLabels) != 4 {
		t.Errorf("stage label set = %d entries, want the four gates", len(approvalGateStageLabels))
	}
	for k := range approvalGateStageLabels {
		if stageLabel(k) == k {
			t.Errorf("gate status %q must map to a display label", k)
		}
	}
}

var _ = fmt.Sprintf

// ============================================================================
// TASK-06 — real-DB delivery-chain tests (require the migrated Postgres via
// channelScopeTestDB; --- SKIP counts as "not tested", never a pass).
// ============================================================================

const (
	remWS        = "b7f00000-0000-4000-8000-000000000001" // primary fixture workspace
	remWS2       = "b7f00000-0000-4000-8000-000000000002" // cross-workspace fixture
	remAgent     = "b7f00000-0000-4000-8000-00000000000a"
	remAgent2    = "b7f00000-0000-4000-8000-00000000000c"
	remAgent3    = "b7f00000-0000-4000-8000-00000000000d"
	remInstaller = "b7f00000-0000-4000-8000-00000000000b"
)

// remDB is the real-DB fixture bundle for the delivery-chain tests.
type remDB struct {
	t      *testing.T
	pool   *pgxpool.Pool
	box    *secretbox.Box
	svc    *InstallationService
	client *reminderTestClient
	logs   *capturedLogs
	rem    *ApprovalReminder
	bus    *events.Bus
}

func bytes32(s string) []byte {
	b := []byte(s)
	for len(b) < 32 {
		b = append(b, '0')
	}
	return b[:32]
}

func strPtr(s string) *string { return &s }

func dbDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
}

func newRemDB(t *testing.T) *remDB {
	t.Helper()
	pool := channelScopeTestDB(t)
	ctx := context.Background()
	box, err := secretbox.New(bytes32("rem-test-key-00000000000000000000"))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	svc, err := NewInstallationService(db.New(pool), box)
	if err != nil {
		t.Fatalf("installation service: %v", err)
	}
	for _, ws := range []string{remWS, remWS2} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO workspace (id, name, slug, description)
			VALUES ($1::uuid, 'reminder ' || $1::text, 'rem-' || $1::text, '')
			ON CONFLICT (id) DO NOTHING`, ws); err != nil {
			t.Fatalf("workspace fixture: %v", err)
		}
	}
	for _, agent := range []string{remAgent, remAgent2, remAgent3} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent (id, workspace_id, name, runtime_mode)
			VALUES ($1::uuid, $2::uuid, 'reminder agent ' || $1::text, 'local')
			ON CONFLICT (id) DO NOTHING`, agent, remWS); err != nil {
			t.Fatalf("agent fixture: %v", err)
		}
	}
	logs := &capturedLogs{}
	client := &reminderTestClient{configured: true}
	rem := NewApprovalReminder(ApprovalReminderConfig{
		Pool: pool, Client: client, Credentials: svc,
		AppURL: "https://multica.test", Logger: newRemLogger(logs),
	})
	bus := events.New()
	rem.Register(bus)
	t.Cleanup(func() { cleanRemDB(ctx, pool) })
	return &remDB{t: t, pool: pool, box: box, svc: svc, client: client, logs: logs, rem: rem, bus: bus}
}

func cleanRemDB(ctx context.Context, pool *pgxpool.Pool) {
	ws := []any{remWS, remWS2}
	for _, q := range []string{
		`DELETE FROM channel_user_binding WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM channel_installation WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM cr WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM cr_sync_event WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM issue WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM pipeline_node_run WHERE run_id IN (SELECT id FROM pipeline_run WHERE workspace_id IN ($1, $2))`,
		`DELETE FROM pipeline_run WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM member WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM project WHERE workspace_id IN ($1, $2)`,
		`DELETE FROM "user" WHERE email LIKE 'rem-%'`,
	} {
		switch {
		case strings.Contains(q, "agent"):
			for _, agent := range []string{remAgent, remAgent2, remAgent3} {
				_, _ = pool.Exec(ctx, q, agent)
			}
		case strings.Contains(q, "rem-%"):
			_, _ = pool.Exec(ctx, q)
		default:
			_, _ = pool.Exec(ctx, q, ws...)
		}
	}
}

func (f *remDB) addUser(ws, role, email string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO "user" (email, name) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET updated_at = now()
		RETURNING id::text`, email, email).Scan(&id); err != nil {
		f.t.Fatalf("user fixture: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1::uuid, $2::uuid, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = $3`, ws, id, role); err != nil {
		f.t.Fatalf("member fixture: %v", err)
	}
	return id
}

func (f *remDB) addProjectIssue(ws string) (projectID, issueID string) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO project (workspace_id, title) VALUES ($1::uuid, 'reminder project')
		RETURNING id::text`, ws).Scan(&projectID); err != nil {
		f.t.Fatalf("project fixture: %v", err)
	}
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, project_id, title, creator_type, creator_id, number)
		SELECT $1::uuid, $2::uuid, 'reminder shell', 'agent', $3::uuid,
		       (SELECT COALESCE(MAX(number),0)+1 FROM issue WHERE workspace_id = $1::uuid)
		RETURNING id::text`, ws, projectID, remAgent).Scan(&issueID); err != nil {
		f.t.Fatalf("issue fixture: %v", err)
	}
	return
}

func (f *remDB) addCR(ws, crID string, shellIssueID *string) {
	f.t.Helper()
	if shellIssueID == nil {
		if _, err := f.pool.Exec(context.Background(), `
			INSERT INTO cr (workspace_id, cr_id, title, status) VALUES ($1::uuid, $2, 'Reminder CR', 'requirement-reviewing')
			ON CONFLICT (workspace_id, cr_id) DO UPDATE SET shell_issue_id = NULL, status = 'requirement-reviewing'`,
			ws, crID); err != nil {
			f.t.Fatalf("cr fixture: %v", err)
		}
		return
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO cr (workspace_id, cr_id, title, status, shell_issue_id) VALUES ($1::uuid, $2, 'Reminder CR', 'requirement-reviewing', $3::uuid)
		ON CONFLICT (workspace_id, cr_id) DO UPDATE SET shell_issue_id = $3::uuid, status = 'requirement-reviewing'`,
		ws, crID, *shellIssueID); err != nil {
		f.t.Fatalf("cr fixture: %v", err)
	}
}

// addInstall inserts an installation whose app_secret is sealed with sealBox
// (defaults to the fixture's real box); pass a different box to force a
// decryption failure later.
func (f *remDB) addInstall(ws, appID, secret, status string, sealBox *secretbox.Box) string {
	return f.addInstallOn(ws, remAgent, appID, secret, status, sealBox)
}

func (f *remDB) addInstallOn(ws, agentID, appID, secret, status string, sealBox *secretbox.Box) string {
	f.t.Helper()
	if sealBox == nil {
		sealBox = f.box
	}
	sealed, err := sealBox.Seal([]byte(secret))
	if err != nil {
		f.t.Fatalf("seal: %v", err)
	}
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, installer_user_id, status)
		VALUES ($1::uuid, $2::uuid, 'feishu',
		        jsonb_build_object('app_id', $3::text, 'app_secret_encrypted', $4::text, 'region', 'feishu'),
		        $5::uuid, $6)
		ON CONFLICT (workspace_id, agent_id, channel_type) DO UPDATE
		  SET config = EXCLUDED.config, status = EXCLUDED.status
		RETURNING id::text`,
		ws, agentID, appID, base64.StdEncoding.EncodeToString(sealed), remInstaller, status).Scan(&id); err != nil {
		f.t.Fatalf("installation fixture: %v", err)
	}
	return id
}

func (f *remDB) addBinding(ws, userID, installationID, openID string, boundAt time.Time) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO channel_user_binding (workspace_id, multica_user_id, installation_id, channel_type, channel_user_id, bound_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'feishu', $4, $5)`,
		ws, userID, installationID, openID, boundAt); err != nil {
		f.t.Fatalf("binding fixture: %v", err)
	}
}

func (f *remDB) publish(crID string) {
	f.t.Helper()
	f.bus.Publish(gateEvent2(remWS, crID))
}

func gateEvent2(ws, crID string) events.Event {
	return events.Event{
		Type:        protocol.EventCRApprovalGateEntered,
		WorkspaceID: ws,
		ActorType:   "system",
		Payload: protocol.ApprovalGateEnteredPayload{
			CRID: crID, Status: "requirement-reviewing", EventID: crID + ":status:rem",
		},
	}
}

func (f *remDB) settled() {
	f.t.Helper()
	waitFor(f.t, "log quiescence", func() bool {
		f.logs.mu.Lock()
		n := len(f.logs.lines)
		f.logs.mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		f.logs.mu.Lock()
		m := len(f.logs.lines)
		f.logs.mu.Unlock()
		return n == m
	})
}

// TestApprovalReminderAC3HappyPath: 3 approvers, 2 with valid bindings → 2
// cards; double-binding uses the newest; two users sharing one open_id → 1.
func TestApprovalReminderAC3HappyPath(t *testing.T) {
	f := newRemDB(t)
	u1 := f.addUser(remWS, "owner", "rem-owner1@test")
	u2 := f.addUser(remWS, "admin", "rem-admin2@test")
	u3 := f.addUser(remWS, "admin", "rem-admin3@test")
	projectID, issueID := f.addProjectIssue(remWS)
	f.addCR(remWS, "CR-9001-001", &issueID)
	inst := f.addInstall(remWS, "cli_rem_happy", "s3cret", "active", nil)
	instShared := f.addInstallOn(remWS, remAgent2, "cli_rem_happy_shared", "s3cret", "active", nil)
	base := time.Now().Add(-2 * time.Hour)
	// u1: two bindings, newest open_id must win.
	f.addBinding(remWS, u1, inst, "ou_old", base)
	f.addBinding(remWS, u1, inst, "ou_new", base.Add(time.Hour))
	// u2 and u3 share one open_id (distinct installations — the binding
	// UNIQUE is (installation_id, channel_user_id)) → only one card for both.
	f.addBinding(remWS, u2, inst, "ou_shared", base)
	f.addBinding(remWS, u3, instShared, "ou_shared", base)

	f.publish("CR-9001-001")
	waitFor(t, "two sent lines", func() bool { return f.logs.count("result", "sent") == 2 })
	f.settled()
	if f.logs.count("result", "sent") != 2 {
		t.Fatalf("sent = %d, want 2", f.logs.count("result", "sent"))
	}
	f.client.mu.Lock()
	params := append([]ApprovalReminderParams(nil), f.client.sentParams...)
	f.client.mu.Unlock()
	if len(params) != 2 {
		t.Fatalf("client calls = %d, want 2", len(params))
	}
	openIDs := map[string]bool{}
	for _, p := range params {
		openIDs[string(p.OpenID)] = true
	}
	if !openIDs["ou_new"] {
		t.Errorf("u1 must send to the newest open_id (ou_new), got %v", openIDs)
	}
	if !openIDs["ou_shared"] || len(openIDs) != 2 {
		t.Errorf("shared open_id must be attempted once: %v", openIDs)
	}
	wantURL := "https://multica.test/rem-" + remWS + "/projects/" + projectID + "?tab=chat"
	for _, p := range params {
		if p.ApproveURL != wantURL {
			t.Errorf("ApproveURL = %q, want %q (AC-5 CTA)", p.ApproveURL, wantURL)
		}
		if p.CRID != "CR-9001-001" || p.StageLabel != "需求审批" {
			t.Errorf("card params = %+v", p)
		}
	}
}

// TestApprovalReminderBL2FirstAttemptFails: same open_id, first send fails →
// client called once, second user produces no log line at all.
func TestApprovalReminderBL2FirstAttemptFails(t *testing.T) {
	f := newRemDB(t)
	u1 := f.addUser(remWS, "owner", "rem-bl2a@test")
	u2 := f.addUser(remWS, "admin", "rem-bl2b@test")
	_, issueID := f.addProjectIssue(remWS)
	f.addCR(remWS, "CR-9001-002", &issueID)
	inst := f.addInstall(remWS, "cli_bl2", "s3cret", "active", nil)
	inst2 := f.addInstallOn(remWS, remAgent2, "cli_bl2b", "s3cret", "active", nil)
	f.addBinding(remWS, u1, inst, "ou_dup", time.Now())
	f.addBinding(remWS, u2, inst2, "ou_dup", time.Now())
	f.client.sendErr = errors.New("lark transport failure")

	f.publish("CR-9001-002")
	waitFor(t, "one failed send", func() bool { return f.logs.count("result", "failed") == 1 })
	f.settled()
	f.client.mu.Lock()
	calls := f.client.sendCalls
	f.client.mu.Unlock()
	if calls != 1 {
		t.Fatalf("client calls = %d, want 1 (BL-2: one attempt per open_id)", calls)
	}
	if f.logs.count("result", "failed") != 1 {
		t.Errorf("failed = %d, want 1", f.logs.count("result", "failed"))
	}
	// Second user: no recipient-level line whatsoever.
	for _, m := range f.logs.all() {
		if v, _ := m["recipient_user_id"].(string); v == u2 {
			t.Errorf("second user must produce no log line, got %v", m)
		}
	}
}

// TestApprovalReminderBL2DecryptAndTimeout: first-attempt failures via decrypt
// and via timeout both still block the second user.
func TestApprovalReminderBL2DecryptAndTimeout(t *testing.T) {
	t.Run("first decrypt fails", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-dc1@test")
		u2 := f.addUser(remWS, "admin", "rem-dc2@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-003", &issueID)
		otherBox, err := secretbox.New(bytes32("rem-other-key-00000000000000000000"))
		if err != nil {
			t.Fatalf("other box: %v", err)
		}
		inst := f.addInstall(remWS, "cli_dc", "s3cret", "active", otherBox)
		inst2 := f.addInstallOn(remWS, remAgent2, "cli_dcb", "s3cret", "active", otherBox)
		f.addBinding(remWS, u1, inst, "ou_dc", time.Now())
		f.addBinding(remWS, u2, inst2, "ou_dc", time.Now())

		f.publish("CR-9001-003")
		waitFor(t, "decrypt failure", func() bool {
			return f.logs.count("step", stepCredentialDecrypt) == 1
		})
		f.settled()
		if f.logs.count("result", "failed") != 1 {
			t.Errorf("failed = %d, want 1", f.logs.count("result", "failed"))
		}
		for _, m := range f.logs.all() {
			if v, _ := m["recipient_user_id"].(string); v == u2 {
				t.Errorf("second user must be silent, got %v", m)
			}
		}
	})

	t.Run("first send times out", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-to1@test")
		u2 := f.addUser(remWS, "admin", "rem-to2@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-004", &issueID)
		inst := f.addInstall(remWS, "cli_to", "s3cret", "active", nil)
		inst2 := f.addInstallOn(remWS, remAgent2, "cli_tob", "s3cret", "active", nil)
		f.addBinding(remWS, u1, inst, "ou_to", time.Now())
		f.addBinding(remWS, u2, inst2, "ou_to", time.Now())
		f.client.sendErr = context.DeadlineExceeded

		f.publish("CR-9001-004")
		waitFor(t, "timeout failure", func() bool {
			return f.logs.count("error_class", errorClassTimeout) == 1
		})
		f.settled()
		if f.logs.count("error_class", errorClassTimeout) != 1 {
			t.Errorf("timeout-class failures = %d, want 1", f.logs.count("error_class", errorClassTimeout))
		}
		for _, m := range f.logs.all() {
			if v, _ := m["recipient_user_id"].(string); v == u2 {
				t.Errorf("second user must be silent, got %v", m)
			}
		}
	})
}

// TestApprovalReminderBL1HydrationFourStates covers BL-1's four hydration
// states + cross-workspace installation lookup.
func TestApprovalReminderBL1HydrationFourStates(t *testing.T) {
	t.Run("happy credentials match db", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-hyd1@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-005", &issueID)
		inst := f.addInstall(remWS, "cli_hyd", "correct-secret", "active", nil)
		f.addBinding(remWS, u1, inst, "ou_hyd", time.Now())

		f.publish("CR-9001-005")
		waitFor(t, "sent", func() bool { return f.logs.count("result", "sent") == 1 })
		f.client.mu.Lock()
		p := f.client.sentParams[0]
		f.client.mu.Unlock()
		if p.InstallationID.AppID != "cli_hyd" || p.InstallationID.AppSecret != "correct-secret" {
			t.Errorf("credentials mismatch: app=%q secret=%q", p.InstallationID.AppID, p.InstallationID.AppSecret)
		}
	})

	t.Run("installation missing", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-hyd2@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-006", &issueID)
		f.addBinding(remWS, u1, "b7f00000-0000-4000-8000-00000000dead", "ou_orphan", time.Now())

		f.publish("CR-9001-006")
		waitFor(t, "installation-missing", func() bool {
			return f.logs.count("reason", reasonInstallationMissing) == 1
		})
		if f.logs.count("result", "skipped") != 1 {
			t.Errorf("skipped = %d, want 1", f.logs.count("result", "skipped"))
		}
	})

	t.Run("revoked after classification", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-hyd3@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-007", &issueID)
		inst := f.addInstall(remWS, "cli_rev", "s3cret", "revoked", nil)
		f.addBinding(remWS, u1, inst, "ou_rev", time.Now())

		f.publish("CR-9001-007")
		waitFor(t, "installation-revoked", func() bool {
			return f.logs.count("reason", reasonInstallationRevoked) == 1
		})
	})

	t.Run("cross-workspace installation", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-hyd4@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-008", &issueID)
		// Installation lives in the other workspace; the binding points at it
		// from the anchor workspace. chooseEffective flags workspace-mismatch
		// before hydration is attempted (ci.workspace_id != anchor).
		inst := f.addInstall(remWS2, "cli_xws", "s3cret", "active", nil)
		f.addBinding(remWS, u1, inst, "ou_xws", time.Now())

		f.publish("CR-9001-008")
		waitFor(t, "workspace-mismatch", func() bool {
			return f.logs.count("reason", reasonWorkspaceMismatch) == 1
		})
	})
}

// TestApprovalReminderAC4FourCases covers the four no-send cases with
// distinguishable reasons and the scope rules (case ① is event-level).
func TestApprovalReminderAC4FourCases(t *testing.T) {
	t.Run("all-member workspace", func(t *testing.T) {
		f := newRemDB(t)
		u := f.addUser(remWS, "member", "rem-mbr@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-010", &issueID)
		inst := f.addInstall(remWS, "cli_mbr", "s3cret", "active", nil)
		f.addBinding(remWS, u, inst, "ou_mbr", time.Now())

		f.publish("CR-9001-010")
		waitFor(t, "no-approver skip", func() bool {
			return f.logs.count("reason", reasonNoApprover) == 1
		})
		f.settled()
		f.client.mu.Lock()
		calls := f.client.sendCalls
		f.client.mu.Unlock()
		if calls != 0 {
			t.Errorf("client calls = %d, want 0", calls)
		}
		for _, m := range f.logs.all() {
			if _, has := m["recipient_user_id"]; has {
				t.Errorf("event-level skip must carry no recipient fields: %v", m)
			}
			if _, has := m["recipient_open_id"]; has {
				t.Errorf("event-level skip must carry no recipient fields: %v", m)
			}
		}
	})

	t.Run("mixed workspace sends to admin only", func(t *testing.T) {
		f := newRemDB(t)
		admin := f.addUser(remWS, "admin", "rem-mix-admin@test")
		member := f.addUser(remWS, "member", "rem-mix-member@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-011", &issueID)
		inst := f.addInstall(remWS, "cli_mix", "s3cret", "active", nil)
		f.addBinding(remWS, admin, inst, "ou_admin", time.Now())
		f.addBinding(remWS, member, inst, "ou_member", time.Now())

		f.publish("CR-9001-011")
		waitFor(t, "one sent", func() bool { return f.logs.count("result", "sent") == 1 })
		f.settled()
		f.client.mu.Lock()
		calls := f.client.sendCalls
		params := append([]ApprovalReminderParams(nil), f.client.sentParams...)
		f.client.mu.Unlock()
		if calls != 1 {
			t.Fatalf("client calls = %d, want 1", calls)
		}
		if string(params[0].OpenID) != "ou_admin" {
			t.Errorf("receive_id = %q, want ou_admin", params[0].OpenID)
		}
		for _, m := range f.logs.all() {
			if v, _ := m["recipient_user_id"].(string); v == member {
				t.Errorf("member must never enter the recipient set: %v", m)
			}
		}
	})

	t.Run("recipient-level reasons", func(t *testing.T) {
		f := newRemDB(t)
		f.addUser(remWS, "owner", "rem-rc1@test") // no binding → binding-missing
		u2 := f.addUser(remWS, "owner", "rem-rc2@test") // revoked install
		u3 := f.addUser(remWS, "owner", "rem-rc3@test") // orphan binding
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-012", &issueID)
		instRev := f.addInstall(remWS, "cli_rc_rev", "s3cret", "revoked", nil)
		f.addBinding(remWS, u2, instRev, "ou_rc2", time.Now())
		f.addBinding(remWS, u3, "b7f00000-0000-4000-8000-00000000dead", "ou_rc3", time.Now())

		f.publish("CR-9001-012")
		waitFor(t, "three recipient skips", func() bool {
			return f.logs.count("result", "skipped") == 3
		})
		f.settled()
		reasons := map[string]int{}
		for _, m := range f.logs.all() {
			if r, _ := m["reason"].(string); r != "" {
				reasons[r]++
				if _, has := m["recipient_user_id"]; !has {
					t.Errorf("recipient-level skip %s must carry recipient_user_id: %v", r, m)
				}
			}
		}
		if reasons[reasonBindingMissing] != 1 || reasons[reasonInstallationRevoked] != 1 || reasons[reasonInstallationMissing] != 1 {
			t.Errorf("reasons = %v, want one each of binding-missing/installation-revoked/installation-missing", reasons)
		}
		if reasons[reasonNoApprover] != 0 {
			t.Errorf("recipient-level no-approver must never appear: %v", reasons)
		}
		valid := map[string]bool{
			reasonProjectUnresolved: true, reasonWorkspaceMismatch: true, reasonNoApprover: true,
			reasonBindingMissing: true, reasonInstallationRevoked: true, reasonInstallationMissing: true,
			reasonAppURLMissing: true, reasonFeishuDisabled: true, reasonOverloaded: true,
		}
		for r := range reasons {
			if !valid[r] {
				t.Errorf("reason %q outside the FR-8.2 closed set", r)
			}
		}
	})
}

// TestApprovalReminderAC10CrossWorkspace covers the cross-workspace negative
// matrix + the forged-payload BL-3 rule + the static SQL-anchor check.
func TestApprovalReminderAC10CrossWorkspace(t *testing.T) {
	t.Run("issue in other workspace", func(t *testing.T) {
		f := newRemDB(t)
		f.addUser(remWS, "owner", "rem-xws1@test")
		_, foreignIssue := f.addProjectIssue(remWS2) // issue+project live in remWS2
		f.addCR(remWS, "CR-9001-020", &foreignIssue)

		f.publish("CR-9001-020")
		waitFor(t, "workspace-mismatch", func() bool {
			return f.logs.count("reason", reasonWorkspaceMismatch) == 1
		})
		if f.logs.count("result", "sent") != 0 {
			t.Error("zero sends expected")
		}
	})

	t.Run("forged payload shell issue id", func(t *testing.T) {
		f := newRemDB(t)
		_, issueID := f.addProjectIssue(remWS2) // cr row points at the foreign issue
		f.addCR(remWS, "CR-9001-021", &issueID)
		// Payload claims yet another workspace's issue; the cr row is the
		// only resolution path (BL-3: payload ShellIssueID is not a query
		// input).
		f.bus.Publish(events.Event{
			Type: protocol.EventCRApprovalGateEntered, WorkspaceID: remWS, ActorType: "system",
			Payload: protocol.ApprovalGateEnteredPayload{
				CRID: "CR-9001-021", Status: "requirement-reviewing",
				EventID:     "CR-9001-021:status:rem",
				ShellIssueID: strPtr("b7f00000-0000-4000-8000-0000000000ff"),
			},
		})
		waitFor(t, "workspace-mismatch via cr row", func() bool {
			return f.logs.count("reason", reasonWorkspaceMismatch) == 1
		})
		if f.logs.count("result", "sent") != 0 {
			t.Error("zero sends expected (payload shell_issue_id is not a query input)")
		}
	})

	t.Run("cr without shell issue", func(t *testing.T) {
		f := newRemDB(t)
		f.addUser(remWS, "owner", "rem-xws3@test")
		f.addCR(remWS, "CR-9001-022", nil)

		f.publish("CR-9001-022")
		waitFor(t, "project-unresolved", func() bool {
			return f.logs.count("reason", reasonProjectUnresolved) == 1
		})
		if f.logs.count("result", "sent") != 0 {
			t.Error("zero sends expected")
		}
	})

	t.Run("cr row missing in anchor workspace", func(t *testing.T) {
		f := newRemDB(t)
		f.addUser(remWS, "owner", "rem-xws4@test")
		// No cr row at all for this id: the reason query itself returns
		// pgx.ErrNoRows, which must resolve to project-unresolved — never
		// workspace-mismatch (SDD §4.4 / TASK-06 point 2: null shell or no
		// row ⇒ project-unresolved).
		f.publish("CR-9001-023")
		waitFor(t, "project-unresolved for missing cr row", func() bool {
			return f.logs.count("reason", reasonProjectUnresolved) == 1
		})
		if f.logs.count("reason", reasonWorkspaceMismatch) != 0 {
			t.Error("a missing cr row must not masquerade as workspace-mismatch")
		}
		if f.logs.count("result", "sent") != 0 {
			t.Error("zero sends expected")
		}
	})

	t.Run("static workspace anchor in every query", func(t *testing.T) {
		raw, err := os.ReadFile("approval_reminder.go")
		if err != nil {
			t.Fatalf("read source: %v", err)
		}
		src := string(raw)
		segments := strings.Split(src, "`")
		for i, seg := range segments {
			if !strings.Contains(seg, "SELECT") && !strings.Contains(seg, "UPDATE") && !strings.Contains(seg, "INSERT") {
				continue
			}
			if !strings.Contains(seg, "workspace_id") {
				t.Errorf("SQL segment %d has no workspace anchor: %q", i, seg)
			}
		}
	})
}

// TestApprovalReminderAC7ForcedDBFailures covers event-level and
// recipient-level forced DB failures with executable injection.
func TestApprovalReminderAC7ForcedDBFailures(t *testing.T) {
	t.Run("project chain failure via closed pool", func(t *testing.T) {
		f := newRemDB(t)
		f.addUser(remWS, "owner", "rem-fail1@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-030", &issueID)

		closed, err := pgxpool.New(context.Background(), dbDSN())
		if err != nil {
			t.Fatalf("closed pool: %v", err)
		}
		closed.Close() // ErrClosedPool on acquire: an error, not a panic
		f.rem.pool = closed

		f.publish("CR-9001-030")
		waitFor(t, "project-chain failure", func() bool {
			return f.logs.count("step", stepProjectChain) == 1
		})
		f.settled()
		if f.logs.count("result", "failed") != 1 {
			t.Errorf("failed = %d, want 1", f.logs.count("result", "failed"))
		}
		if f.logs.count("error_class", errorClassOther) != 1 {
			t.Errorf("error_class=other = %d, want 1", f.logs.count("error_class", errorClassOther))
		}
		for _, m := range f.logs.all() {
			if v, _ := m["step"].(string); v == stepProjectChain {
				if _, has := m["recipient_user_id"]; has {
					t.Errorf("event-level failure must carry no recipient fields: %v", m)
				}
				if _, has := m["reason"]; has {
					t.Errorf("failed must never carry a reason: %v", m)
				}
			}
		}
	})

	t.Run("binding query failure via private schema shadow", func(t *testing.T) {
		f := newRemDB(t)
		u1 := f.addUser(remWS, "owner", "rem-fail2@test")
		_, issueID := f.addProjectIssue(remWS)
		f.addCR(remWS, "CR-9001-031", &issueID)
		inst := f.addInstall(remWS, "cli_shadow", "s3cret", "active", nil)
		f.addBinding(remWS, u1, inst, "ou_shadow", time.Now())

		// Step 1: uniquely named private schema; no variable-length input
		// (plan §5.7 BL-2), bounded to 49 bytes. CREATE once, reuse the name
		// for search_path and DROP.
		schema := "ac7_binding_shadow_" + fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
		schemaIdent := pgx.Identifier{schema}.Sanitize()
		if len(schema) > 63 {
			t.Fatalf("schema name %d bytes > 63", len(schema))
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(schema) {
			t.Fatalf("schema name %q outside [a-z][a-z0-9_]*", schema)
		}
		ctx := context.Background()
		if _, err := f.pool.Exec(ctx, `CREATE SCHEMA `+schemaIdent); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				t.Skipf("test role lacks CREATE privilege (42501); skipping, not a pass")
			}
			t.Fatalf("CREATE SCHEMA: %v", err)
		}
		t.Cleanup(func() {
			if _, err := f.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schemaIdent+` CASCADE`); err != nil {
				t.Logf("drop shadow schema: %v", err) // correctness never depends on this
			}
		})
		// Column-incompatible same-name shadow table; public untouched.
		if _, err := f.pool.Exec(ctx, `CREATE TABLE `+schemaIdent+`.channel_user_binding (id uuid PRIMARY KEY)`); err != nil {
			t.Fatalf("CREATE shadow table: %v", err)
		}

		// Step 2: dedicated pool whose search_path resolves the shadow table
		// first; the first two hops still resolve public.
		cfg, err := pgxpool.ParseConfig(dbDSN())
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
		shadowPool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("shadow pool: %v", err)
		}
		t.Cleanup(shadowPool.Close)
		f.rem.pool = shadowPool

		// Preflight self-check A: the injection really produces 42703.
		var probeErr error
		_, probeErr = shadowPool.Query(ctx, `SELECT b.id::text, b.channel_user_id, b.installation_id::text, ci.id::text, ci.workspace_id::text, ci.channel_type, ci.status FROM channel_user_binding b LEFT JOIN channel_installation ci ON ci.id = b.installation_id WHERE b.workspace_id = $1 AND b.multica_user_id = $2 AND b.channel_type = 'feishu' ORDER BY b.bound_at DESC, b.id ASC`, remWS, u1)
		if probeErr == nil {
			t.Fatal("preflight self-check: shadow query must fail")
		}
		var pgErr *pgconn.PgError
		if !errors.As(probeErr, &pgErr) || pgErr.Code != "42703" {
			t.Fatalf("preflight self-check: want 42703, got %v", probeErr)
		}
		// Preflight self-check B: public table untouched.
		var publicOK bool
		if err := f.pool.QueryRow(ctx, `SELECT to_regclass('public.channel_user_binding') IS NOT NULL`).Scan(&publicOK); err != nil || !publicOK {
			t.Fatalf("public.channel_user_binding must be intact (err=%v)", err)
		}
		// Preflight self-check C: the server stored the name verbatim.
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`, schema).Scan(&n); err != nil || n != 1 {
			t.Fatalf("pg_namespace row for %q = %d (err=%v), want 1", schema, n, err)
		}

		f.publish("CR-9001-031")
		waitFor(t, "binding-query failure", func() bool {
			return f.logs.count("step", stepBindingQuery) == 1
		})
		f.settled()
		if f.logs.count("result", "failed") != 1 {
			t.Errorf("failed = %d, want 1", f.logs.count("result", "failed"))
		}
		if f.logs.count("reason", reasonBindingMissing) != 0 {
			t.Error("a binding-query failure must not masquerade as binding-missing")
		}
		found := false
		for _, m := range f.logs.all() {
			if v, _ := m["step"].(string); v == stepBindingQuery {
				found = true
				if rid, _ := m["recipient_user_id"].(string); rid != u1 {
					t.Errorf("recipient-level failure must name the recipient, got %v", m)
				}
			}
		}
		if !found {
			t.Error("no binding-query failure line found")
		}
		// Only the third hop failed: no project-chain/approver-query failures.
		for _, step := range []string{stepProjectChain, stepApproverQuery} {
			if f.logs.count("step", step) != 0 {
				t.Errorf("step=%s must have zero failures", step)
			}
		}
	})
}

// TestApprovalReminderChooseEffective is the pure-function unit suite.
func TestApprovalReminderChooseEffective(t *testing.T) {
	str := func(s string) *string { return &s }
	base := func() approvalBindingCandidate {
		return approvalBindingCandidate{
			BindingID: "b1", OpenID: "ou1",
			InstallationID: str("i1"), InstID: str("i1"),
			InstWorkspaceID: str("ws1"), InstChannelType: str("feishu"), InstStatus: str("active"),
		}
	}
	t.Run("hit first valid in order", func(t *testing.T) {
		older := base()
		older.BindingID = "b-old"
		newer := base()
		newer.BindingID = "b-new"
		newer.OpenID = "ou-new"
		rows := []approvalBindingCandidate{newer, older}
		pick, reason := chooseEffective(rows, "ws1")
		if pick == nil || pick.BindingID != "b-new" || reason != "" {
			t.Errorf("pick=%v reason=%q", pick, reason)
		}
	})
	t.Run("reason priority mismatch over revoked over missing", func(t *testing.T) {
		revoked := base()
		revoked.InstStatus = str("revoked")
		missing := base()
		missing.InstallationID = nil
		missing.InstID = nil
		mismatch := base()
		mismatch.InstWorkspaceID = str("ws-other")
		for _, tc := range []struct {
			rows []approvalBindingCandidate
			want string
		}{
			{[]approvalBindingCandidate{revoked, missing}, reasonInstallationRevoked},
			{[]approvalBindingCandidate{missing}, reasonInstallationMissing},
			{[]approvalBindingCandidate{mismatch, revoked, missing}, reasonWorkspaceMismatch},
			{[]approvalBindingCandidate{base()}, ""},
		} {
			pick, reason := chooseEffective(tc.rows, "ws1")
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want == "" && pick == nil {
				t.Error("valid row must be picked")
			}
			if tc.want != "" && pick != nil {
				t.Error("no valid row must return nil pick")
			}
		}
	})
}
