package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ProjectSettingDiscussionCoordinatorID is the project.settings key holding
// the agent bound as the project's Discussion Coordinator (CR-2026-012).
// Mentioning this agent in the project Discussion container activates it;
// an unset key keeps the CR-2026-009 red line (no agent on Discussion).
// Stored as the agent UUID string, mirroring ProjectSettingTeamAgentID.
const ProjectSettingDiscussionCoordinatorID = "discussion_coordinator_agent_id"

// ProjectDiscussionCoordinatorID resolves the agent bound as the project's
// Discussion Coordinator. A zero-value UUID means "unconfigured" — the
// Discussion container then stays agent-free (CR-2026-009 red line). The
// read is fail-safe in every branch: a missing project, malformed settings
// bag, or invalid UUID string all degrade to "unconfigured" rather than
// erroring, so a broken setting can never wedge the comment-trigger path.
func (s *TaskService) ProjectDiscussionCoordinatorID(ctx context.Context, projectID pgtype.UUID) pgtype.UUID {
	coordinatorID, _ := s.ProjectDiscussionAgentIDs(ctx, projectID)
	return coordinatorID
}

// ProjectDiscussionAgentIDs resolves the Discussion Coordinator binding and
// the Team Agent binding from ONE project.settings read — the comment-trigger
// hot path needs both (activation filter + routing target, SDD §8) and the
// settings bag is a single JSONB column. Both values follow the same
// fail-safe contract as ProjectDiscussionCoordinatorID: zero UUID =
// unconfigured.
func (s *TaskService) ProjectDiscussionAgentIDs(ctx context.Context, projectID pgtype.UUID) (coordinatorID, teamAgentID pgtype.UUID) {
	if !projectID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	raw, err := s.Queries.GetProjectSettings(ctx, projectID)
	if err != nil || len(raw) == 0 {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	return uuidSettingFromBag(settings, ProjectSettingDiscussionCoordinatorID),
		uuidSettingFromBag(settings, ProjectSettingTeamAgentID)
}

// uuidSettingFromBag pulls one agent-UUID setting out of an already-parsed
// settings bag. Missing key, wrong JSON type, or invalid UUID string all
// degrade to the zero UUID.
func uuidSettingFromBag(settings map[string]any, key string) pgtype.UUID {
	v, ok := settings[key].(string)
	if !ok {
		return pgtype.UUID{}
	}
	id, err := util.ParseUUID(v)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}

// discussionCoordinatorIDFromSettings parses the coordinator binding out of
// the raw project.settings JSONB bag. Kept separate from the DB read so the
// parsing branches are unit-testable without a pool (same shape as the
// handler-side projectTeamAgentID tests).
func discussionCoordinatorIDFromSettings(raw []byte) pgtype.UUID {
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return pgtype.UUID{}
	}
	return uuidSettingFromBag(settings, ProjectSettingDiscussionCoordinatorID)
}

