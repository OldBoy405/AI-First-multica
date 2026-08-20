package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	runnerTestAgent   = "90000000-0000-0000-0000-000000000045"
	runnerTestRuntime = "90000000-0000-0000-0000-000000000046"
	runnerTestSource  = "90000000-0000-0000-0000-000000000047"
)

type insertingPipelineEnqueuer struct {
	t        *testing.T
	count    int
	lastSpec service.PipelineTaskSpec
}

func (f *insertingPipelineEnqueuer) EnqueuePipelineTask(ctx context.Context, spec service.PipelineTaskSpec) (db.AgentTaskQueue, error) {
	f.count++
	f.lastSpec = spec
	body, _ := json.Marshal(map[string]any{
		"type": "pipeline_node", "schema": "ai-first.pipeline-task/v1",
		"workspace_id": spec.WorkspaceID, "pipeline_id": spec.PipelineID,
		"cr_id": spec.CrID, "run_id": spec.RunID, "node_id": spec.NodeRunID,
		"attempt": spec.Attempt, "prompt": spec.Prompt,
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue(agent_id,runtime_id,status,priority,context,cr_id,pipeline_node_run_id)
		VALUES($1::uuid,$2::uuid,'queued',0,$3::jsonb,$4,$5::uuid)`,
		runnerTestAgent, runnerTestRuntime, body, spec.CrID, spec.NodeRunID); err != nil {
		return db.AgentTaskQueue{}, err
	}
	return db.AgentTaskQueue{}, nil
}

func setupRunnerIntegration(t *testing.T, crID string) (*Runner, *insertingPipelineEnqueuer, string) {
	t.Helper()
	if testPool == nil {
		t.Skip("no database connection")
	}
	resetGateProjection(t, crID)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE agent_id=$1::uuid`, runnerTestAgent); err != nil {
		t.Fatalf("clean Runner tasks: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO agent_runtime(id,workspace_id,name,runtime_mode,provider)
		VALUES($1::uuid,$2::uuid,'runner-test-runtime','local','multica_daemon') ON CONFLICT(id) DO NOTHING`, runnerTestRuntime, testWorkspaceID); err != nil {
		t.Fatalf("seed Runner runtime: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id)
		VALUES($1::uuid,$2::uuid,'runner-test-agent','local',$3::uuid) ON CONFLICT(id) DO NOTHING`, runnerTestAgent, testWorkspaceID, runnerTestRuntime); err != nil {
		t.Fatalf("seed Runner agent: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO cr(workspace_id,cr_id,title,status) VALUES($1::uuid,$2,'Runner test','tech-designing')`, testWorkspaceID, crID); err != nil {
		t.Fatalf("seed Runner CR: %v", err)
	}
	registry, err := parseCoreRegistry()
	if err != nil {
		t.Fatal(err)
	}
	inputs, _ := json.Marshal(map[string]any{"cr_id": crID, "tech_context": "test"})
	execCtx, _ := json.Marshal(map[string]any{
		"runner": runnerSchema, "template_digest": registry.Digest,
		"pipeline_owner": registry.PipelineOwner, "executor_agent_id": runnerTestAgent, "source_task_id": runnerTestSource,
	})
	var runID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO pipeline_run(workspace_id,pipeline_id,cr_id,status,inputs,execution_context,started_by)
		VALUES($1::uuid,'architecture-design',$2,'running',$3::jsonb,$4::jsonb,$5::uuid)
		RETURNING id::text`, testWorkspaceID, crID, inputs, execCtx, testUserID(t)).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	fake := &insertingPipelineEnqueuer{t: t}
	runner := &Runner{pool: testPool, tasks: fake, registry: registry}
	t.Cleanup(func() {
		resetGateProjection(t, crID)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE agent_id=$1::uuid`, runnerTestAgent)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent WHERE id=$1::uuid`, runnerTestAgent)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id=$1::uuid`, runnerTestRuntime)
	})
	return runner, fake, runID
}

