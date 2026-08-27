// AIFIRST: Runner Core (CR-2026-045, SDD §3.4/§5).
//
// This is intentionally not a generic workflow engine. It drives the fixed
// architecture-design slice with one idempotent Reconcile entry point and
// leaves CR state, approval validation, and Git effects to Skills + crctl.
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

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
	RunnerErrApprovalRejected       = "RUNNER_APPROVAL_REJECTED"
	RunnerErrLoopExhausted          = "RUNNER_LOOP_EXHAUSTED"
)

const (
	runnerSchema        = "architecture-core/v1"
	maxTechContextBytes = 16 * 1024
)

var crIDPattern = regexp.MustCompile(`^CR-[0-9]{4}-[0-9]{3,}$`)

// ArchitectureRunnerEnabled is the deployment switch. Default-off preserves
// the existing manual Skill + crctl route; enabling only mounts/wires Core.
func ArchitectureRunnerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AIFIRST_ARCHITECTURE_RUNNER"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type coreRegistry struct {
	Schema   string `json:"schema"`
	Pipeline struct {
		ID    string     `json:"id"`
		Nodes []coreNode `json:"nodes"`
	} `json:"pipeline"`
	PipelineOwner   string               `json:"pipelineOwner"`
	NodePermissions []coreNodePermission `json:"nodePermissions"`
	Digest          string               `json:"digest"`
}

type coreNodePermission struct {
	Ref                  string `json:"ref"`
	Owner                string `json:"owner"`
	PipelineOwnerCanCall bool   `json:"pipelineOwnerCanCall"`
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

func parseCoreRegistry() (*coreRegistry, error) {
	var r coreRegistry
	if err := json.Unmarshal([]byte(ArchitectureCoreRegistryJSON), &r); err != nil {
		return nil, errCode(RunnerErrContractInvalid, "malformed embedded registry")
	}
	if r.Schema != "ai-first.pipeline-registry/architecture-core-v1" || r.Pipeline.ID != PipelineIDs.ArchitectureDesign || r.Digest != ArchitectureCoreRegistryDigest {
		return nil, errCode(RunnerErrContractInvalid, "registry identity or digest mismatch")
	}
	expected := []struct{ kind, ref string }{
		{"skill", "write-tech-design"},
		{"skill", "review-tech-design"},
		{"human_approval", ""},
		{"skill", "approve-tech-design"},
		{"skill", "push-progress"},
	}
	if len(r.Pipeline.Nodes) != len(expected) {
		return nil, errCode(RunnerErrContractInvalid, "architecture Core must have five nodes")
	}
	seenIDs := map[string]bool{}
	for i, node := range r.Pipeline.Nodes {
		if node.ID == "" || seenIDs[node.ID] || node.Kind != expected[i].kind || node.Ref != expected[i].ref || node.OnFail != "abort" {
			return nil, errCode(RunnerErrContractInvalid, fmt.Sprintf("invalid node contract at seq %d", i+1))
		}
		if _, err := parseUUID(node.ID); err != nil {
			return nil, errCode(RunnerErrContractInvalid, "node id is not UUID")
		}
		seenIDs[node.ID] = true
	}
	review := r.Pipeline.Nodes[1]
	if review.ReviewLoop == nil || review.ReviewLoop.ReplayPolicy != "rerun-listed-nodes-in-order" || review.ReviewLoop.MaxAttempts != 3 || len(review.ReviewLoop.ReplayNodes) != 2 ||
		review.ReviewLoop.ReplayNodes[0].NodeID != r.Pipeline.Nodes[0].ID || review.ReviewLoop.ReplayNodes[0].Ref != "write-tech-design" ||
		review.ReviewLoop.ReplayNodes[1].NodeID != r.Pipeline.Nodes[1].ID || review.ReviewLoop.ReplayNodes[1].Ref != "review-tech-design" {
		return nil, errCode(RunnerErrContractInvalid, "reviewLoop contract invalid")
	}
	permissions := map[string]coreNodePermission{}
	for _, p := range r.NodePermissions {
		if p.Ref == "" || p.Owner == "" || !p.PipelineOwnerCanCall {
			return nil, errCode(RunnerErrContractInvalid, "node permission invalid")
		}
		if _, exists := permissions[p.Ref]; exists {
			return nil, errCode(RunnerErrContractInvalid, "node permission owner not unique")
		}
		permissions[p.Ref] = p
	}
	for _, node := range r.Pipeline.Nodes {
		if node.Kind == "skill" {
			if _, ok := permissions[node.Ref]; !ok {
				return nil, errCode(RunnerErrContractInvalid, "skill permission missing: "+node.Ref)
			}
		}
	}
	return &r, nil
}

type pipelineTaskEnqueuer interface {
	EnqueuePipelineTask(context.Context, service.PipelineTaskSpec) (db.AgentTaskQueue, error)
}

type Runner struct {
	pool     *pgxpool.Pool
	tasks    pipelineTaskEnqueuer
	registry *coreRegistry
}

func NewRunner(pool *pgxpool.Pool, _ *events.Bus, tasks pipelineTaskEnqueuer) (*Runner, error) {
	registry, err := parseCoreRegistry()
	if err != nil {
		return nil, err
	}
	return &Runner{pool: pool, tasks: tasks, registry: registry}, nil
}

type StartArchitectureInput struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	TaskID      pgtype.UUID
	UserID      pgtype.UUID
	CrID        string
	TechContext string
}

