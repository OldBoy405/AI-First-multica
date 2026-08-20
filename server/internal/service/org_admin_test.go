package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestMaturityReportBuiltinSkillContract(t *testing.T) {
	var content string
	for _, skill := range loadBuiltinSkills() {
		if skill.Name == "multica-maturity-weekly-report" {
			content = skill.Content
			break
		}
	}
	if content == "" {
		t.Fatal("maturity report built-in skill not embedded")
	}
	for _, required := range []string{
		"atomic temp-file + rename", "baseline_suggestions", "ai-first.maturity-report/v1",
		"source_task_id", "chat_session_id", "## Individual efficiency",
		"## Team delivery", "## Knowledge compounding", "## Risk & yield", "## Cost",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("maturity report skill missing %q", required)
		}
	}
}

func TestPersistMaturityReportCompletionIgnoresFollowUpTurns(t *testing.T) {
	result := []byte(`{"output":"Why did EPC fall?"}`)
	got, err := persistMaturityReportCompletion(context.Background(), nil, db.AgentTaskQueue{}, result)
	if err != nil || string(got) != string(result) {
		t.Fatalf("follow-up completion = %q, %v", got, err)
	}
}

func TestBuildReportEnvelope(t *testing.T) {
	wsID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	taskID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	sessionID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	body := []byte("# Weekly\n\nFive sections.\n")

	env, err := BuildReportEnvelope(wsID, "2026-W34", body, taskID, sessionID, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ReportKey != uuid.UUID(wsID.Bytes).String()+":2026-W34" {
		t.Fatalf("report_key = %q", env.ReportKey)
	}
	if env.RelativePath != "docs/org-admin/maturity-review-2026-W34.md" {
		t.Fatalf("relative_path = %q", env.RelativePath)
	}
	if !VerifyReportSHA(body, env.ContentSha256) {
		t.Fatal("envelope SHA must verify")
	}
	// Tampered body must fail verification.
	if VerifyReportSHA([]byte("# altered"), env.ContentSha256) {
		t.Fatal("tampered body must not verify")
	}
	// Bad week / empty body rejected.
	if _, err := BuildReportEnvelope(wsID, "2026-34", body, taskID, sessionID, nil); err == nil {
		t.Fatal("bad week must be rejected")
	}
	if _, err := BuildReportEnvelope(wsID, "2026-W34", nil, taskID, sessionID, nil); err == nil {
		t.Fatal("empty markdown must be rejected")
	}
}

func TestEnsureOrgAdminWorkspaceIdempotent(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)

	wsID := uuid.New()
	ownerID := uuid.New()
	runtimeID := uuid.New()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug) VALUES ($1,'oa-fixture',$2)`, wsID, "oa-fixture-"+wsID.String()[:8])
	exec(`INSERT INTO "user" (id, name, email) VALUES ($1,'oa-owner',$2)`, ownerID, "oa-"+ownerID.String()+"@example.test")
	exec(`INSERT INTO member (id, workspace_id, user_id, role) VALUES ($1,$2,$3,'admin')`, uuid.New(), wsID, ownerID)
	exec(`INSERT INTO agent_runtime (id, workspace_id, name, runtime_mode, provider, status) VALUES ($1,$2,'oa-runtime','local','openai','online')`, runtimeID, wsID)

	queries := db.New(pool)
	wsp := pgtype.UUID{Bytes: wsID, Valid: true}
	ownp := pgtype.UUID{Bytes: ownerID, Valid: true}
	runp := pgtype.UUID{Bytes: runtimeID, Valid: true}

	first, err := EnsureOrgAdminWorkspace(ctx, queries, pool, wsp, ownp, runp)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	second, err := EnsureOrgAdminWorkspace(ctx, queries, pool, wsp, ownp, runp)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if first.ProjectID.Bytes != second.ProjectID.Bytes ||
		first.AgentID.Bytes != second.AgentID.Bytes ||
		first.AutopilotID.Bytes != second.AutopilotID.Bytes ||
		first.TriggerID.Bytes != second.TriggerID.Bytes {
		t.Fatalf("bootstrap not idempotent: %+v vs %+v", first, second)
	}

	// Row-count invariants: one system-key project, one system-key agent,
	// one autopilot + one schedule trigger.
	var projectCount, agentCount, autopilotCount, triggerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project WHERE workspace_id=$1 AND settings->>'system_key'='org-admin-workspace'`, wsID).Scan(&projectCount); err != nil || projectCount != 1 {
		t.Fatalf("projects = %d, %v", projectCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND system_key='org-admin'`, wsID).Scan(&agentCount); err != nil || agentCount != 1 {
		t.Fatalf("agents = %d, %v", agentCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM autopilot WHERE workspace_id=$1 AND project_id=$2`, wsID, first.ProjectID.Bytes).Scan(&autopilotCount); err != nil || autopilotCount != 1 {
		t.Fatalf("autopilots = %d, %v", autopilotCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM autopilot_trigger WHERE autopilot_id=$1 AND kind='schedule'`, first.AutopilotID.Bytes).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("triggers = %d, %v", triggerCount, err)
	}
	var ruleCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM autopilot_rule_version WHERE autopilot_id=$1`, first.AutopilotID.Bytes).Scan(&ruleCount); err != nil || ruleCount != 1 {
		t.Fatalf("rule versions = %d, %v", ruleCount, err)
	}
	var leadID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT lead_id FROM project WHERE id=$1`, first.ProjectID.Bytes).Scan(&leadID); err != nil || leadID != uuid.UUID(first.AgentID.Bytes) {
		t.Fatalf("project lead = %s, %v; want Org Admin agent", leadID, err)
	}

	// Dispatch seam: the weekly schedule creates a project-bound task on one
	// reusable Team Agent chat session. Completion then unwraps the daemon
	// payload into the direct envelope indexed by 379 and notifies the Owner.
	taskSvc := &TaskService{Queries: queries, TxStarter: pool, Bus: events.New()}
	autopilotSvc := NewAutopilotService(queries, pool, events.New(), taskSvc)
	autopilot, err := queries.GetAutopilot(ctx, first.AutopilotID)
	if err != nil {
		t.Fatalf("load report autopilot: %v", err)
	}
	run, err := autopilotSvc.DispatchAutopilot(ctx, autopilot, first.TriggerID, "schedule", nil)
	if err != nil {
		t.Fatalf("dispatch report autopilot: %v", err)
	}
	task, err := queries.GetAgentTask(ctx, run.TaskID)
	if err != nil {
		t.Fatalf("load report task: %v", err)
	}
	if !task.ProjectID.Valid || task.ProjectID.Bytes != first.ProjectID.Bytes || !task.ChatSessionID.Valid {
		t.Fatalf("report task missing project/chat binding: %+v", task)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatalf("start report task: %v", err)
	}
	if _, err := taskSvc.CompleteTask(ctx, task.ID, []byte(`{"output":"not a report"}`), "", "", "", false, ""); err == nil {
		t.Fatal("malformed scheduled report completion must fail closed")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, task.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("malformed completion changed task status to %q: %v", status, err)
	}
	markdown := []byte("## Individual efficiency\n`token_intensity`\n\n## Team delivery\n`cr_throughput_per_capita`\n\n## Knowledge compounding\n`process_completion_rate`\n\n## Risk & yield\n`gate_first_pass_rate`\n\n## Cost\n`cost_usd`\n")
	envelope, err := BuildReportEnvelope(wsp, "2026-W34", markdown,
		task.ID, task.ChatSessionID, []string{"config-rev"})
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	output, _ := json.Marshal(envelope)
	wrapped, _ := json.Marshal(protocol.TaskCompletedPayload{TaskID: uuid.UUID(task.ID.Bytes).String(), Output: string(output)})
	completed, err := taskSvc.CompleteTask(ctx, task.ID, wrapped, "", "", "", false, "")
	if err != nil {
		t.Fatalf("complete report task: %v", err)
	}
	if report, ok := decodeReport(completed.Result); !ok || report.ReportKey != envelope.ReportKey {
		t.Fatalf("stored direct envelope = %s", completed.Result)
	}
	var inboxCount, messageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND recipient_id=$2 AND type='maturity_report_ready'`, wsID, ownerID).Scan(&inboxCount); err != nil || inboxCount != 1 {
		t.Fatalf("report inbox = %d, %v", inboxCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1 AND role='assistant'`, task.ChatSessionID.Bytes).Scan(&messageCount); err != nil || messageCount != 1 {
		t.Fatalf("report chat messages = %d, %v", messageCount, err)
	}
	if _, err := taskSvc.CompleteTask(ctx, task.ID, wrapped, "", "", "", false, ""); err != nil {
		t.Fatalf("idempotent report completion: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND recipient_id=$2 AND type='maturity_report_ready'`, wsID, ownerID).Scan(&inboxCount); err != nil || inboxCount != 1 {
		t.Fatalf("report inbox after retry = %d, %v", inboxCount, err)
	}

	// A distinct retry task for the same ISO week must reuse the chat and must
	// not fan out a duplicate Owner inbox notification.
	secondRun, err := autopilotSvc.DispatchAutopilot(ctx, autopilot, first.TriggerID, "schedule", nil)
	if err != nil {
		t.Fatalf("dispatch second report: %v", err)
	}
	secondTask, err := queries.GetAgentTask(ctx, secondRun.TaskID)
	if err != nil {
		t.Fatalf("load second report task: %v", err)
	}
	if secondTask.ChatSessionID.Bytes != task.ChatSessionID.Bytes {
		t.Fatal("same Org Admin project/Owner must reuse the report chat")
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, secondTask.ID); err != nil {
		t.Fatalf("start second report task: %v", err)
	}
	secondEnvelope, err := BuildReportEnvelope(wsp, "2026-W34", markdown,
		secondTask.ID, secondTask.ChatSessionID, []string{"config-rev"})
	if err != nil {
		t.Fatalf("build second report: %v", err)
	}
	secondOutput, _ := json.Marshal(secondEnvelope)
	if _, err := taskSvc.CompleteTask(ctx, secondTask.ID, secondOutput, "", "", "", false, ""); err != nil {
		t.Fatalf("complete second report: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND recipient_id=$2 AND type='maturity_report_ready'`, wsID, ownerID).Scan(&inboxCount); err != nil || inboxCount != 1 {
		t.Fatalf("same-week report inboxes = %d, %v", inboxCount, err)
	}
}
