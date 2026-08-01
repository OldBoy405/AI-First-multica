package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CR-2026-004: shared Team Agent queue capacity governance.
//
// Priority tiers on agent_task_queue.priority: 0-4 are the ordinary
// issue-priority tiers (see priorityToInt); 100 is the governance preempt
// tier for workspace owners/admins. Keep new tiers below 100 unless they are
// meant to outrank governance preemption.
const (
	// DefaultTeamAgentQueueLimit caps a project's shared queue (queued +
	// dispatched tasks) when the project has no explicit setting.
	DefaultTeamAgentQueueLimit = 50
	// PreemptPriorityOwnerAdmin is stamped on tasks enqueued by workspace
	// owners/admins so the claim ORDER BY (priority DESC, created_at ASC)
	// serves them before ordinary members' tasks.
	PreemptPriorityOwnerAdmin = 100
	// ProjectSettingTeamAgentQueueLimit is the project.settings key holding
	// the per-project capacity override. Exported so the project-update
	// handler validates against the same key the guard reads.
	ProjectSettingTeamAgentQueueLimit = "team_agent_queue_limit"
)

// ErrProjectQueueFull is returned when a plain member's enqueue would push a
// project's shared queue past its capacity. Handlers map it to HTTP 429.
type ErrProjectQueueFull struct {
	Depth int64
	Limit int64
}

func (e *ErrProjectQueueFull) Error() string {
	return fmt.Sprintf("project queue full: %d/%d", e.Depth, e.Limit)
}

// guardProjectQueueCapacity enforces the shared Team Agent queue limit for
// user-driven enqueues (the CreateAgentTask and CreateQuickCreateTask paths
// only — see the INSERT-point table in the CR-2026-004 SDD; deferred, retry,
// autopilot and chat inserts stay unguarded).
//
// Returns a non-zero priority override for workspace owners/admins, who both
// bypass the capacity check and preempt the FIFO. Enqueues with no resolved
// human originator (agent-to-agent chains, autopilot-created issues) pass
// unguarded: an agent chain must not deadlock on a full queue it cannot see.
//
// Capacity is a soft limit: count-then-insert without a lock may briefly
// overshoot under concurrency (accepted, PRD NFR-2). Lookup failures fail
// open on capacity and closed on preemption — a transient DB hiccup can
// neither take enqueue down nor mint preempt priority.
func (s *TaskService) guardProjectQueueCapacity(ctx context.Context, projectID, workspaceID, callerID pgtype.UUID) (int32, error) {
	if !projectID.Valid || !callerID.Valid {
		return 0, nil
	}
	member, err := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      callerID,
		WorkspaceID: workspaceID,
	})
	if err == nil && (member.Role == "owner" || member.Role == "admin") {
		return PreemptPriorityOwnerAdmin, nil
	}
	limit := s.projectQueueLimit(ctx, projectID)
	depth, err := s.Queries.CountProjectPendingTasks(ctx, projectID)
	if err != nil {
		slog.Warn("project queue capacity check skipped",
			"project_id", util.UUIDToString(projectID), "error", err)
		return 0, nil
	}
	if depth >= limit {
		return 0, &ErrProjectQueueFull{Depth: depth, Limit: limit}
	}
	return 0, nil
}

// ProjectQueueStatus reports the live shared-queue depth and effective limit
// for a project — the read side of the capacity gate, consumed by the
// frontend queue indicator (CR-2026-004 FR-5).
func (s *TaskService) ProjectQueueStatus(ctx context.Context, projectID pgtype.UUID) (depth, limit int64, err error) {
	depth, err = s.Queries.CountProjectPendingTasks(ctx, projectID)
	if err != nil {
		return 0, 0, err
	}
	return depth, s.projectQueueLimit(ctx, projectID), nil
}

// projectQueueLimit reads the per-project override, falling back to the
// default on any missing/invalid value — config must never block enqueue.
func (s *TaskService) projectQueueLimit(ctx context.Context, projectID pgtype.UUID) int64 {
	raw, err := s.Queries.GetProjectSettings(ctx, projectID)
	if err != nil || len(raw) == 0 {
		return DefaultTeamAgentQueueLimit
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return DefaultTeamAgentQueueLimit
	}
	// JSON numbers decode as float64; any other type is an invalid setting.
	if v, ok := settings[ProjectSettingTeamAgentQueueLimit].(float64); ok && v >= 1 {
		return int64(v)
	}
	return DefaultTeamAgentQueueLimit
}