func completeLatestRunnerTask(t *testing.T, runID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_task_queue SET status='completed',completed_at=now()
		WHERE id=(SELECT t.id FROM agent_task_queue t JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id
		          WHERE n.run_id=$1::uuid AND t.status<>'completed' ORDER BY n.attempt DESC,n.seq DESC,t.created_at DESC LIMIT 1)`, runID); err != nil {
		t.Fatal(err)
	}
}

func setCRStatus(t *testing.T, crID, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE cr SET status=$2 WHERE cr_id=$1`, crID, status); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDigestMismatchIsRecoverableOnSameRun(t *testing.T) {
	const crID = "CR-9045-008"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_run SET execution_context=jsonb_set(execution_context,'{template_digest}',to_jsonb('sha256:drift'::text)) WHERE id=$1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID)
	if err == nil || !strings.Contains(err.Error(), RunnerErrTemplateDigestMismatch) {
		t.Fatalf("digest drift must fail closed, got %v", err)
	}
	var status string
	var completedAt *time.Time
	var tasks int
	if err := testPool.QueryRow(ctx, `SELECT status,completed_at FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&status, &completedAt); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue t JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id WHERE n.run_id=$1::uuid`, runID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if status != "running" || completedAt != nil || tasks != 0 || fake.count != 0 {
		t.Fatalf("digest drift mutated run: status=%s completed=%v tasks=%d fake=%d", status, completedAt, tasks, fake.count)
	}
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_run SET execution_context=jsonb_set(execution_context,'{template_digest}',to_jsonb($2::text)) WHERE id=$1::uuid`, runID, runner.registry.Digest); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 1 {
		t.Fatalf("restored digest did not resume same run: tasks=%d err=%v", fake.count, err)
	}
	var resumedRunID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM pipeline_run WHERE workspace_id=$1::uuid AND cr_id=$2 AND status='running'`, testWorkspaceID, crID).Scan(&resumedRunID); err != nil || resumedRunID != runID {
		t.Fatalf("digest restore changed run: got=%q want=%q err=%v", resumedRunID, runID, err)
	}
}