type activeRun struct {
	ID               pgtype.UUID
	WorkspaceID      pgtype.UUID
	CRID             string
	ExecutionContext json.RawMessage
	Inputs           json.RawMessage
}

func (r *Runner) StartArchitecture(ctx context.Context, in StartArchitectureInput) (pgtype.UUID, bool, error) {
	if !in.WorkspaceID.Valid || !in.AgentID.Valid || !in.TaskID.Valid || !in.UserID.Valid || !crIDPattern.MatchString(in.CrID) {
		return pgtype.UUID{}, false, errCode(RunnerErrAuthorityMismatch, "invalid bound IDs or CR ID")
	}
	if len([]byte(in.TechContext)) > maxTechContextBytes {
		return pgtype.UUID{}, false, errCode(RunnerErrContractInvalid, "tech_context exceeds 16 KiB")
	}
	var crStatus string
	var needsReconcile bool
	err := r.pool.QueryRow(ctx, `SELECT status, needs_reconcile FROM cr WHERE workspace_id = $1 AND cr_id = $2`, in.WorkspaceID, in.CrID).Scan(&crStatus, &needsReconcile)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (crStatus != "requirement-approved" || needsReconcile)) {
		return pgtype.UUID{}, false, errCode(RunnerErrCRNotReady, "CR projection not ready")
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.checkSourceTask(ctx, in.WorkspaceID, in.AgentID, in.TaskID); err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.checkExecutorAgent(ctx, in.WorkspaceID, in.AgentID); err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.checkExecutorSkills(ctx, in.AgentID); err != nil {
		return pgtype.UUID{}, false, err
	}
	execCtx, err := json.Marshal(map[string]any{
		"runner": runnerSchema, "template_digest": r.registry.Digest,
		"pipeline_owner": r.registry.PipelineOwner, "executor_agent_id": in.AgentID.String(), "source_task_id": in.TaskID.String(),
	})
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	inputs, err := json.Marshal(map[string]any{"cr_id": in.CrID, "tech_context": in.TechContext})
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	runID, changed, err := r.upsertRun(ctx, in.WorkspaceID, in.CrID, in.UserID, inputs, execCtx)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if err := r.Reconcile(ctx, in.WorkspaceID, in.CrID); err != nil {
		return runID, changed, err
	}
	return runID, changed, nil
}

// Reconcile serializes on a PostgreSQL advisory lock derived from the run ID.
// Every wake source uses this method; partial unique indexes remain the
// cross-process crash-window backstop for run and task creation.
func (r *Runner) Reconcile(ctx context.Context, workspaceID pgtype.UUID, crID string) error {
	run, err := r.findActiveRun(ctx, workspaceID, crID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	lockConn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, run.ID.String()); err != nil {
		return err
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, run.ID.String())
	}()

	run, err = r.findActiveRun(ctx, workspaceID, crID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.checkDigest(ctx, run); err != nil {
		return err
	}
	return r.reconcileLocked(ctx, run)
}

