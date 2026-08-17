// AIFIRST: Runner Core (CR-2026-045, SDD §3.4/§5).
//
// Implements the fixed architecture-design five-node slice as a single
// idempotent Reconcile(run): every wake source (start, cr:updated, task
// terminal, grant ACK, startup scan) funnels into Reconcile, which reads the
// generated registry, the existing run/node/task/CR/review/approval/checkpoint
// projections and performs at most one deterministic next action.
//
// The Runner never parses agent final text, blocker text or crctl stderr to
// decide routing, and never writes CR-controlled files or runs git — those
// remain the Agent (via Skill) + crctl boundary.
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Runner error codes (SDD §6).
const (
	RunnerErrUnsupportedPipeline    = "RUNNER_UNSUPPORTED_PIPELINE"
	RunnerErrRequiresAgentRoute     = "RUNNER_REQUIRES_AGENT_ROUTE"
	RunnerErrCRNotReady             = "RUNNER_CR_NOT_READY"
	RunnerErrAgentNotReady          = "RUNNER_AGENT_NOT_READY"
	RunnerErrSkillMissing           = "RUNNER_SKILL_MISSING"
	RunnerErrContractInvalid        = "RUNNER_CONTRACT_INVALID"
	RunnerErrTemplateDigestMismatch = "TEMPLATE_DIGEST_MISMATCH"
	RunnerErrAuthorityMismatch      = "RUNNER_AUTHORITY_MISMATCH"
	RunnerErrAttributionInvalid     = "RUNNER_ATTRIBUTION_INVALID"
	RunnerErrReviewEvidenceIncomp   = "RUNNER_REVIEW_EVIDENCE_INCOMPLETE"
	RunnerErrPipelineCrctlUnavail   = "PIPELINE_CRCTL_UNAVAILABLE"
	RunnerErrTaskFailed             = "RUNNER_TASK_FAILED"
	RunnerErrLoopExhausted          = "RUNNER_LOOP_EXHAUSTED"
)

// coreRegistry is the compiled architecture-design Core registry embedded by
// gen/generate-gate-nodes.mjs (ArchitectureCoreRegistryJSON).
type coreRegistry struct {
	Schema   string `json:"schema"`
	Pipeline struct {
		ID    string     `json:"id"`
		Nodes []coreNode `json:"nodes"`
	} `json:"pipeline"`
	PipelineOwner string `json:"pipelineOwner"`
	Digest        string `json:"digest"`
}

type coreNode struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Ref        string `json:"ref"`
	Prompt     string `json:"prompt"`
	OnFail     string `json:"onFail"`
	ReviewLoop *struct {
		ReplayPolicy string `json:"replayPolicy"`
		ReplayNodes  []struct {
			NodeID  string `json:"nodeId"`
			Ref     string `json:"ref"`
			Purpose string `json:"purpose"`
		} `json:"replayNodes"`
		MaxAttempts int `json:"maxAttempts"`
	} `json:"reviewLoop"`
}

// parseCoreRegistry decodes the embedded registry. It is parsed once at
// construction; a malformed registry is a startup contract failure.
func parseCoreRegistry() (*coreRegistry, error) {
	var r coreRegistry
	if err := json.Unmarshal([]byte(ArchitectureCoreRegistryJSON), &r); err != nil {
		return nil, fmt.Errorf("%s: malformed embedded registry: %w", RunnerErrContractInvalid, err)
	}
	if r.Pipeline.ID != PipelineIDs.ArchitectureDesign {
		return nil, fmt.Errorf("%s: registry pipeline %q != %q", RunnerErrContractInvalid, r.Pipeline.ID, PipelineIDs.ArchitectureDesign)
	}
	if r.Digest != ArchitectureCoreRegistryDigest {
		return nil, fmt.Errorf("%s: registry digest %q != embedded %q", RunnerErrContractInvalid, r.Digest, ArchitectureCoreRegistryDigest)
	}
	return &r, nil
}

// Runner drives the fixed architecture-design Core slice.
type Runner struct {
	pool     *pgxpool.Pool
	bus      *events.Bus
	tasks    *service.TaskService
	registry *coreRegistry
}

// NewRunner builds a Runner and fails closed if the embedded registry is
// inconsistent (digest / pipeline id / node contract).
func NewRunner(pool *pgxpool.Pool, bus *events.Bus, tasks *service.TaskService) (*Runner, error) {
	registry, err := parseCoreRegistry()
	if err != nil {
		return nil, err
	}
	return &Runner{pool: pool, bus: bus, tasks: tasks, registry: registry}, nil
}