func TestRunnerArchitectureHappyPathWaitsForAuthority(t *testing.T) {
	const crID = "CR-9045-001"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()

	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 1 || fake.lastSpec.NodeID.String() != runner.registry.Pipeline.Nodes[0].ID {
		t.Fatalf("write dispatch: count=%d spec=%+v err=%v", fake.count, fake.lastSpec, err)
	}
	completeLatestRunnerTask(t, runID)
	// Task success alone is insufficient: CR is still designing, so review may not start.
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 1 {
		t.Fatalf("write authority wait: count=%d err=%v", fake.count, err)
	}
	setCRStatus(t, crID, "tech-design-review-pending")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 2 || fake.lastSpec.NodeID.String() != runner.registry.Pipeline.Nodes[1].ID {
		t.Fatalf("review dispatch: count=%d spec=%+v err=%v", fake.count, fake.lastSpec, err)
	}
	completeLatestRunnerTask(t, runID)
	review := runner.registry.Pipeline.Nodes[1]
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='passed',detail=$3::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=1`, runID, review.ID, `{"verdict":"pass","attempt":1,"blockers":[],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`); err != nil {
		t.Fatal(err)
	}
	setCRStatus(t, crID, "tech-design-review-pending")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 2 {
		t.Fatalf("human gate must not enqueue an Agent task: count=%d err=%v", fake.count, err)
	}
	var runStatus string
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil || runStatus != "waiting_approval" {
		t.Fatalf("expected waiting_approval, got %q err=%v", runStatus, err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO approval_record(workspace_id,cr_id,stage,decision,approver_user_id,evidence_digest,key_id,signature,delivered_at)
		VALUES($1::uuid,$2,'tech-design','approve',$3::uuid,'digest','test','sig',now())`, testWorkspaceID, crID, testUserID(t)); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 3 || fake.lastSpec.NodeID.String() != runner.registry.Pipeline.Nodes[3].ID {
		t.Fatalf("approve dispatch: count=%d spec=%+v err=%v", fake.count, fake.lastSpec, err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-reviewed")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 4 || fake.lastSpec.NodeID.String() != runner.registry.Pipeline.Nodes[4].ID {
		t.Fatalf("checkpoint dispatch: count=%d spec=%+v err=%v", fake.count, fake.lastSpec, err)
	}
	completeLatestRunnerTask(t, runID)
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil || runStatus != "running" {
		t.Fatalf("task success without checkpoint must not complete: %q err=%v", runStatus, err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO cr_sync_event(workspace_id,cr_id,commit_sha,event_kind,payload,occurred_at) VALUES($1::uuid,$2,'checkpoint-sha','checkpoint','{}',now()+interval '1 second')`, testWorkspaceID, crID); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil || runStatus != "completed" {
		t.Fatalf("expected completed after canonical checkpoint: %q err=%v", runStatus, err)
	}
}

func TestRunnerCheckpointFailureRetriesOnlyCheckpoint(t *testing.T) {
	const crID = "CR-9045-009"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()

	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-review-pending")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	review := runner.registry.Pipeline.Nodes[1]
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='passed',detail=$3::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=1`, runID, review.ID, `{"verdict":"pass","attempt":1,"blockers":[],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO approval_record(workspace_id,cr_id,stage,decision,approver_user_id,evidence_digest,key_id,signature,delivered_at) VALUES($1::uuid,$2,'tech-design','approve',$3::uuid,'digest','test','sig',now())`, testWorkspaceID, crID, testUserID(t)); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-reviewed")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 4 {
		t.Fatalf("checkpoint dispatch failed: tasks=%d err=%v", fake.count, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status='failed',completed_at=now() WHERE id=(SELECT t.id FROM agent_task_queue t JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id WHERE n.run_id=$1::uuid AND n.seq=5 ORDER BY t.created_at DESC LIMIT 1)`, runID); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 5 {
		t.Fatalf("checkpoint failure did not create one retry: tasks=%d err=%v", fake.count, err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 5 {
		t.Fatalf("duplicate wake duplicated checkpoint retry: tasks=%d err=%v", fake.count, err)
	}
	var runStatus, crStatus string
	var priorTasks, checkpointTasks, activeCheckpoint int
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT status FROM cr WHERE workspace_id=$1::uuid AND cr_id=$2`, testWorkspaceID, crID).Scan(&crStatus); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE n.seq<5),count(*) FILTER (WHERE n.seq=5),count(*) FILTER (WHERE n.seq=5 AND t.status IN ('queued','deferred','dispatched','waiting_local_directory','running')) FROM agent_task_queue t JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id WHERE n.run_id=$1::uuid`, runID).Scan(&priorTasks, &checkpointTasks, &activeCheckpoint); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || crStatus != "tech-design-reviewed" || priorTasks != 3 || checkpointTasks != 2 || activeCheckpoint != 1 {
		t.Fatalf("checkpoint retry changed prior state: run=%s cr=%s prior=%d checkpoint=%d active=%d", runStatus, crStatus, priorTasks, checkpointTasks, activeCheckpoint)
	}
	completeLatestRunnerTask(t, runID)
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil || runStatus != "running" {
		t.Fatalf("retry task success bypassed checkpoint authority: status=%s err=%v", runStatus, err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO cr_sync_event(workspace_id,cr_id,commit_sha,event_kind,payload,occurred_at) VALUES($1::uuid,$2,'checkpoint-retry-sha','checkpoint','{}',now()+interval '1 second')`, testWorkspaceID, crID); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&runStatus); err != nil || runStatus != "completed" {
		t.Fatalf("checkpoint retry did not complete after authority event: status=%s err=%v", runStatus, err)
	}
}