func (r *Runner) reconcileLocked(ctx context.Context, run activeRun) error {
	writeNode, reviewNode, humanNode, approveNode, pushNode := r.registry.Pipeline.Nodes[0], r.registry.Pipeline.Nodes[1], r.registry.Pipeline.Nodes[2], r.registry.Pipeline.Nodes[3], r.registry.Pipeline.Nodes[4]
	maxAttempts := reviewNode.ReviewLoop.MaxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		status, err := r.crStatus(ctx, run.WorkspaceID, run.CRID)
		if err != nil {
			return err
		}
		complete, stop, err := r.reconcileSkillTask(ctx, run, writeNode, 1, attempt)
		if err != nil || stop {
			return err
		}
		if !complete {
			return r.waitAuthority(ctx, run.ID, writeNode, attempt)
		}
		writeReady := status == "tech-design-review-pending" || status == "tech-design-reviewed"
		approvalRejected := false
		if status == "tech-designing" {
			decision, _, delivered, approvalErr := r.deliveredTechApproval(ctx, run.WorkspaceID, run.CRID)
			if approvalErr != nil {
				return approvalErr
			}
			approvalRejected = delivered && decision == "reject"
		}
		if !writeReady && status == "tech-designing" {
			// After a canonical block the CR intentionally returns to designing;
			// allow the completed prior round to replay so later attempts remain
			// reachable on every wake, not only in the first reconcile call.
			if prior, evidenceErr := r.reviewEvidence(ctx, run.ID, reviewNode.ID, attempt); evidenceErr == nil {
				writeReady = prior.Verdict == "block" && prior.Attempt == attempt && len(prior.Blockers) > 0 && prior.SubjectSHA256 != "" && prior.ReviewedAt != ""
			}
			// approve-tech-design applies a signed reject by rolling the CR back
			// to designing. Preserve reachability of node 4 so that business
			// result can terminate this run explicitly.
			writeReady = writeReady || approvalRejected
		}
		if !writeReady {
			return r.waitAuthority(ctx, run.ID, writeNode, attempt)
		}
		if err := r.markNode(ctx, run.ID, writeNode, attempt, "passed"); err != nil {
			return err
		}

		complete, stop, err = r.reconcileSkillTask(ctx, run, reviewNode, 2, attempt)
		if err != nil || stop {
			return err
		}
		if !complete {
			return nil
		}
		evidence, err := r.reviewEvidence(ctx, run.ID, reviewNode.ID, attempt)
		if err != nil {
			return err
		}
		status, err = r.crStatus(ctx, run.WorkspaceID, run.CRID)
		if err != nil {
			return err
		}
		switch evidence.Verdict {
		case "block":
			if evidence.Attempt != attempt || len(evidence.Blockers) == 0 || evidence.SubjectSHA256 == "" || evidence.ReviewedAt == "" {
				return r.waitEvidence(ctx, run.ID, reviewNode, attempt, RunnerErrReviewEvidenceIncomp)
			}
			if status != "tech-designing" {
				replayStarted, replayErr := r.nodeAttemptExists(ctx, run.ID, writeNode.ID, attempt+1)
				if replayErr != nil {
					return replayErr
				}
				if !replayStarted {
					return r.waitAuthority(ctx, run.ID, reviewNode, attempt)
				}
			}
			if err := r.markNode(ctx, run.ID, reviewNode, attempt, "blocked"); err != nil {
				return err
			}
			if attempt == maxAttempts {
				return r.failRun(ctx, run.ID, RunnerErrLoopExhausted)
			}
			continue
		case "pass":
			if evidence.Attempt != attempt || len(evidence.Blockers) != 0 || evidence.SubjectSHA256 == "" || evidence.ReviewedAt == "" {
				return r.waitEvidence(ctx, run.ID, reviewNode, attempt, RunnerErrReviewEvidenceIncomp)
			}
			if status != "tech-design-review-pending" && status != "tech-design-reviewed" && !approvalRejected {
				return r.waitAuthority(ctx, run.ID, reviewNode, attempt)
			}
			if err := r.markNode(ctx, run.ID, reviewNode, attempt, "passed"); err != nil {
				return err
			}
			return r.reconcileApprovalAndCheckpoint(ctx, run, humanNode, approveNode, pushNode)
		default:
			return r.waitEvidence(ctx, run.ID, reviewNode, attempt, RunnerErrReviewEvidenceIncomp)
		}
	}
	return r.failRun(ctx, run.ID, RunnerErrLoopExhausted)
}

