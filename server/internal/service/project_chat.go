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

// projectDiscussionIssueTitle is the fixed title of every project's hidden
// Discussion container issue (CR-2026-009). Same visibility rule as the chat
// container: never shown in any issue-listing surface.
const projectDiscussionIssueTitle = "Discussion"

// EnsureProjectChatIssue returns the hidden container issue that anchors a
// project's Team Agent group chat (CR-2026-006), creating it on first use.
//
// The message stream reuses the existing comment/timeline/websocket stack by
// hanging every chat message off this one issue. It is stamped
// origin_type='project_chat' so all issue-listing queries filter it out.
func (s *IssueService) EnsureProjectChatIssue(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID) (db.Issue, error) {
	return s.ensureContainerIssue(ctx, workspaceID, projectID, callerID,
		"project_chat", projectChatIssueTitle, "project-chat",
		func(q *db.Queries, ctx context.Context, projectID, workspaceID pgtype.UUID) (db.Issue, error) {
			return q.GetProjectChatIssue(ctx, db.GetProjectChatIssueParams{ProjectID: projectID, WorkspaceID: workspaceID})
		})
}

// EnsureProjectDiscussionIssue returns the hidden container issue that anchors
// a project's Discussion tab — a pure-human, agent-free multi-member chat
// (CR-2026-009), creating it on first use.
//
// Structurally identical to EnsureProjectChatIssue: every Discussion message
// is a comment hung off this one issue, stamped origin_type='project_discussion'
// so all issue-listing queries filter it out. The only behavioral difference
// from the chat container lives downstream, in computeCommentAgentTriggers'
// origin-type short-circuit (CR-2026-009 red line: no agent is ever enqueued
// off a Discussion comment).
func (s *IssueService) EnsureProjectDiscussionIssue(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID) (db.Issue, error) {
	return s.ensureContainerIssue(ctx, workspaceID, projectID, callerID,
		"project_discussion", projectDiscussionIssueTitle, "project-discussion",
		func(q *db.Queries, ctx context.Context, projectID, workspaceID pgtype.UUID) (db.Issue, error) {
			return q.GetProjectDiscussionIssue(ctx, db.GetProjectDiscussionIssueParams{ProjectID: projectID, WorkspaceID: workspaceID})
		})
}

// containerIssueGetter is the shape shared by GetProjectChatIssue and
// GetProjectDiscussionIssue — sqlc gives each origin type its own generated
// method (and its own Params struct) rather than a parameterized one, so
// ensureContainerIssue takes this as a closure over *db.Queries to stay
// generated-code-friendly while sharing the tx/lock/create plumbing below. The
// caller passes *db.Queries explicitly (rather than binding it in the closure)
// so ensureContainerIssue can run the same getter against both the plain
// s.Queries fast-path read and the tx-scoped qtx recheck under the lock.
type containerIssueGetter func(q *db.Queries, ctx context.Context, projectID, workspaceID pgtype.UUID) (db.Issue, error)

