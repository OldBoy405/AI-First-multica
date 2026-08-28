package handler

// AIFIRST: CR-2026-053 TASK-06 — CreatePipelineTask issue_id/project_id
// inheritance tests (FR-B12, AC-B10/AC-B11, SDD §4.4 path 1). Runs against
// the shared handler test database; skips when unreachable (TestMain).

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// pipelineTaskFixture seeds source agent+task (with full attribution), an
// executor agent on a runtime, and the CR row. Returns the IDs.
func pipelineTaskFixture(t *testing.T, withIssue bool) (sourceTaskID, executorAgentID, issueID, projectID, crID string) {
	t.Helper()
	projectID = dbfx.Project(t, "cr-pipe-project")
	issueID = dbfx.Issue(t, "cr-pipe-issue", testutil.Cols{"project_id": projectID})
	sourceAgentID := dbfx.Agent(t, "cr-pipe-source-agent", "")
	// cr_pipeline_task_test.go fix (review-code BLOCK-④): migration 251's
	// agent_task_queue_active_requires_runtime CHECK requires every queued row
	// to carry a runtime_id (or a terminal completed_at); the shared dbfx.Task
	// fixture does not set either, so the fixture explicitly stamps the handler
	// test runtime here instead of touching dbfx.Task.
	sourceCols := testutil.Cols{
		"runtime_id":             handlerTestRuntimeID(t),
		"originator_source":      "direct_human",
		"originator_user_id":     testUserID,
		"accountable_user_id":    testUserID,
		"delegated_from_task_id": nil,
	}
	if withIssue {
		sourceCols["issue_id"] = issueID
		sourceCols["project_id"] = projectID
	}
	sourceTaskID = dbfx.Task(t, sourceAgentID, sourceCols)
	executorAgentID = dbfx.Agent(t, "cr-pipe-executor-agent", handlerTestRuntimeID(t))
	crID = "CR-PIPE-TEST-01"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID)
	// agent_task_queue.pipeline_node_run_id is an FK to pipeline_node_run(id)
	// (migration 437), itself FK'd to pipeline_run(id): the positive test's
	// EnqueuePipelineTask insert needs both projection rows to exist, so the
	// fixture seeds them with the test's deterministic run/node/node-run ids.
	// (review-code BLOCK-④ fix: real-DB run surfaced this missing seed.)
	dbfx.Exec(t, `INSERT INTO pipeline_run (id, workspace_id, pipeline_id, cr_id, started_by, status)
		VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1'::uuid, $1::uuid, 'code-implementation', $2, $3::uuid, 'running')
		ON CONFLICT (id) DO NOTHING`, testWorkspaceID, crID, testUserID)
	dbfx.Exec(t, `INSERT INTO pipeline_node_run (id, run_id, node_id, kind, seq, status)
		VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3'::uuid, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1'::uuid, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2'::uuid, 'skill', 0, 'running')
		ON CONFLICT (id) DO NOTHING`)
	return sourceTaskID, executorAgentID, issueID, projectID, crID
}

func TestCreatePipelineTaskIssueInheritPositive(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sourceTaskID, executorAgentID, issueID, projectID, crID := pipelineTaskFixture(t, true)
	ctx := context.Background()

	task, err := testHandler.TaskService.EnqueuePipelineTask(ctx, service.PipelineTaskSpec{
		WorkspaceID:     util.MustParseUUID(testWorkspaceID),
		CrID:            crID,
		RunID:           util.MustParseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"),
		NodeID:          util.MustParseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"),
		NodeRunID:       util.MustParseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3"),
		PipelineID:      "code-implementation",
		Attempt:         1,
		Prompt:          "review-code test prompt",
		SourceTaskID:    util.MustParseUUID(sourceTaskID),
		ExecutorAgentID: util.MustParseUUID(executorAgentID),
		Priority:        0,
	})
	if err != nil {
		t.Fatalf("enqueue should succeed: %v", err)
	}
	// AC-B10: the new reviewer task row inherits issue_id/project_id bit-for-bit
	// from the source task row.
	var gotIssue, gotProject pgtype.UUID
	dbfx.QueryRow(t, `SELECT issue_id, project_id FROM agent_task_queue WHERE id = $1::uuid`, util.UUIDToString(task.ID)).Scan(&gotIssue, &gotProject)
	if !gotIssue.Valid || gotIssue.String() != issueID {
		t.Errorf("inherited issue_id = %v, want %s", gotIssue.String(), issueID)
	}
	if !gotProject.Valid || gotProject.String() != projectID {
		t.Errorf("inherited project_id = %v, want %s", gotProject.String(), projectID)
	}
}