func (r *Runner) reconcileApprovalAndCheckpoint(ctx context.Context, run activeRun, human, approve, push coreNode) error {
	humanID, err := r.ensureNodeRow(ctx, run.ID, human, 3, 1)
	if err != nil {
		return err
	}
	decision, approvalID, delivered, err := r.deliveredTechApproval(ctx, run.WorkspaceID, run.CRID)
	if err != nil {
		return err
	}
	if !delivered {
		_, err = r.pool.Exec(ctx, `UPDATE pipeline_run SET status='waiting_approval' WHERE id=$1 AND status='running'`, run.ID)
		return err
	}
	if _, err = r.pool.Exec(ctx, `UPDATE pipeline_node_run SET status='passed', completed_at=now(), approval_id=$2 WHERE id=$1 AND status IN ('pending','running','passed')`, humanID, approvalID); err != nil {
		return err
	}
	_, _ = r.pool.Exec(ctx, `UPDATE pipeline_run SET status='running' WHERE id=$1 AND status='waiting_approval'`, run.ID)

	complete, stop, err := r.reconcileSkillTask(ctx, run, approve, 4, 1)
	if err != nil || stop {
		if decision == "reject" {
			status, serr := r.crStatus(ctx, run.WorkspaceID, run.CRID)
			if serr == nil && status == "tech-designing" {
				return r.failRun(ctx, run.ID, RunnerErrTaskFailed)
			}
		}
		return err
	}
	if !complete {
		return nil
	}
	status, err := r.crStatus(ctx, run.WorkspaceID, run.CRID)
	if err != nil {
		return err
	}
	if decision == "reject" {
		if status != "tech-designing" {
			return r.waitAuthority(ctx, run.ID, approve, 1)
		}
		return r.failRun(ctx, run.ID, RunnerErrApprovalRejected)
	}
	if decision != "approve" || status != "tech-design-reviewed" {
		return r.waitAuthority(ctx, run.ID, approve, 1)
	}
	if err := r.markNode(ctx, run.ID, approve, 1, "passed"); err != nil {
		return err
	}

	complete, stop, err = r.reconcileCheckpointTask(ctx, run, push)
	if err != nil || stop {
		return err
	}
	if !complete {
		return nil
	}
	ok, err := r.checkpointProjected(ctx, run.ID, push.ID)
	if err != nil {
		return err
	}
	if !ok {
		return r.waitAuthority(ctx, run.ID, push, 1)
	}
	if err := r.markNode(ctx, run.ID, push, 1, "passed"); err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `UPDATE pipeline_run SET status='completed', completed_at=now() WHERE id=$1 AND status IN ('running','waiting_approval')`, run.ID)
	return err
}

// reconcileSkillTask returns complete=true only for a completed Agent task.
// stop=true means it enqueued/waits on an active task, so this wake is done.
func (r *Runner) reconcileSkillTask(ctx context.Context, run activeRun, node coreNode, seq, attempt int) (complete, stop bool, err error) {
	nodeRunID, err := r.ensureNodeRow(ctx, run.ID, node, seq, attempt)
	if err != nil {
		return false, false, err
	}
	state, exists, err := r.latestTaskState(ctx, nodeRunID)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, true, r.enqueueNode(ctx, run, node, nodeRunID, attempt)
	}
	switch state {
	case "queued", "deferred", "dispatched", "waiting_local_directory", "running":
		return false, true, nil
	case "completed":
		return true, false, nil
	case "failed", "cancelled":
		return false, true, r.failRun(ctx, run.ID, RunnerErrTaskFailed)
	default:
		return false, true, r.failRun(ctx, run.ID, RunnerErrAuthorityMismatch)
	}
}

// Checkpoint is the one recoverable skill failure in Core. The CR is already
// approved, so a terminal task creates one successor for the same node run;
// the active-task partial unique index makes duplicate wakes idempotent.
func (r *Runner) reconcileCheckpointTask(ctx context.Context, run activeRun, node coreNode) (complete, stop bool, err error) {
	nodeRunID, err := r.ensureNodeRow(ctx, run.ID, node, 5, 1)
	if err != nil {
		return false, false, err
	}
	state, exists, err := r.latestTaskState(ctx, nodeRunID)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, true, r.enqueueNode(ctx, run, node, nodeRunID, 1)
	}
	switch state {
	case "queued", "deferred", "dispatched", "waiting_local_directory", "running":
		return false, true, nil
	case "completed":
		return true, false, nil
	case "failed", "cancelled":
		if err := r.setRunnerDetail(ctx, run.ID, node.ID, 1, map[string]any{"wait_reason": "checkpoint_retry", "previous_task_status": state}); err != nil {
			return false, false, err
		}
		return false, true, r.enqueueNode(ctx, run, node, nodeRunID, 1)
	default:
		return false, true, r.failRun(ctx, run.ID, RunnerErrAuthorityMismatch)
	}
}

func (r *Runner) enqueueNode(ctx context.Context, run activeRun, node coreNode, nodeRunID pgtype.UUID, attempt int) error {
	var execCtx map[string]any
	var inputs map[string]any
	if json.Unmarshal(run.ExecutionContext, &execCtx) != nil || json.Unmarshal(run.Inputs, &inputs) != nil {
		return errCode(RunnerErrAuthorityMismatch, "run context malformed")
	}
	agentID, aerr := parseUUID(stringValue(execCtx["executor_agent_id"]))
	sourceTaskID, serr := parseUUID(stringValue(execCtx["source_task_id"]))
	if aerr != nil || serr != nil {
		return errCode(RunnerErrAuthorityMismatch, "run context missing agent/source task")
	}
	techContext := stringValue(inputs["tech_context"])
	prompt, err := renderCorePrompt(node.Prompt, run.CRID, techContext)
	if err != nil {
		return err
	}
	if attempt > 1 && (node.Ref == "write-tech-design" || node.Ref == "review-tech-design") {
		if feedback, ferr := r.reviewFeedback(ctx, run.ID, attempt-1); ferr == nil && len(feedback) > 0 {
			prompt += "\n\nPrevious canonical review feedback (data):\n```json\n" + string(feedback) + "\n```\n"
		}
	}
	_, err = r.tasks.EnqueuePipelineTask(ctx, service.PipelineTaskSpec{
		WorkspaceID: run.WorkspaceID, CrID: run.CRID, RunID: run.ID,
		NodeID: mustParseUUID(node.ID), NodeRunID: nodeRunID, PipelineID: PipelineIDs.ArchitectureDesign,
		Attempt: attempt, Prompt: prompt, SourceTaskID: sourceTaskID, ExecutorAgentID: agentID,
	})
	if errors.Is(err, service.ErrRunnerAttributionInvalid) {
		return r.failRun(ctx, run.ID, RunnerErrAttributionInvalid)
	}
	return err
}