func seedRunnerStartPrerequisites(t *testing.T, runner *Runner) *service.TaskService {
	t.Helper()
	ctx := context.Background()
	userID := testUserID(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue(id,agent_id,runtime_id,status,priority,originator_source,originator_user_id,accountable_user_id,trigger_evidence_kind,trigger_evidence_ref_id)
		VALUES($1::uuid,$2::uuid,$3::uuid,'completed',0,'task_token',$4::uuid,$4::uuid,'task',$1::uuid)`,
		runnerTestSource, runnerTestAgent, runnerTestRuntime, userID); err != nil {
		t.Fatalf("seed source task: %v", err)
	}
	for _, node := range runner.registry.Pipeline.Nodes {
		if node.Kind != "skill" {
			continue
		}
		var skillID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO skill(workspace_id,name,description,content,config,created_by)
			VALUES($1::uuid,$2,'runner test','test','{}',$3::uuid)
			ON CONFLICT(workspace_id,name) DO UPDATE SET updated_at=now()
			RETURNING id::text`, testWorkspaceID, node.Ref, userID).Scan(&skillID); err != nil {
			t.Fatalf("seed skill %s: %v", node.Ref, err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO agent_skill(agent_id,skill_id,enabled) VALUES($1::uuid,$2::uuid,true) ON CONFLICT(agent_id,skill_id) DO UPDATE SET enabled=true`, runnerTestAgent, skillID); err != nil {
			t.Fatalf("assign skill %s: %v", node.Ref, err)
		}
	}
	return service.NewTaskService(db.New(testPool), testPool, nil, events.New())
}

func TestRunnerStartupAndEventWakeSourcesConvergeOnReconcile(t *testing.T) {
	const crID = "CR-9045-007"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	bus := events.New()
	runner.WireEvents(bus)
	if err := runner.StartupScan(ctx); err != nil || fake.count != 1 {
		t.Fatalf("startup scan did not dispatch first node: count=%d err=%v", fake.count, err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-review-pending")
	var taskID string
	if err := testPool.QueryRow(ctx, `SELECT t.id::text FROM agent_task_queue t JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id WHERE n.run_id=$1::uuid ORDER BY n.seq LIMIT 1`, runID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	bus.Publish(events.Event{Type: protocol.EventTaskCompleted, TaskID: taskID})
	if fake.count != 2 {
		t.Fatalf("task terminal wake did not dispatch review: count=%d", fake.count)
	}
	completeLatestRunnerTask(t, runID)
	review := runner.registry.Pipeline.Nodes[1]
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='passed',detail=$3::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=1`, runID, review.ID, `{"verdict":"pass","attempt":1,"blockers":[],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`); err != nil {
		t.Fatal(err)
	}
	bus.Publish(events.Event{Type: EventCRUpdated, WorkspaceID: testWorkspaceID, Payload: map[string]any{"cr_id": crID}})
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&status); err != nil || status != "waiting_approval" {
		t.Fatalf("CR projection wake did not reach human wait: status=%q err=%v", status, err)
	}
}