// StartArchitectureInput carries the task-token-bound fields the Start
// endpoint resolved. Actor/user/agent are trusted only via the auth context,
// never the request body.
type StartArchitectureInput struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID // executor agent (task-token X-Agent-ID)
	TaskID      pgtype.UUID // source task (task-token X-Task-ID)
	UserID      pgtype.UUID // started_by (task-token X-User-ID)
	CrID        string
	TechContext string
}

// StartArchitecture validates the start contract and creates (or re-reads) the
// single non-terminal architecture run for the CR. It then reconciles once so
// the first node is enqueued in the same request path.
func (r *Runner) StartArchitecture(ctx context.Context, in StartArchitectureInput) (runID pgtype.UUID, changed bool, err error) {
	var crStatus string
	var needsReconcile bool
	err = r.pool.QueryRow(ctx, `
		SELECT status, needs_reconcile FROM cr
		WHERE workspace_id = $1 AND cr_id = $2`, in.WorkspaceID, in.CrID).Scan(&crStatus, &needsReconcile)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, errCode(RunnerErrCRNotReady, "CR projection missing")
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if crStatus != "requirement-approved" || needsReconcile {
		return pgtype.UUID{}, false, errCode(RunnerErrCRNotReady, "CR not requirement-approved or needs_reconcile")
	}
	if err := r.checkSourceTask(ctx, in.AgentID, in.TaskID); err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.checkExecutorAgent(ctx, in.WorkspaceID, in.AgentID); err != nil {
		return pgtype.UUID{}, false, err
	}

	execCtx, _ := json.Marshal(map[string]any{
		"runner":            "architecture-core/v1",
		"template_digest":   r.registry.Digest,
		"pipeline_owner":    r.registry.PipelineOwner,
		"executor_agent_id": in.AgentID.String(),
		"source_task_id":    in.TaskID.String(),
	})
	inputs, _ := json.Marshal(map[string]any{"cr_id": in.CrID, "tech_context": in.TechContext})
	runID, err = r.upsertRun(ctx, in.WorkspaceID, in.CrID, in.UserID, inputs, execCtx)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.Reconcile(ctx, in.WorkspaceID, in.CrID); err != nil {
		return runID, true, err
	}
	return runID, true, nil
}

// Reconcile is the single idempotent scheduler. It reads run/node/task/CR
// projections and performs at most one next action. Idempotent: re-delivery or
// restart converges to the same state.
func (r *Runner) Reconcile(ctx context.Context, workspaceID pgtype.UUID, crID string) error {
	run, err := r.findActiveRun(ctx, workspaceID, crID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no active run for this CR
	}
	if err != nil {
		return err
	}
	if err := r.checkDigest(ctx, run); err != nil {
		return err
	}
	crStatus, err := r.crStatus(ctx, workspaceID, crID)
	if err != nil {
		return err
	}
	current, err := r.currentNode(ctx, run.ID, crStatus)
	if err != nil {
		return err
	}
	if current == nil {
		return r.finishRun(ctx, run.ID)
	}
	// If the current node's row already has an active task, nothing to do.
	if current.NodeRunID.Valid {
		active, err := r.activeTaskForNode(ctx, current.NodeRunID)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		if err := r.failIfTerminalFailed(ctx, run.ID, current); err != nil {
			return err
		}
		if err := r.advancePassedNode(ctx, run.ID, current, crStatus); err != nil {
			return err
		}
	}
	// Loop exhaustion: canonical review block at maxAttempts stops the run.
	if exhausted, err := r.loopExhausted(ctx, run.ID, current, crStatus); err != nil {
		return err
	} else if exhausted {
		return errCode(RunnerErrLoopExhausted, "review loop exhausted")
	}
	return r.enqueueNode(ctx, run, current)
}

// --- helpers ----------------------------------------------------------------

func errCode(code, msg string) error { return fmt.Errorf("%s: %s", code, msg) }

func (r *Runner) checkSourceTask(ctx context.Context, agentID, taskID pgtype.UUID) error {
	var srcAgentID pgtype.UUID
	err := r.pool.QueryRow(ctx, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&srcAgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errCode(RunnerErrAttributionInvalid, "source task missing")
	}
	if err != nil {
		return err
	}
	if srcAgentID != agentID {
		return errCode(RunnerErrAttributionInvalid, "source task agent != executor agent")
	}
	return nil
}