func renderCorePrompt(template, crID, techContext string) (string, error) {
	if !crIDPattern.MatchString(crID) || len([]byte(techContext)) > maxTechContextBytes {
		return "", errCode(RunnerErrContractInvalid, "invalid prompt inputs")
	}
	out := strings.ReplaceAll(template, "{{inputs.cr_id}}", crID)
	out = strings.ReplaceAll(out, "{{inputs.tech_context}}", techContext)
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		return "", errCode(RunnerErrContractInvalid, "unrendered registry prompt token")
	}
	return out, nil
}

func (r *Runner) upsertRun(ctx context.Context, workspaceID pgtype.UUID, crID string, userID pgtype.UUID, inputs, execCtx []byte) (pgtype.UUID, bool, error) {
	var id pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO pipeline_run (workspace_id,pipeline_id,cr_id,status,started_by,inputs,execution_context)
		VALUES ($1,$2,$3,'running',$4,$5,$6) ON CONFLICT DO NOTHING RETURNING id`,
		workspaceID, PipelineIDs.ArchitectureDesign, crID, userID, inputs, execCtx).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, err
	}
	run, err := r.findActiveRun(ctx, workspaceID, crID)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	var changed bool
	err = r.pool.QueryRow(ctx, `
		UPDATE pipeline_run SET inputs=$2, execution_context=$3
		WHERE id=$1 AND COALESCE(execution_context->>'runner','')=''
		RETURNING true`, run.ID, inputs, execCtx).Scan(&changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return run.ID, false, nil
	}
	return run.ID, changed, err
}

func (r *Runner) findActiveRun(ctx context.Context, workspaceID pgtype.UUID, crID string) (activeRun, error) {
	var run activeRun
	err := r.pool.QueryRow(ctx, `
		SELECT id,workspace_id,cr_id,execution_context,inputs FROM pipeline_run
		WHERE workspace_id=$1 AND cr_id=$2 AND pipeline_id=$3 AND status IN ('running','waiting_approval')
		ORDER BY created_at DESC LIMIT 1`, workspaceID, crID, PipelineIDs.ArchitectureDesign).
		Scan(&run.ID, &run.WorkspaceID, &run.CRID, &run.ExecutionContext, &run.Inputs)
	return run, err
}

func (r *Runner) checkDigest(_ context.Context, run activeRun) error {
	var m map[string]any
	if json.Unmarshal(run.ExecutionContext, &m) != nil || stringValue(m["runner"]) != runnerSchema || stringValue(m["template_digest"]) != r.registry.Digest {
		// Registry drift is a deployment mismatch, not a terminal business
		// result. Keep the run active so rolling back to its digest resumes it.
		return errCode(RunnerErrTemplateDigestMismatch, "active run registry digest does not match this deployment")
	}
	return nil
}

func (r *Runner) checkSourceTask(ctx context.Context, workspaceID, agentID, taskID pgtype.UUID) error {
	var found bool
	err := r.pool.QueryRow(ctx, `
		SELECT true FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
		WHERE t.id=$1 AND t.agent_id=$2 AND a.workspace_id=$3`, taskID, agentID, workspaceID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return errCode(RunnerErrAttributionInvalid, "source task/agent/workspace mismatch")
	}
	return err
}

func (r *Runner) checkExecutorAgent(ctx context.Context, workspaceID, agentID pgtype.UUID) error {
	var ready bool
	err := r.pool.QueryRow(ctx, `SELECT archived_at IS NULL AND runtime_id IS NOT NULL FROM agent WHERE id=$1 AND workspace_id=$2`, agentID, workspaceID).Scan(&ready)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ready) {
		return errCode(RunnerErrAgentNotReady, "executor agent missing, archived, or runtime-unbound")
	}
	return err
}

func (r *Runner) checkExecutorSkills(ctx context.Context, agentID pgtype.UUID) error {
	rows, err := r.pool.Query(ctx, `SELECT s.name FROM skill s JOIN agent_skill x ON x.skill_id=s.id WHERE x.agent_id=$1 AND x.enabled=true`, agentID)
	if err != nil {
		return err
	}
	defer rows.Close()
	enabled := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		enabled[name] = true
	}
	for _, node := range r.registry.Pipeline.Nodes {
		if node.Kind == "skill" && !enabled[node.Ref] {
			return errCode(RunnerErrSkillMissing, "executor agent missing skill: "+node.Ref)
		}
	}
	return rows.Err()
}

func (r *Runner) ensureNodeRow(ctx context.Context, runID pgtype.UUID, node coreNode, seq, attempt int) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO pipeline_node_run (run_id,node_id,ref,kind,seq,status,attempt,started_at)
		VALUES ($1,$2,$3,$4,$5,'running',$6,now())
		ON CONFLICT (run_id,node_id,attempt) DO UPDATE SET started_at=COALESCE(pipeline_node_run.started_at,now())
		RETURNING id`, runID, node.ID, nullIfEmpty(node.Ref), node.Kind, seq, attempt).Scan(&id)
	return id, err
}