func TestStartArchitectureAdoptsProjectorRunAndDeduplicatesConcurrentStart(t *testing.T) {
	const crID = "CR-9045-004"
	runner, _, projectorRunID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	runner.tasks = seedRunnerStartPrerequisites(t, runner)
	setCRStatus(t, crID, "requirement-approved")
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_run SET execution_context='{}'::jsonb WHERE id=$1::uuid`, projectorRunID); err != nil {
		t.Fatal(err)
	}
	input := StartArchitectureInput{
		WorkspaceID: runnerID(testWorkspaceID), AgentID: runnerID(runnerTestAgent),
		TaskID: runnerID(runnerTestSource), UserID: runnerID(testUserID(t)),
		CrID: crID, TechContext: "bounded",
	}
	adopted, changed, err := runner.StartArchitecture(ctx, input)
	if err != nil || adopted.String() != projectorRunID || !changed {
		t.Fatalf("projector run adoption failed: run=%s changed=%v err=%v", adopted.String(), changed, err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id<>$1::uuid AND agent_id=$2::uuid`, runnerTestSource, runnerTestAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM pipeline_run WHERE id=$1::uuid`, projectorRunID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		id      pgtype.UUID
		changed bool
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			id, changed, err := runner.StartArchitecture(context.Background(), input)
			results <- result{id: id, changed: changed, err: err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.id != second.id || first.changed == second.changed {
		t.Fatalf("concurrent start mismatch: first=%+v second=%+v", first, second)
	}
	var runs, tasks int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM pipeline_run WHERE workspace_id=$1::uuid AND cr_id=$2 AND pipeline_id='architecture-design' AND status IN ('running','waiting_approval')`, testWorkspaceID, crID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE cr_id=$1 AND status IN ('queued','deferred','dispatched','waiting_local_directory','running')`, crID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || tasks != 1 {
		t.Fatalf("partial unique invariants failed: active runs=%d active tasks=%d", runs, tasks)
	}
}

func TestEnqueuePipelineTaskCopiesAttributionAndDeduplicates(t *testing.T) {
	const crID = "CR-9045-003"
	runner, _, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	userID := testUserID(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue(id,agent_id,runtime_id,status,priority,originator_source,originator_user_id,accountable_user_id,trigger_evidence_kind,trigger_evidence_ref_id)
		VALUES($1::uuid,$2::uuid,$3::uuid,'completed',0,'task_token',$4::uuid,$4::uuid,'task',$1::uuid)`,
		runnerTestSource, runnerTestAgent, runnerTestRuntime, userID); err != nil {
		t.Fatalf("seed source task: %v", err)
	}
	node := runner.registry.Pipeline.Nodes[0]
	nodeRunID, err := runner.ensureNodeRow(ctx, runnerID(runID), node, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewTaskService(db.New(testPool), testPool, nil, events.New())
	spec := service.PipelineTaskSpec{
		WorkspaceID: runnerID(testWorkspaceID), CrID: crID, RunID: runnerID(runID),
		NodeID: runnerID(node.ID), NodeRunID: nodeRunID, PipelineID: "architecture-design",
		Attempt: 1, Prompt: "fixed", SourceTaskID: runnerID(runnerTestSource),
		ExecutorAgentID: runnerID(runnerTestAgent),
	}
	type enqueueResult struct {
		task db.AgentTaskQueue
		err  error
	}
	results := make(chan enqueueResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			task, err := svc.EnqueuePipelineTask(context.Background(), spec)
			results <- enqueueResult{task: task, err: err}
		}()
	}
	firstResult, secondResult := <-results, <-results
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("concurrent enqueue failed: first=%v second=%v", firstResult.err, secondResult.err)
	}
	first, second := firstResult.task, secondResult.task
	if first.ID != second.ID {
		t.Fatalf("idempotent loser must reread winner: first=%v second=%v", first.ID, second.ID)
	}
	var count int
	var originatorSource, copiedUser, accountable, evidenceKind, evidenceRef string
	if err := testPool.QueryRow(ctx, `
		SELECT count(*),min(originator_source),min(originator_user_id::text),min(accountable_user_id::text),min(trigger_evidence_kind),min(trigger_evidence_ref_id::text)
		FROM agent_task_queue WHERE pipeline_node_run_id=$1`, nodeRunID).Scan(&count, &originatorSource, &copiedUser, &accountable, &evidenceKind, &evidenceRef); err != nil {
		t.Fatal(err)
	}
	if count != 1 || originatorSource != "task_token" || copiedUser != userID || accountable != userID || evidenceKind != "task" || evidenceRef != runnerTestSource {
		t.Fatalf("attribution copy mismatch: n=%d source=%s user=%s accountable=%s evidence=%s/%s", count, originatorSource, copiedUser, accountable, evidenceKind, evidenceRef)
	}
	if got := svc.ResolveTaskWorkspaceID(ctx, first); got != testWorkspaceID {
		t.Fatalf("pipeline task workspace resolution failed: %q", got)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := svc.EnqueuePipelineTask(ctx, spec)
	if err != nil || retry.ID == first.ID {
		t.Fatalf("terminal parent must permit one retry task: retry=%v err=%v", retry.ID, err)
	}
	var total, active int
	if err := testPool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status IN ('queued','deferred','dispatched','waiting_local_directory','running')) FROM agent_task_queue WHERE pipeline_node_run_id=$1`, nodeRunID).Scan(&total, &active); err != nil || total != 2 || active != 1 {
		t.Fatalf("retry cardinality total=%d active=%d err=%v", total, active, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, retry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET originator_source=NULL WHERE id=$1::uuid`, runnerTestSource); err != nil {
		t.Fatal(err)
	}
	invalidNodeRunID, err := runner.ensureNodeRow(ctx, runnerID(runID), node, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	spec.NodeRunID, spec.Attempt = invalidNodeRunID, 2
	if _, err := svc.EnqueuePipelineTask(ctx, spec); !errors.Is(err, service.ErrRunnerAttributionInvalid) {
		t.Fatalf("unattributed source must fail closed, got %v", err)
	}
}

func runnerID(value string) (id pgtype.UUID) {
	if err := id.Scan(value); err != nil {
		panic(err)
	}
	return id
}