func (r *Runner) checkExecutorAgent(ctx context.Context, workspaceID, agentID pgtype.UUID) error {
	var archivedAt *pgtype.Timestamptz
	var rtID pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT archived_at, runtime_id FROM agent
		WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID).Scan(&archivedAt, &rtID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errCode(RunnerErrAgentNotReady, "executor agent missing in workspace")
	}
	if err != nil {
		return err
	}
	if archivedAt != nil || !rtID.Valid {
		return errCode(RunnerErrAgentNotReady, "executor agent archived or unbound runtime")
	}
	return nil
}

type activeRun struct {
	ID              pgtype.UUID
	ExecutionContext json.RawMessage
	Inputs          json.RawMessage
}

func (r *Runner) findActiveRun(ctx context.Context, workspaceID pgtype.UUID, crID string) (activeRun, error) {
	var run activeRun
	err := r.pool.QueryRow(ctx, `
		SELECT id, execution_context, inputs FROM pipeline_run
		WHERE workspace_id = $1 AND cr_id = $2 AND pipeline_id = $3
		  AND status IN ('running', 'waiting_approval')
		ORDER BY created_at DESC LIMIT 1`,
		workspaceID, crID, PipelineIDs.ArchitectureDesign).Scan(&run.ID, &run.ExecutionContext, &run.Inputs)
	return run, err
}

func (r *Runner) checkDigest(ctx context.Context, run activeRun) error {
	var m map[string]any
	if err := json.Unmarshal(run.ExecutionContext, &m); err != nil {
		return err
	}
	if d, _ := m["template_digest"].(string); d != r.registry.Digest {
		if err := r.failRun(ctx, run.ID, RunnerErrTemplateDigestMismatch); err != nil {
			return err
		}
		return errCode(RunnerErrTemplateDigestMismatch, "template digest drift, run failed")
	}
	return nil
}

func (r *Runner) upsertRun(ctx context.Context, workspaceID pgtype.UUID, crID string, userID pgtype.UUID, inputs, execCtx []byte) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO pipeline_run (workspace_id, pipeline_id, cr_id, status, started_by, inputs, execution_context)
		VALUES ($1, $2, $3, 'running', $4, $5, $6)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		workspaceID, PipelineIDs.ArchitectureDesign, crID, userID, inputs, execCtx).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		run, rerr := r.findActiveRun(ctx, workspaceID, crID)
		if rerr != nil {
			return pgtype.UUID{}, rerr
		}
		return run.ID, nil
	}
	return id, err
}

func (r *Runner) crStatus(ctx context.Context, workspaceID pgtype.UUID, crID string) (string, error) {
	var s string
	err := r.pool.QueryRow(ctx, `SELECT status FROM cr WHERE workspace_id = $1 AND cr_id = $2`, workspaceID, crID).Scan(&s)
	return s, err
}

// nodeRun is a pipeline_node_run row (or a not-yet-materialized node).
type nodeRun struct {
	NodeRunID pgtype.UUID // pipeline_node_run.id (invalid if not yet materialized)
	NodeID    string      // template node UUID
	Ref       string      // skill id (empty for human_approval)
	Kind      string
	Status    string
	Attempt   int
	Seq       int
}

// currentNode returns the first not-yet-passed node in template order, or nil
// when every node has passed (run complete).
func (r *Runner) currentNode(ctx context.Context, runID pgtype.UUID, crStatus string) (*nodeRun, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, node_id, COALESCE(ref,''), kind, status, attempt, seq
		FROM pipeline_node_run WHERE run_id = $1 ORDER BY seq ASC, attempt ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bySeq := map[int][]nodeRun{}
	for rows.Next() {
		var n nodeRun
		if err := rows.Scan(&n.NodeRunID, &n.NodeID, &n.Ref, &n.Kind, &n.Status, &n.Attempt, &n.Seq); err != nil {
			return nil, err
		}
		bySeq[n.Seq] = append(bySeq[n.Seq], n)
	}
	for i, node := range r.registry.Pipeline.Nodes {
		seq := i + 1
		lasts := bySeq[seq]
		if node.Kind == "human_approval" {
			if r.approvalPassed(node, crStatus) {
				continue
			}
			// Waiting for approval: the human_approval node is current.
			return &nodeRun{NodeID: node.ID, Kind: node.Kind, Seq: seq, Attempt: 1}, nil
		}
		if len(lasts) == 0 {
			return &nodeRun{NodeID: node.ID, Ref: node.Ref, Kind: node.Kind, Seq: seq, Attempt: 1}, nil
		}
		cur := &lasts[len(lasts)-1]
		if cur.Status == "passed" {
			continue
		}
		if cur.Status == "blocked" {
			// A blocked review returns here for the repair attempt.
			return &nodeRun{NodeRunID: cur.NodeRunID, NodeID: cur.NodeID, Ref: cur.Ref, Kind: cur.Kind, Status: cur.Status, Attempt: cur.Attempt + 1, Seq: seq}, nil
		}
		return cur, nil
	}
	return nil, nil
}