func TestCreatePipelineTaskIssueInheritNegative(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sourceTaskID, executorAgentID, _, _, crID := pipelineTaskFixture(t, false)
	ctx := context.Background()
	before := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1::uuid`, executorAgentID)

	_, err := testHandler.TaskService.EnqueuePipelineTask(ctx, service.PipelineTaskSpec{
		WorkspaceID:     util.MustParseUUID(testWorkspaceID),
		CrID:            crID,
		RunID:           util.MustParseUUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb1"),
		NodeID:          util.MustParseUUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2"),
		NodeRunID:       util.MustParseUUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3"),
		PipelineID:      "code-implementation",
		Attempt:         1,
		Prompt:          "review-code test prompt",
		SourceTaskID:    util.MustParseUUID(sourceTaskID),
		ExecutorAgentID: util.MustParseUUID(executorAgentID),
		Priority:        0,
	})
	// AC-B11: an issue-less source task must be refused, not create a doomed
	// NULL-issue reviewer task (the guard maps pgx.ErrNoRows →
	// ErrRunnerAttributionInvalid via the GetActivePipelineTask recheck).
	if !errors.Is(err, service.ErrRunnerAttributionInvalid) {
		t.Fatalf("err = %v, want ErrRunnerAttributionInvalid", err)
	}
	after := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1::uuid`, executorAgentID)
	if after != before {
		t.Errorf("agent_task_queue rows = %d → %d, want no new row", before, after)
	}
}

func TestCreatePipelineTaskRejectsCrossWorkspaceSource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	foreignWorkspaceID := dbfx.Workspace(t, "cr-pipe-foreign", "cr-pipe-foreign")
	foreign := testutil.New(testPool, foreignWorkspaceID, testUserID)
	foreignRuntimeID := foreign.Runtime(t, "cr-pipe-foreign-runtime")
	foreignProjectID := foreign.Project(t, "cr-pipe-foreign-project")
	foreignIssueID := foreign.Issue(t, "cr-pipe-foreign-issue", testutil.Cols{"project_id": foreignProjectID})
	foreignAgentID := foreign.Agent(t, "cr-pipe-foreign-agent", foreignRuntimeID)
	foreignTaskID := foreign.Task(t, foreignAgentID, testutil.Cols{
		"runtime_id":          foreignRuntimeID,
		"originator_source":   "direct_human",
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
		"issue_id":            foreignIssueID,
		"project_id":          foreignProjectID,
	})

	executorAgentID := dbfx.Agent(t, "cr-pipe-cross-workspace-executor", handlerTestRuntimeID(t))
	crID := "CR-PIPE-CROSS-WORKSPACE"
	runID := "cccccccc-cccc-4ccc-8ccc-ccccccccccc1"
	nodeID := "cccccccc-cccc-4ccc-8ccc-ccccccccccc2"
	nodeRunID := "cccccccc-cccc-4ccc-8ccc-ccccccccccc3"
	dbfx.Exec(t, `INSERT INTO cr (workspace_id, cr_id, status) VALUES ($1::uuid, $2, 'developing')
		ON CONFLICT (workspace_id, cr_id) DO NOTHING`, testWorkspaceID, crID)
	dbfx.Exec(t, `INSERT INTO pipeline_run (id, workspace_id, pipeline_id, cr_id, started_by, status)
		VALUES ($1::uuid, $2::uuid, 'code-implementation', $3, $4::uuid, 'running')
		ON CONFLICT (id) DO NOTHING`, runID, testWorkspaceID, crID, testUserID)
	dbfx.Exec(t, `INSERT INTO pipeline_node_run (id, run_id, node_id, kind, seq, status)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'skill', 0, 'running')
		ON CONFLICT (id) DO NOTHING`, nodeRunID, runID, nodeID)
	dbfx.Cleanup(t, `DELETE FROM cr WHERE workspace_id = $1::uuid AND cr_id = $2`, testWorkspaceID, crID)
	dbfx.Cleanup(t, `DELETE FROM pipeline_run WHERE id = $1::uuid`, runID)
	dbfx.Cleanup(t, `DELETE FROM pipeline_node_run WHERE id = $1::uuid`, nodeRunID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE pipeline_node_run_id = $1::uuid`, nodeRunID)

	_, err := testHandler.TaskService.EnqueuePipelineTask(ctx, service.PipelineTaskSpec{
		WorkspaceID:     util.MustParseUUID(testWorkspaceID),
		CrID:            crID,
		RunID:           util.MustParseUUID(runID),
		NodeID:          util.MustParseUUID(nodeID),
		NodeRunID:       util.MustParseUUID(nodeRunID),
		PipelineID:      "code-implementation",
		Attempt:         1,
		Prompt:          "cross-workspace source must be rejected",
		SourceTaskID:    util.MustParseUUID(foreignTaskID),
		ExecutorAgentID: util.MustParseUUID(executorAgentID),
	})
	if !errors.Is(err, service.ErrRunnerAttributionInvalid) {
		t.Fatalf("err = %v, want ErrRunnerAttributionInvalid", err)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE pipeline_node_run_id = $1::uuid`, nodeRunID); got != 0 {
		t.Fatalf("cross-workspace pipeline tasks = %d, want 0", got)
	}
}