// ensureContainerIssue is the shared lazy-creation path behind
// EnsureProjectChatIssue and EnsureProjectDiscussionIssue.
//
// Concurrency: the fast path is a plain read. On a miss we open a tx, take an
// advisory lock keyed on (kind, workspace, project) to serialize concurrent
// first-opens, re-check inside the lock, and only then create. The container's
// partial unique index (issue_project_chat_unique / issue_project_discussion_unique)
// is the belt-and-suspenders backstop if two creators ever slip past the lock.
func (s *IssueService) ensureContainerIssue(
	ctx context.Context,
	workspaceID, projectID, callerID pgtype.UUID,
	originType, title, lockKeyPrefix string,
	get containerIssueGetter,
) (db.Issue, error) {
	if !workspaceID.Valid || !projectID.Valid {
		return db.Issue{}, fmt.Errorf("ensure %s issue: workspace and project required", originType)
	}

	existing, err := get(s.Queries, ctx, projectID, workspaceID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("lookup %s issue: %w", originType, err)
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.Issue{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Serialize concurrent first-opens on the same project. Released on commit.
	lockKey := strings.Join([]string{lockKeyPrefix, util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
	if err := qtx.LockIssueDuplicateKey(ctx, lockKey); err != nil {
		return db.Issue{}, fmt.Errorf("lock %s key: %w", originType, err)
	}

	// Re-check under the lock — another opener may have created it while we
	// waited on the advisory lock.
	if existing, err := get(qtx, ctx, projectID, workspaceID); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("recheck %s issue: %w", originType, err)
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
		Title:       title,
		Description: pgtype.Text{},
		Status:      "todo",
		// CR-2026-006 (TSUG-002): 'medium' maps to priority tier 2 via
		// priorityToInt. Every group-chat task enqueues off the chat container
		// issue and inherits its priority, so pinning it at medium keeps
		// project chat on par with 1:1 chat (also fixed at 2) — otherwise
		// 'none'(=0) would let 1:1 chat perpetually jump ahead of group chat on
		// the same agent. Owner/admin preemption (tier 100) still outranks it.
		// The Discussion container never enqueues anything (CR-2026-009 red
		// line), so this value is inert for it — kept identical for one fewer
		// branch, not because it means anything there.
		Priority:     "medium",
		AssigneeType: pgtype.Text{},
		AssigneeID:   pgtype.UUID{},
		// The container has no meaningful author; attribute it to the member
		// who first opened it. It is never surfaced, so this only affects the
		// (also hidden) issue's own creator record.
		CreatorType:   "member",
		CreatorID:     callerID,
		ParentIssueID: pgtype.UUID{},
		Position:      position,
		StartDate:     pgtype.Date{},
		DueDate:       pgtype.Date{},
		Number:        number,
		ProjectID:     projectID,
		OriginType:    pgtype.Text{String: originType, Valid: true},
		OriginID:      pgtype.UUID{},
		Stage:         pgtype.Int4{},
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create %s issue: %w", originType, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Issue{}, fmt.Errorf("commit %s issue: %w", originType, err)
	}
	return issue, nil
}

// SendProjectChatMessage posts a member's message into the project's Team Agent
// group chat (a comment on the hidden container issue) and enqueues a run for
// the bound agent (CR-2026-006).
//
// The presenter and capacity guards are front-loaded so a rejected sender
// never gets an orphan comment — nothing is persisted for either rejection.
// If the enqueue still fails after a comment is created (a concurrent enqueue
// slipping past the front-load capacity check into the guard *inside*
// enqueueMentionTaskWithCommentPlan, or a DB error), the comment is
// compensated by physical delete (comment has no soft-delete) so a failed
// send leaves nothing behind.
//
// Error contract for the handler:
//   - *ErrPresenterRequired -> 403 (an active presenter holds single-writer
//     control and the caller is neither the presenter nor owner/admin).
//   - *ErrProjectQueueFull  -> 429 (queue full, try later). Covers both the
//     front-load rejection and the inner-guard race (TSUG-001).
//   - any other error       -> 502 (send failed, retryable).
//
// The presenter guard runs before the capacity guard (SDD §4.3/DD-6): a
// rejected sender must see "presenter required", never "queue full", and
// nothing is persisted (no comment, no task) for either rejection.
func (s *TaskService) SendProjectChatMessage(ctx context.Context, issue db.Issue, agentID, callerID pgtype.UUID, content string) (db.Comment, db.AgentTaskQueue, error) {
	suppressPreempt, perr := s.guardProjectChatPresenter(ctx, issue, callerID)
	if perr != nil {
		return db.Comment{}, db.AgentTaskQueue{}, perr
	}

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

	// enqueueMentionTaskWithCommentPlan re-runs the capacity guard internally;
	// a concurrent send can pass our front-load check and then trip that inner
	// guard, returning *ErrProjectQueueFull here (TSUG-001). We return it
	// verbatim so the handler maps it to 429 rather than the generic 502.
	//
	// Called directly (bypassing the EnqueueTaskForMention wrapper) so
	// suppressPreempt can reach the priority computation: DD-6 requires
	// owner/admin sends to keep their capacity exemption but lose the
	// queue-jump priority while a presenter other than themselves is active.
	task, err := s.enqueueMentionTaskWithCommentPlan(ctx, issue, agentID, comment.ID, nil, false, pgtype.UUID{}, false, "", suppressPreempt)
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

// ErrPresenterRequired is returned when a project has an active presenter and
// the caller is neither that presenter nor a workspace owner/admin (SDD
// §4.3, DD-6: the presenter is the group chat's single writer while active).
type ErrPresenterRequired struct {
	PresenterUserID string
}

func (e *ErrPresenterRequired) Error() string {
	return "an active presenter holds single-writer control of this chat"
}

// guardProjectChatPresenter enforces CR-2026-010 single-writer control ahead
// of the capacity guard (SDD §4.3):
//
//	active  = GetPresenterState(project).presenter
//	allowed = active != nil ? (caller == active.user_id || role ∈ {owner,admin})
//	                        : (role ∈ {owner,admin})
//
// With no active presenter the default state is a strict subset of the prior
// CR-A behavior: owner/admin send, everyone else is rejected — there is no
// separate "presenter feature disabled" branch because GetPresenterState
// already returns presenter=nil for a project with no grant rows, which is
// exactly this default state. With an active presenter: the presenter always
// sends; owner/admin may still send (their capacity exemption is untouched)
// but must not preempt the presenter's queue position, so the returned bool
// tells the caller to suppress the queue-jump priority; anyone else is
// rejected with the presenter's id so the frontend can show who holds it.
func (s *TaskService) guardProjectChatPresenter(ctx context.Context, issue db.Issue, callerID pgtype.UUID) (suppressPreemptPriority bool, err error) {
	if !issue.ProjectID.Valid {
		return false, nil
	}
	project := db.Project{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID}
	state, err := s.GetPresenterState(ctx, project, callerID)
	if err != nil {
		return false, fmt.Errorf("load presenter state for chat guard: %w", err)
	}

	member, merr := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: callerID, WorkspaceID: issue.WorkspaceID,
	})
	callerIsOwnerOrAdmin := merr == nil && isOwnerOrAdmin(member.Role)

	if state.Presenter == nil {
		if callerIsOwnerOrAdmin {
			return false, nil
		}
		return false, &ErrPresenterRequired{}
	}
	if state.Presenter.UserID == callerID {
		return false, nil
	}
	if callerIsOwnerOrAdmin {
		return true, nil
	}
	return false, &ErrPresenterRequired{PresenterUserID: util.UUIDToString(state.Presenter.UserID)}
}