func (r *Runner) approvalPassed(node coreNode, crStatus string) bool {
	// The tech-design human_approval node is passed once the CR leaves
	// tech-design-review-pending (approved → tech-design-reviewed).
	return crStatus == "tech-design-reviewed" || crStatus == "task-breakdown" || crStatus == "developing" ||
		crStatus == "code-reviewing" || crStatus == "code-approved" || crStatus == "merging" || crStatus == "writing-back" || crStatus == "archived"
}

func (r *Runner) activeTaskForNode(ctx context.Context, nodeRunID pgtype.UUID) (bool, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE pipeline_node_run_id = $1
		  AND status IN ('queued','deferred','dispatched','waiting_local_directory','running')`,
		nodeRunID).Scan(&n)
	return n > 0, err
}

func (r *Runner) failIfTerminalFailed(ctx context.Context, runID pgtype.UUID, n *nodeRun) error {
	var status string
	err := r.pool.QueryRow(ctx, `SELECT status FROM pipeline_node_run WHERE id = $1`, n.NodeRunID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "failed" {
		return r.failRun(ctx, runID, RunnerErrTaskFailed)
	}
	return nil
}

// advancePassedNode clears a stale "running" node whose task is terminal and
// whose authority postcondition is now satisfied — the next reconcile will see
// the successor node. A terminal task WITHOUT the authority postcondition is
// left in place (wait_reason) and no successor is scheduled (double success
// condition, SDD §5).
func (r *Runner) advancePassedNode(ctx context.Context, runID pgtype.UUID, n *nodeRun, crStatus string) error {
	var taskStatus string
	err := r.pool.QueryRow(ctx, `
		SELECT status FROM agent_task_queue WHERE pipeline_node_run_id = $1
		ORDER BY created_at DESC LIMIT 1`, n.NodeRunID).Scan(&taskStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no task yet: enqueue path handles it
	}
	if err != nil {
		return err
	}
	terminal := taskStatus == "completed" || taskStatus == "failed" || taskStatus == "cancelled"
	if !terminal {
		return nil
	}
	authorityOk := r.authorityPostcondition(n, crStatus)
	if !authorityOk {
		// Task terminal but authority fact lagging: wait, no successor.
		_, err = r.pool.Exec(ctx, `
			UPDATE pipeline_node_run SET detail = jsonb_set(COALESCE(detail,'{}'), '{runner,wait_reason}',
				'"authority_postcondition"', true) WHERE id = $1`, n.NodeRunID)
		return err
	}
	// Authority satisfied: mark the node passed so the successor becomes current.
	_, err = r.pool.Exec(ctx, `
		UPDATE pipeline_node_run SET status = 'passed', completed_at = now()
		WHERE id = $1 AND status IN ('running','pending')`, n.NodeRunID)
	return err
}

// authorityPostcondition maps a skill node to its CR/review authority fact
// (SDD §5). Only skill nodes have a task+authority double condition.
func (r *Runner) authorityPostcondition(n *nodeRun, crStatus string) bool {
	switch n.Ref {
	case "write-tech-design":
		return crStatus == "tech-design-review-pending" || crStatus == "tech-designing"
	case "approve-tech-design":
		return crStatus == "tech-design-reviewed" || crStatus == "task-breakdown" || crStatus == "developing"
	case "push-progress":
		// Completed only once the checkpoint projection exists (TASK-09 wiring).
		// Until the checkpoint evidence is projected, wait.
		return r.checkpointProjected(n)
	case "review-tech-design":
		// Review node authority is the review projection; block/pass both are
		// "done" — the successor is the human_approval node only on pass, and a
		// repair attempt on block (handled by currentNode + loopExhausted).
		return true
	default:
		return true
	}
}

func (r *Runner) checkpointProjected(n *nodeRun) bool {
	// Placeholder: checkpoint correlation is wired in TASK-09. Until then a
	// terminal push task is not advanced (authority pending) — safe by default.
	return false
}

// loopExhausted reports whether the canonical review projection is blocked at
// maxAttempts, in which case the Runner stops without creating attempt+1.
func (r *Runner) loopExhausted(ctx context.Context, runID pgtype.UUID, n *nodeRun, crStatus string) (bool, error) {
	if n.Ref != "review-tech-design" {
		return false, nil
	}
	var detail json.RawMessage
	var attempt int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(detail,'{}'), attempt FROM pipeline_node_run
		WHERE id = $1`, n.NodeRunID).Scan(&detail, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var m map[string]any
	_ = json.Unmarshal(detail, &m)
	maxA := 3
	if node := r.reviewNode(); node != nil && node.ReviewLoop != nil && node.ReviewLoop.MaxAttempts > 0 {
		maxA = node.ReviewLoop.MaxAttempts
	}
	return m["verdict"] == "block" && attempt >= maxA, nil
}

