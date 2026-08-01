package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// projectChatIssueTitle is the fixed title of every project's hidden Team Agent
// chat container issue. Never shown in any issue-listing surface (all of them
// exclude origin_type='project_chat'), so it only needs to be stable, not nice.
const projectChatIssueTitle = "Team Agent Chat"

// EnsureProjectChatIssue returns the hidden container issue that anchors a
// project's Team Agent group chat (CR-2026-006), creating it on first use.
//
// The message stream reuses the existing comment/timeline/websocket stack by
// hanging every chat message off this one issue. It is stamped
// origin_type='project_chat' so all issue-listing queries filter it out.
//
// Concurrency: the fast path is a plain read. On a miss we open a tx, take an
// advisory lock keyed on (workspace, project) to serialize concurrent
// first-opens, re-check inside the lock, and only then create. The partial
// unique index issue_project_chat_unique is the belt-and-suspenders backstop
// if two creators ever slip past the lock.
func (s *IssueService) EnsureProjectChatIssue(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID) (db.Issue, error) {
	if !workspaceID.Valid || !projectID.Valid {
		return db.Issue{}, fmt.Errorf("ensure project chat issue: workspace and project required")
	}

	existing, err := s.Queries.GetProjectChatIssue(ctx, db.GetProjectChatIssueParams{
		ProjectID:   projectID,
		WorkspaceID: workspaceID,
	})
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("lookup project chat issue: %w", err)
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.Issue{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Serialize concurrent first-opens on the same project. Released on commit.
	lockKey := strings.Join([]string{"project-chat", util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
	if err := qtx.LockIssueDuplicateKey(ctx, lockKey); err != nil {
		return db.Issue{}, fmt.Errorf("lock project chat key: %w", err)
	}

	// Re-check under the lock — another opener may have created it while we
	// waited on the advisory lock.
	if existing, err := qtx.GetProjectChatIssue(ctx, db.GetProjectChatIssueParams{
		ProjectID:   projectID,
		WorkspaceID: workspaceID,
	}); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("recheck project chat issue: %w", err)
	}

	number, err := qtx.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return db.Issue{}, fmt.Errorf("increment issue counter: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, workspaceID, "todo")
	if err != nil {
		return db.Issue{}, fmt.Errorf("next top position: %w", err)
	}

	issue, err := qtx.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
		WorkspaceID: workspaceID,
		Title:       projectChatIssueTitle,
		Description: pgtype.Text{},
		Status:      "todo",
		// CR-2026-006 (TSUG-002): 'medium' maps to priority tier 2 via
		// priorityToInt. Every group-chat task enqueues off this container
		// issue and inherits its priority, so pinning the container at medium
		// keeps project chat on par with 1:1 chat (also fixed at 2) — otherwise
		// 'none'(=0) would let 1:1 chat perpetually jump ahead of group chat on
		// the same agent. Owner/admin preemption (tier 100) still outranks it.
		Priority:     "medium",
		AssigneeType: pgtype.Text{},
		AssigneeID:   pgtype.UUID{},
		// The container has no meaningful author; attribute it to the member
		// who first opened the chat. It is never surfaced, so this only
		// affects the (also hidden) issue's own creator record.
		CreatorType:   "member",
		CreatorID:     callerID,
		ParentIssueID: pgtype.UUID{},
		Position:      position,
		StartDate:     pgtype.Date{},
		DueDate:       pgtype.Date{},
		Number:        number,
		ProjectID:     projectID,
		OriginType:    pgtype.Text{String: "project_chat", Valid: true},
		OriginID:      pgtype.UUID{},
		Stage:         pgtype.Int4{},
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create project chat issue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Issue{}, fmt.Errorf("commit project chat issue: %w", err)
	}
	return issue, nil
}

// SendProjectChatMessage posts a member's message into the project's Team Agent
// group chat (a comment on the hidden container issue) and enqueues a run for
// the bound agent (CR-2026-006).
//
// The capacity guard is front-loaded so a full queue is rejected before any
// comment is persisted — no orphan message for the capacity failure case,
// which dominates. If the enqueue still fails (a concurrent enqueue slipping
// past the front-load check into the guard *inside* EnqueueTaskForMention, or a
// DB error), the comment is compensated by physical delete (comment has no
// soft-delete) so a failed send leaves nothing behind.
//
// Error contract for the handler:
//   - *ErrProjectQueueFull  -> 429 (queue full, try later). Covers both the
//     front-load rejection and the inner-guard race (TSUG-001).
//   - any other error       -> 502 (send failed, retryable).
func (s *TaskService) SendProjectChatMessage(ctx context.Context, issue db.Issue, agentID, callerID pgtype.UUID, content string) (db.Comment, db.AgentTaskQueue, error) {
	if _, gerr := s.guardProjectQueueCapacity(ctx, issue.ProjectID, issue.WorkspaceID, callerID); gerr != nil {
		return db.Comment{}, db.AgentTaskQueue{}, gerr
	}

	comment, err := s.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "member",
		AuthorID:    callerID,
		Content:     content,
		Type:        "comment",
	})
	if err != nil {
		return db.Comment{}, db.AgentTaskQueue{}, fmt.Errorf("create chat comment: %w", err)
	}

	// EnqueueTaskForMention re-runs the guard internally; a concurrent send can
	// pass our front-load check and then trip that inner guard, returning
	// *ErrProjectQueueFull here (TSUG-001). We return it verbatim so the handler
	// maps it to 429 rather than the generic 502.
	task, err := s.EnqueueTaskForMention(ctx, issue, agentID, comment.ID)
	if err != nil {
		if derr := s.Queries.DeleteComment(ctx, db.DeleteCommentParams{
			ID: comment.ID, WorkspaceID: issue.WorkspaceID,
		}); derr != nil {
			// Double fault: the compensating delete also failed, leaving a
			// visible-but-unanswered message. Log ids only, never the body
			// (audit redaction). Still surface the original enqueue error.
			slog.Error("project chat compensating delete failed",
				"comment_id", util.UUIDToString(comment.ID),
				"issue_id", util.UUIDToString(issue.ID),
				"enqueue_error", err, "delete_error", derr)
		}
		return db.Comment{}, db.AgentTaskQueue{}, err
	}

	// Broadcast only after a successful enqueue (SDD §4.3 step 6): a send that
	// fails and rolls back must never first flash a ghost message to peers.
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(callerID),
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
			"issue_title":  issue.Title,
			"issue_status": issue.Status,
		},
	})
	return comment, task, nil
}