func (r *Runner) nodeAttemptExists(ctx context.Context, runID pgtype.UUID, nodeID string, attempt int) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_node_run WHERE run_id=$1 AND node_id=$2::uuid AND attempt=$3)`, runID, nodeID, attempt).Scan(&exists)
	return exists, err
}

func (r *Runner) latestTaskState(ctx context.Context, nodeRunID pgtype.UUID) (string, bool, error) {
	var state string
	err := r.pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE pipeline_node_run_id=$1 ORDER BY created_at DESC LIMIT 1`, nodeRunID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return state, err == nil, err
}

type reviewEvidence struct {
	Verdict       string   `json:"verdict"`
	Blockers      []string `json:"blockers"`
	Attempt       int      `json:"attempt"`
	ReviewedAt    string   `json:"reviewed_at"`
	SubjectSHA256 string   `json:"subject_sha256"`
}

func (r *Runner) reviewEvidence(ctx context.Context, runID pgtype.UUID, nodeID string, attempt int) (reviewEvidence, error) {
	var raw json.RawMessage
	err := r.pool.QueryRow(ctx, `SELECT detail FROM pipeline_node_run WHERE run_id=$1 AND node_id=$2 AND attempt=$3`, runID, nodeID, attempt).Scan(&raw)
	if err != nil {
		return reviewEvidence{}, err
	}
	var e reviewEvidence
	if err := json.Unmarshal(raw, &e); err != nil {
		return reviewEvidence{}, err
	}
	return e, nil
}

func (r *Runner) reviewFeedback(ctx context.Context, runID pgtype.UUID, attempt int) (json.RawMessage, error) {
	returnRaw := json.RawMessage{}
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('attempt',attempt,'verdict',detail->'verdict','blockers',detail->'blockers') FROM pipeline_node_run WHERE run_id=$1 AND node_id=$2 AND attempt=$3`, runID, r.registry.Pipeline.Nodes[1].ID, attempt).Scan(&returnRaw)
	return returnRaw, err
}

func (r *Runner) deliveredTechApproval(ctx context.Context, workspaceID pgtype.UUID, crID string) (string, pgtype.UUID, bool, error) {
	var decision string
	var id pgtype.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT decision,id FROM approval_record WHERE workspace_id=$1 AND cr_id=$2 AND stage='tech-design' AND delivered_at IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, workspaceID, crID).Scan(&decision, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", pgtype.UUID{}, false, nil
	}
	return decision, id, err == nil, err
}

func (r *Runner) checkpointProjected(ctx context.Context, runID pgtype.UUID, pushNodeID string) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pipeline_node_run n JOIN pipeline_run r ON r.id=n.run_id
		  JOIN cr_sync_event e ON e.cr_id=r.cr_id AND e.workspace_id=r.workspace_id AND e.event_kind='checkpoint' AND e.commit_sha<>'' AND e.occurred_at>=n.started_at
		  WHERE n.run_id=$1 AND n.node_id=$2 AND n.attempt=1
		)`, runID, pushNodeID).Scan(&ok)
	return ok, err
}

func (r *Runner) crStatus(ctx context.Context, workspaceID pgtype.UUID, crID string) (string, error) {
	var status string
	err := r.pool.QueryRow(ctx, `SELECT status FROM cr WHERE workspace_id=$1 AND cr_id=$2`, workspaceID, crID).Scan(&status)
	return status, err
}