func (r *Runner) reviewNode() *coreNode {
	for i := range r.registry.Pipeline.Nodes {
		if r.registry.Pipeline.Nodes[i].Ref == "review-tech-design" {
			return &r.registry.Pipeline.Nodes[i]
		}
	}
	return nil
}

func (r *Runner) enqueueNode(ctx context.Context, run activeRun, n *nodeRun) error {
	var execCtx map[string]any
	if err := json.Unmarshal(run.ExecutionContext, &execCtx); err != nil {
		return err
	}
	agentID, _ := execCtx["executor_agent_id"].(string)
	sourceTaskID, _ := execCtx["source_task_id"].(string)
	if agentID == "" || sourceTaskID == "" {
		return errCode(RunnerErrAuthorityMismatch, "execution_context missing agent/source task")
	}
	var inputs map[string]any
	_ = json.Unmarshal(run.Inputs, &inputs)
	crID, _ := inputs["cr_id"].(string)
	if crID == "" {
		return errCode(RunnerErrAuthorityMismatch, "run inputs missing cr_id")
	}
	var wsID pgtype.UUID
	_ = r.pool.QueryRow(ctx, `SELECT workspace_id FROM pipeline_run WHERE id = $1`, run.ID).Scan(&wsID)
	nodeRunID, err := r.ensureNodeRow(ctx, run.ID, n)
	if err != nil {
		return err
	}
	prompt := r.promptFor(n)
	spec := service.PipelineTaskSpec{
		WorkspaceID:     wsID,
		CrID:            crID,
		RunID:           run.ID,
		NodeID:          mustUUID(n.NodeID),
		NodeRunID:       nodeRunID,
		PipelineID:      PipelineIDs.ArchitectureDesign,
		Attempt:         n.Attempt,
		Prompt:          prompt,
		SourceTaskID:    mustUUID(sourceTaskID),
		ExecutorAgentID: mustUUID(agentID),
		Priority:        0,
	}
	_, err = r.tasks.EnqueuePipelineTask(ctx, spec)
	if errors.Is(err, service.ErrRunnerAttributionInvalid) {
		return errCode(RunnerErrAttributionInvalid, "pipeline task enqueue guard failed")
	}
	return err
}

func (r *Runner) ensureNodeRow(ctx context.Context, runID pgtype.UUID, n *nodeRun) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO pipeline_node_run (run_id, node_id, ref, kind, seq, status, attempt, started_at)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, now())
		ON CONFLICT (run_id, node_id, attempt) DO UPDATE
		  SET status = 'running', started_at = COALESCE(pipeline_node_run.started_at, now())
		RETURNING id`,
		runID, n.NodeID, nullIfEmpty(n.Ref), n.Kind, n.Seq, n.Attempt).Scan(&id)
	return id, err
}

func (r *Runner) promptFor(n *nodeRun) string {
	for _, node := range r.registry.Pipeline.Nodes {
		if node.ID == n.NodeID {
			return node.Prompt
		}
	}
	return ""
}

func (r *Runner) failRun(ctx context.Context, runID pgtype.UUID, code string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE pipeline_run SET status = 'failed', completed_at = now()
		WHERE id = $1 AND status IN ('running','waiting_approval')`, runID)
	if err != nil {
		return err
	}
	_, _ = r.pool.Exec(ctx, `
		UPDATE pipeline_node_run SET detail = jsonb_set(COALESCE(detail,'{}'), '{runner,error}',
			jsonb_build_object('code', $2), true)
		WHERE run_id = $1 AND status = 'running'`, runID, code)
	return errCode(code, "run failed")
}

