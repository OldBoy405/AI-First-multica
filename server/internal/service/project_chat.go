package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
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

	issue, err := createContainerIssueInTx(ctx, qtx, tx, workspaceID, projectID, callerID, originType, title, pgtype.UUID{})
	if err != nil {
		return db.Issue{}, err
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
	return s.sendProjectChatCore(ctx, issue, agentID, callerID, content)
}

// MergeForwardDiscussion posts a member's multi-select of Discussion messages
// as ONE merged message on the project Team Agent chat and enqueues exactly
// one Team Agent run for it (CR-2026-012 DD-7/DD-8): one confirmation = one
// comment + one task. comments must already be validated as belonging to the
// project's Discussion container and are rendered in created_at ascending
// order inside the merged markdown. registerCR appends the
// requirement-register instruction block — pure comment text, the server
// keeps zero CR write paths (DD-8).
//
// Reuses the exact SendProjectChatMessage kernel (presenter guard → capacity
// guard → create → enqueue → compensating delete → broadcast-after-success),
// so CR-2026-010 presenter control and CR-2026-004 capacity governance apply
// to merged forwards without a second implementation. Deliberately NOT
// coalesced_comment_ids: that mechanism assumes same-issue delivery plans,
// which cross-container forwarding is not (DD-7).
//
// ponytail: the 50-comment selection cap lives at the handler; compressing
// genuinely longer discussions into a summary is an upgrade path, not a
// v1 feature.
func (s *TaskService) MergeForwardDiscussion(ctx context.Context, issue db.Issue, agentID, callerID pgtype.UUID, comments []db.Comment, registerCR bool) (db.Comment, db.AgentTaskQueue, error) {
	// Render in created_at ascending order regardless of caller ordering — the
	// history list and the "earliest message" trigger quote both depend on it.
	sorted := append([]db.Comment(nil), comments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Time.Before(sorted[j].CreatedAt.Time) })
	content := buildMergedForwardContent(ctx, s, sorted, registerCR)
	return s.sendProjectChatCore(ctx, issue, agentID, callerID, content)
}

// sendProjectChatCore is the shared guard → create → enqueue → compensate →
// broadcast sequence behind SendProjectChatMessage and MergeForwardDiscussion.
func (s *TaskService) sendProjectChatCore(ctx context.Context, issue db.Issue, agentID, callerID pgtype.UUID, content string) (db.Comment, db.AgentTaskQueue, error) {
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
	task, err := s.enqueueMentionTaskWithCommentPlan(ctx, issue, agentID, comment.ID, nil, false, pgtype.UUID{}, false, "", suppressPreempt, callerID, pgtype.UUID{})
	if err != nil {
		if _, derr := s.Queries.DeleteComment(ctx, db.DeleteCommentParams{
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
	return commentFromCreateRow(comment), task, nil
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

// mergedForwardRegisterCRBlock is the DD-8 instruction block appended when
// the user checks "upgrade to CR" in the merge preview. It is comment text —
// visible and auditable in the chat record — while the actual CR registration
// stays with the executing agent via the requirement-register Skill (the
// server keeps zero CR write paths).
const mergedForwardRegisterCRBlock = "## 升级为 CR\n请按 requirement-register Skill 将上述讨论注册为 CR（目标 workspace 的 knowledge-base 仓），完成后在本会话回报 CR-ID。"

// buildMergedForwardContent renders the selected Discussion messages into the
// merged markdown posted on the Team Agent chat (SDD §4.3): a trigger-message
// blockquote quoting the earliest message in full, a chronological history
// list of ALL selected messages, and optionally the register-CR instruction
// block. Comments must arrive sorted by created_at ascending; headings stay
// in fixed English because this is persisted agent-facing content — the
// chat.merged_forward.* locale keys cover the frontend preview chrome.
func buildMergedForwardContent(ctx context.Context, s *TaskService, comments []db.Comment, registerCR bool) string {
	var b strings.Builder

	b.WriteString("## Trigger message\n")
	first := comments[0]
	for _, line := range strings.Split(strings.TrimRight(first.Content, "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n## Conversation history (")
	b.WriteString(strconv.Itoa(len(comments)))
	b.WriteString(" messages)\n")
	for _, c := range comments {
		b.WriteString("- [")
		b.WriteString(commentAuthorDisplayName(ctx, s, c))
		b.WriteString(" ")
		b.WriteString(c.CreatedAt.Time.UTC().Format(time.RFC3339))
		b.WriteString("] ")
		// Flatten newlines so each message stays one list item.
		b.WriteString(strings.Join(strings.Fields(c.Content), " "))
		b.WriteString("\n")
	}

	if registerCR {
		b.WriteString("\n")
		b.WriteString(mergedForwardRegisterCRBlock)
		b.WriteString("\n")
	}
	return b.String()
}

// commentAuthorDisplayName resolves a comment author's display name for the
// merged-forward rendering. Lookup failures degrade to the author kind —
// attribution is best-effort, the message bodies carry the substance.
func commentAuthorDisplayName(ctx context.Context, s *TaskService, c db.Comment) string {
	if !c.AuthorID.Valid {
		return c.AuthorType
	}
	switch c.AuthorType {
	case "member":
		if u, err := s.Queries.GetUser(ctx, c.AuthorID); err == nil {
			return u.Name
		}
	case "agent":
		if a, err := s.Queries.GetAgent(ctx, c.AuthorID); err == nil {
			return a.Name
		}
	}
	return c.AuthorType
}