func (r *Runner) markNode(ctx context.Context, runID pgtype.UUID, node coreNode, attempt int, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE pipeline_node_run SET status=$4,completed_at=now(),detail=jsonb_set(COALESCE(detail,'{}'),'{runner}',COALESCE(detail->'runner','{}') - 'wait_reason',true) WHERE run_id=$1 AND node_id=$2 AND attempt=$3 AND status NOT IN ('failed','skipped')`, runID, node.ID, attempt, status)
	return err
}

func (r *Runner) waitAuthority(ctx context.Context, runID pgtype.UUID, node coreNode, attempt int) error {
	return r.setRunnerDetail(ctx, runID, node.ID, attempt, map[string]any{"wait_reason": "authority_postcondition"})
}

func (r *Runner) waitEvidence(ctx context.Context, runID pgtype.UUID, node coreNode, attempt int, code string) error {
	return r.setRunnerDetail(ctx, runID, node.ID, attempt, map[string]any{"wait_reason": "review_evidence", "error": map[string]any{"code": code}})
}

func (r *Runner) setRunnerDetail(ctx context.Context, runID pgtype.UUID, nodeID string, attempt int, detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `UPDATE pipeline_node_run SET detail=jsonb_set(COALESCE(detail,'{}'),'{runner}',$4::jsonb,true) WHERE run_id=$1 AND node_id=$2 AND attempt=$3`, runID, nodeID, attempt, raw)
	return err
}

func (r *Runner) failRun(ctx context.Context, runID pgtype.UUID, code string) error {
	_, err := r.pool.Exec(ctx, `UPDATE pipeline_run SET status='failed',completed_at=now() WHERE id=$1 AND status IN ('running','waiting_approval')`, runID)
	if err != nil {
		return err
	}
	_, _ = r.pool.Exec(ctx, `UPDATE pipeline_node_run SET status=CASE WHEN status='running' THEN 'failed' ELSE status END,completed_at=CASE WHEN status='running' THEN now() ELSE completed_at END,detail=jsonb_set(COALESCE(detail,'{}'),'{runner,error}',jsonb_build_object('code',$2),true) WHERE run_id=$1 AND status IN ('running','blocked')`, runID, code)
	return errCode(code, "run failed")
}

func (r *Runner) WireEvents(bus *events.Bus) {
	bus.Subscribe(EventCRUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok || e.WorkspaceID == "" {
			return
		}
		ws, err := parseUUID(e.WorkspaceID)
		crID := stringValue(payload["cr_id"])
		if err == nil && crID != "" {
			if err := r.Reconcile(context.Background(), ws, crID); err != nil {
				slog.Warn("runner CR wake failed", "cr_id", crID, "error", err)
			}
		}
	})
	for _, eventType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed} {
		bus.Subscribe(eventType, func(e events.Event) {
			if e.TaskID != "" {
				if err := r.reconcileFromTaskEvent(context.Background(), e.TaskID); err != nil {
					slog.Warn("runner task wake failed", "task_id", e.TaskID, "error", err)
				}
			}
		})
	}
}

// WakeGrant is the POST-COMMIT wake installed into ApprovalService's
// SetGrantAckCommittedHandler. The ACK handler already scoped the IDs to this
// workspace and committed delivered_at; the callback only wakes, then
// Reconcile re-reads approval_record as authority. Its error is logged by the
// ACK handler and never turns a committed ACK into a 5xx (TD-BL-12 / SDD §3.2).
// AIFIRST: CR-2026-052 TASK-05 — signature extended to GrantAckEvent + error.
func (r *Runner) WakeGrant(ctx context.Context, ev GrantAckEvent) error {
	ws, err := parseUUID(ev.WorkspaceID)
	if err != nil {
		return fmt.Errorf("invalid workspace id: %w", err)
	}
	return r.Reconcile(ctx, ws, ev.CrID)
}

// ValidateGrantAck is the PRE-COMMIT FR-10 callback installed into
// ApprovalService's SetGrantAckHandler. It is a pure validation with ZERO
// external side effects — it takes no advisory lock, writes no
// pipeline_run/pipeline_node_run, enqueues no task, and does not call
// Reconcile. An error here rolls back the whole ACK batch and yields HTTP 5xx
// (FR-10 canonical callback, TD-BL-12). It validates the event fields and
// confirms the CR exists in the authenticated workspace via a workspace-scoped
// read-only lookup (reuses GetCrShellIssueInWorkspaceForShare without the
// FOR SHARE lock — the ACK transaction's own resolveContinuationTarget already
// holds the authority locks; this is a defense-in-depth re-check only).
// AIFIRST: CR-2026-052 TASK-05.
func (r *Runner) ValidateGrantAck(ctx context.Context, ev GrantAckEvent) error {
	if ev.WorkspaceID == "" {
		return fmt.Errorf("grant ack event: empty workspace id")
	}
	if ev.CrID == "" {
		return fmt.Errorf("grant ack event: empty cr id")
	}
	if _, err := parseUUID(ev.WorkspaceID); err != nil {
		return fmt.Errorf("grant ack event: invalid workspace id: %w", err)
	}
	if _, err := parseUUID(ev.RecordID); err != nil {
		return fmt.Errorf("grant ack event: invalid record id: %w", err)
	}
	if !approvalStages[ev.Stage] {
		return fmt.Errorf("grant ack event: invalid stage %q", ev.Stage)
	}
	if ev.Decision != "approve" && ev.Decision != "reject" {
		return fmt.Errorf("grant ack event: invalid decision %q", ev.Decision)
	}
	// Read-only workspace-scoped CR existence check on a SEPARATE connection
	// with NO lock: resolveContinuationTarget in the ACK transaction already
	// holds the authoritative FOR SHARE; taking it again here would self-deadlock.
	// This is defense-in-depth against a malformed/foreign event reaching the
	// Runner — it reads the committed cr row only.
	ws, _ := parseUUID(ev.WorkspaceID)
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cr WHERE workspace_id=$1::uuid AND cr_id=$2)`, ws, ev.CrID).Scan(&exists); err != nil {
		return fmt.Errorf("grant ack event: cr %s lookup failed: %w", ev.CrID, err)
	}
	if !exists {
		return fmt.Errorf("grant ack event: cr %s not found in workspace", ev.CrID)
	}
	return nil
}