func (r *Runner) finishRun(ctx context.Context, runID pgtype.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE pipeline_run SET status = 'completed', completed_at = now()
		WHERE id = $1 AND status IN ('running','waiting_approval')`, runID)
	return err
}

func mustUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}

// WireEvents subscribes the Runner to its wake sources (cr:updated and task
// terminal). Events are only wake signals, never authority; a lost wake is
// healed by a later event or StartupScan.
func (r *Runner) WireEvents(bus *events.Bus) {
	bus.Subscribe(EventCRUpdated, func(e events.Event) {
		if e.WorkspaceID == "" {
			return
		}
		p, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		crID, _ := p["cr_id"].(string)
		if crID == "" {
			return
		}
		_ = r.Reconcile(context.Background(), mustUUID(e.WorkspaceID), crID)
	})
	for _, evType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed} {
		bus.Subscribe(evType, func(e events.Event) {
			if e.TaskID == "" {
				return
			}
			_ = r.reconcileFromTaskEvent(context.Background(), e.TaskID)
		})
	}
}

// StartupScan reconciles every non-terminal Core run once at server start.
func (r *Runner) StartupScan(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
		SELECT workspace_id, cr_id FROM pipeline_run
		WHERE pipeline_id = $1 AND status IN ('running','waiting_approval')`,
		PipelineIDs.ArchitectureDesign)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ws pgtype.UUID
		var cr string
		if err := rows.Scan(&ws, &cr); err != nil {
			return err
		}
		if err := r.Reconcile(ctx, ws, cr); err != nil {
			slog.Error("runner startup scan reconcile failed", "cr_id", cr, "error", err)
		}
	}
	return rows.Err()
}

// HandleStartArchitecture is POST /api/workspaces/{workspaceID}/pipeline-runs.
// It only accepts the server-set task-token actor (X-Actor-Source=task_token);
// workspace/agent/task/user IDs come from the auth middleware headers, never
// the request body.
func (r *Runner) HandleStartArchitecture(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("X-Actor-Source") != "task_token" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": RunnerErrRequiresAgentRoute})
		return
	}
	workspaceID := req.Header.Get("X-Workspace-ID")
	agentID := req.Header.Get("X-Agent-ID")
	taskID := req.Header.Get("X-Task-ID")
	userID := req.Header.Get("X-User-ID")
	if workspaceID == "" || agentID == "" || taskID == "" || userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task-token headers"})
		return
	}
	var body struct {
		PipelineID string `json:"pipeline_id"`
		CrID       string `json:"cr_id"`
		Inputs     struct {
			TechContext string `json:"tech_context"`
		} `json:"inputs"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if body.PipelineID != PipelineIDs.ArchitectureDesign {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": RunnerErrUnsupportedPipeline})
		return
	}
	runID, changed, err := r.StartArchitecture(req.Context(), StartArchitectureInput{
		WorkspaceID: mustUUID(workspaceID),
		AgentID:     mustUUID(agentID),
		TaskID:      mustUUID(taskID),
		UserID:      mustUUID(userID),
		CrID:        body.CrID,
		TechContext: body.Inputs.TechContext,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "changed": changed})
}

func (r *Runner) reconcileFromTaskEvent(ctx context.Context, taskID string) error {
	var nodeRunID pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT pipeline_node_run_id FROM agent_task_queue WHERE id = $1`, mustUUID(taskID)).Scan(&nodeRunID)
	if errors.Is(err, pgx.ErrNoRows) || !nodeRunID.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	var ws pgtype.UUID
	var cr string
	err = r.pool.QueryRow(ctx, `
		SELECT pr.workspace_id, pr.cr_id FROM pipeline_run pr
		JOIN pipeline_node_run pnr ON pnr.run_id = pr.id
		WHERE pnr.id = $1`, nodeRunID).Scan(&ws, &cr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.Reconcile(ctx, ws, cr)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