// RouteDiscussionToTeamAgent posts the Discussion Coordinator's routing
// comment onto the project Team Agent chat container and enqueues the Team
// Agent run against it (CR-2026-012 DD-5) — the executed work and the
// coordination record then live on the Team Agent pane, not in Discussion.
//
// originatorID is the EXPLICITLY resolved activation human (TSUG-001): the
// route comment is created here in the service, so it carries no
// source_task_id and resolveOriginatorFromTriggerComment cannot pierce it —
// the handler walks the route comment's parent chain (the coordinator's
// completion comment hangs under the human's @-mention) and passes the human
// down. This path stamps the override straight onto the task row so the
// capacity guard and preempt logic judge the right person. An invalid
// originatorID keeps the existing a2a unguarded semantics
// (task_queue_capacity.go:49-57).
//
// Failure contract mirrors SendProjectChatMessage: *ErrProjectQueueFull from
// either guard surfaces verbatim (the handler maps it to the DD-6 system
// comment); any other enqueue failure compensating-deletes the route comment
// before returning so no ghost routing message lingers on the chat pane.
func (s *TaskService) RouteDiscussionToTeamAgent(ctx context.Context, chatIssue db.Issue, teamAgentID, coordinatorID, originatorID pgtype.UUID, content string) (db.Comment, db.AgentTaskQueue, error) {
	if _, gerr := s.guardProjectQueueCapacity(ctx, chatIssue.ProjectID, chatIssue.WorkspaceID, originatorID); gerr != nil {
		return db.Comment{}, db.AgentTaskQueue{}, gerr
	}

	comment, err := s.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     chatIssue.ID,
		WorkspaceID: chatIssue.WorkspaceID,
		AuthorType:  "agent",
		AuthorID:    coordinatorID,
		Content:     content,
		Type:        "comment",
	})
	if err != nil {
		return db.Comment{}, db.AgentTaskQueue{}, fmt.Errorf("create discussion route comment: %w", err)
	}

	// The inner capacity guard can still trip on a concurrent enqueue (same
	// race documented on SendProjectChatMessage); *ErrProjectQueueFull is
	// returned verbatim.
	task, err := s.enqueueMentionTaskWithCommentPlanAndOriginator(ctx, chatIssue, teamAgentID, comment.ID, nil, false, pgtype.UUID{}, false, "", false, originatorID, pgtype.UUID{}, originatorID)
	if err != nil {
		if _, derr := s.Queries.DeleteComment(ctx, db.DeleteCommentParams{
			ID: comment.ID, WorkspaceID: chatIssue.WorkspaceID,
		}); derr != nil {
			slog.Error("discussion route compensating delete failed",
				"comment_id", util.UUIDToString(comment.ID),
				"issue_id", util.UUIDToString(chatIssue.ID),
				"enqueue_error", err, "delete_error", derr)
		}
		return db.Comment{}, db.AgentTaskQueue{}, err
	}

	// Broadcast only after a successful enqueue — a rolled-back route must
	// never flash a ghost message on the Team Agent pane (same ordering as
	// SendProjectChatMessage).
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(chatIssue.WorkspaceID),
		ActorType:   "agent",
		ActorID:     util.UUIDToString(coordinatorID),
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          util.UUIDToString(comment.ID),
				"issue_id":    util.UUIDToString(comment.IssueID),
				"author_type": comment.AuthorType,
				"author_id":   util.UUIDToString(comment.AuthorID),
				"content":     comment.Content,
				"type":        comment.Type,
				"created_at":  comment.CreatedAt.Time.Format("2006-01-02T15:04:05Z"),
			},
			"issue_title":  chatIssue.Title,
			"issue_status": chatIssue.Status,
		},
	})
	return commentFromCreateRow(comment), task, nil
}

// commentFromCreateRow adapts sqlc's CreateCommentRow to the Comment model.
// IssueRevision exists only on the row (activity-touch feedback), never on
// the stored comment, so it is dropped here.
func commentFromCreateRow(row db.CreateCommentRow) db.Comment {
	return db.Comment{
		ID: row.ID, IssueID: row.IssueID, AuthorType: row.AuthorType,
		AuthorID: row.AuthorID, Content: row.Content, Type: row.Type,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ParentID: row.ParentID,
		WorkspaceID: row.WorkspaceID, ResolvedAt: row.ResolvedAt,
		ResolvedByType: row.ResolvedByType, ResolvedByID: row.ResolvedByID,
		SourceTaskID: row.SourceTaskID, QuickActionID: row.QuickActionID,
		ViaPluginID: row.ViaPluginID, Revision: row.Revision,
	}
}

// PostDiscussionSystemNotice writes an auditable system comment authored by
// the Discussion Coordinator onto the Discussion container (DD-6: a full
// queue must leave a visible, attributable record — the activation path is
// fire-and-forget, so a silent log is not an honest failure surface).
// Fire-and-forget like every createAgentComment caller.
func (s *TaskService) PostDiscussionSystemNotice(ctx context.Context, discussionIssue db.Issue, coordinatorID pgtype.UUID, content string) {
	s.createAgentComment(ctx, discussionIssue.ID, coordinatorID, content, "system", pgtype.UUID{}, pgtype.UUID{})
}

// DiscussionQueueFullNotice renders the DD-6 queue-full notice for the
// Discussion container. Server-side system comments are not localized — the
// frontend renders them verbatim; the numbers are substituted here so the
// message is self-contained (the chat.dc.queue_full_notice locale anchor is
// the frontend-side counterpart for UI surfaces).
func DiscussionQueueFullNotice(full *ErrProjectQueueFull) string {
	return fmt.Sprintf("The project Team Agent queue is full (%d/%d). The task was not enqueued — please retry once a slot frees up.", full.Depth, full.Limit)
}