func (r *Runner) StartupScan(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
		SELECT workspace_id,cr_id FROM pipeline_run
		WHERE pipeline_id=$1 AND status IN ('running','waiting_approval') AND execution_context->>'runner'=$2`, PipelineIDs.ArchitectureDesign, runnerSchema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ws pgtype.UUID
		var crID string
		if err := rows.Scan(&ws, &crID); err != nil {
			return err
		}
		if err := r.Reconcile(ctx, ws, crID); err != nil {
			slog.Warn("runner startup reconcile failed", "cr_id", crID, "error", err)
		}
	}
	return rows.Err()
}

func (r *Runner) HandleStartArchitecture(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("X-Actor-Source") != "task_token" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": RunnerErrRequiresAgentRoute})
		return
	}
	workspaceID, werr := parseUUID(req.Header.Get("X-Workspace-ID"))
	if pathWorkspace := chi.URLParam(req, "workspaceID"); pathWorkspace == "" || pathWorkspace != workspaceID.String() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": RunnerErrAuthorityMismatch})
		return
	}
	agentID, aerr := parseUUID(req.Header.Get("X-Agent-ID"))
	taskID, terr := parseUUID(req.Header.Get("X-Task-ID"))
	userID, uerr := parseUUID(req.Header.Get("X-User-ID"))
	if werr != nil || aerr != nil || terr != nil || uerr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid task-token binding"})
		return
	}
	var body struct {
		PipelineID string `json:"pipeline_id"`
		CrID       string `json:"cr_id"`
		Inputs     struct {
			TechContext string `json:"tech_context"`
		} `json:"inputs"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxTechContextBytes+4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if body.PipelineID != PipelineIDs.ArchitectureDesign {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": RunnerErrUnsupportedPipeline})
		return
	}
	runID, changed, err := r.StartArchitecture(req.Context(), StartArchitectureInput{WorkspaceID: workspaceID, AgentID: agentID, TaskID: taskID, UserID: userID, CrID: body.CrID, TechContext: body.Inputs.TechContext})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID.String(), "changed": changed})
}

func (r *Runner) reconcileFromTaskEvent(ctx context.Context, taskID string) error {
	id, err := parseUUID(taskID)
	if err != nil {
		return nil
	}
	var ws pgtype.UUID
	var crID string
	err = r.pool.QueryRow(ctx, `
		SELECT r.workspace_id,r.cr_id FROM agent_task_queue t
		JOIN pipeline_node_run n ON n.id=t.pipeline_node_run_id JOIN pipeline_run r ON r.id=n.run_id
		WHERE t.id=$1 AND r.execution_context->>'runner'=$2`, id, runnerSchema).Scan(&ws, &crID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.Reconcile(ctx, ws, crID)
}

func errCode(code, message string) error { return fmt.Errorf("%s: %s", code, message) }
func stringValue(v any) string           { s, _ := v.(string); return s }
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseUUID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if strings.TrimSpace(s) == "" {
		return id, errors.New("UUID is empty")
	}
	if err := id.Scan(s); err != nil || !id.Valid {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID %q", s)
	}
	return id, nil
}

func mustParseUUID(s string) pgtype.UUID {
	id, err := parseUUID(s)
	if err != nil {
		panic(err) // registry UUIDs were validated at startup
	}
	return id
}