func TestRunnerLoopExhaustionFailsAfterThirdCanonicalBlock(t *testing.T) {
	const crID = "CR-9045-005"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		completeLatestRunnerTask(t, runID)
		setCRStatus(t, crID, "tech-design-review-pending")
		if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
			t.Fatalf("dispatch review attempt %d: %v", attempt, err)
		}
		completeLatestRunnerTask(t, runID)
		review := runner.registry.Pipeline.Nodes[1]
		detail := fmt.Sprintf(`{"verdict":"block","attempt":%d,"blockers":["B%02d"],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`, attempt, attempt)
		if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='blocked',detail=$4::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=$3`, runID, review.ID, attempt, detail); err != nil {
			t.Fatal(err)
		}
		setCRStatus(t, crID, "tech-designing")
		err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID)
		if attempt < 3 && err != nil {
			t.Fatalf("repair attempt %d: %v", attempt+1, err)
		}
		if attempt == 3 && (err == nil || !strings.Contains(err.Error(), RunnerErrLoopExhausted)) {
			t.Fatalf("third block must exhaust loop, got %v", err)
		}
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&status); err != nil || status != "failed" || fake.count != 6 {
		t.Fatalf("loop terminal mismatch: status=%q tasks=%d err=%v", status, fake.count, err)
	}
}

func TestRunnerSignedRejectTerminatesAfterCanonicalRollback(t *testing.T) {
	const crID = "CR-9045-006"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-review-pending")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	review := runner.registry.Pipeline.Nodes[1]
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='passed',detail=$3::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=1`, runID, review.ID, `{"verdict":"pass","attempt":1,"blockers":[],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO approval_record(workspace_id,cr_id,stage,decision,approver_user_id,evidence_digest,key_id,signature,reject_reason,delivered_at)
		VALUES($1::uuid,$2,'tech-design','reject',$3::uuid,'digest','test','sig','revise',now())`, testWorkspaceID, crID, testUserID(t)); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 3 {
		t.Fatalf("reject application dispatch failed: tasks=%d err=%v", fake.count, err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-designing")
	err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID)
	if err == nil || !strings.Contains(err.Error(), RunnerErrApprovalRejected) {
		t.Fatalf("signed reject must terminate run, got %v", err)
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM pipeline_run WHERE id=$1::uuid`, runID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("reject run status=%q err=%v", status, err)
	}
}

func TestRunnerReviewBlockReplaysWriteAtNextAttempt(t *testing.T) {
	const crID = "CR-9045-002"
	runner, fake, runID := setupRunnerIntegration(t, crID)
	ctx := context.Background()
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	setCRStatus(t, crID, "tech-design-review-pending")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	completeLatestRunnerTask(t, runID)
	review := runner.registry.Pipeline.Nodes[1]
	if _, err := testPool.Exec(ctx, `UPDATE pipeline_node_run SET status='blocked',detail=$3::jsonb WHERE run_id=$1::uuid AND node_id=$2::uuid AND attempt=1`, runID, review.ID, `{"verdict":"block","attempt":1,"blockers":["B01"],"reviewed_at":"2026-08-02T10:00:00Z","subject_sha256":"abc"}`); err != nil {
		t.Fatal(err)
	}
	setCRStatus(t, crID, "tech-designing")
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil {
		t.Fatal(err)
	}
	if fake.count != 3 || fake.lastSpec.NodeID.String() != runner.registry.Pipeline.Nodes[0].ID || fake.lastSpec.Attempt != 2 {
		t.Fatalf("expected repair write attempt 2, count=%d spec=%+v", fake.count, fake.lastSpec)
	}
	if err := runner.Reconcile(ctx, runnerID(testWorkspaceID), crID); err != nil || fake.count != 3 {
		t.Fatalf("later wake must remain on repair attempt without duplicate enqueue: count=%d err=%v", fake.count, err)
	}
	var createdAt time.Time
	if err := testPool.QueryRow(ctx, `SELECT created_at FROM agent_task_queue WHERE pipeline_node_run_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, fake.lastSpec.NodeRunID).Scan(&createdAt); err != nil || createdAt.IsZero() {
		t.Fatalf("repair task missing: %v", err)
	}
}
