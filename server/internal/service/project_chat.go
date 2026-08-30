package service

import (
	"context"
	"errors"
	"fmt"
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
		"project_chat", projectChatIssueTitle, "project-chat-session",
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

// SendProjectChatMessageResult is the committed outcome of a Team Agent send
// (messages or merge-forward): the session id (echoed from the request for
// messages, the ensured active session for merge-forward) and the ids the
// single send transaction wrote.
type SendProjectChatMessageResult struct {
	SessionID string
	IssueID   string
	CommentID string
	TaskID    string
}

// ErrAttachmentAlreadyBound: a draft attachment id in the send request was
// already bound to another target (handler: 409 attachment_already_bound).
var ErrAttachmentAlreadyBound = errors.New("attachment already bound")

// SendProjectChatMessage posts a member's message into the project's Team
// Agent group chat and enqueues one run for the bound agent (CR-2026-056,
// SDD §4.5 / FR-16). The container bind, the comment, the enqueue and the
// draft-attachment binding commit in ONE transaction — any failure rolls the
// whole send back, leaving no container, no comment, no task and no bound
// attachment behind (BLOCK-003).
//
// Error contract for the handler:
//   - *ErrPresenterRequired -> 403 (single-writer presenter control)
//   - *ErrProjectQueueFull -> 429
//   - ErrTeamAgentNotConfigured -> 409
//   - ErrChatSessionNotFound -> 404 / ErrChatSessionClosedOrChanged -> 409
//   - ErrInvalidModelOrThinkingLevel -> 400 (pre-transaction §4.3)
//   - ErrAttachmentAlreadyBound -> 409
//   - any other error -> 502 (send failed, retryable)
func (s *IssueService) SendProjectChatMessage(ctx context.Context, workspaceID, projectID, sessionID, callerID pgtype.UUID, content string, attachmentIDs []pgtype.UUID) (*SendProjectChatMessageResult, error) {
	return s.sendProjectChatCore(ctx, workspaceID, projectID, sessionID, callerID, content, attachmentIDs)
}

// MergeForwardDiscussion posts a member's multi-select of Discussion messages
// as ONE merged message on the project Team Agent chat and enqueues exactly
// one Team Agent run for it (CR-2026-012 DD-7/DD-8 + CR-2026-056 §4.12): one
// confirmation = one comment + one task. The request carries no session_id —
// the active session is ensured first (created on first use, base_* snapshot
// included); a concurrent rebind that closes it surfaces as 409
// chat_session_closed_or_changed from the send kernel's lock-internal check.
//
// comments must already be validated as belonging to the project's Discussion
// container and are rendered in created_at ascending order inside the merged
// markdown. registerCR appends the requirement-register instruction block —
// pure comment text, the server keeps zero CR write paths (DD-8).
func (s *IssueService) MergeForwardDiscussion(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID, comments []db.Comment, registerCR bool) (*SendProjectChatMessageResult, error) {
	sorted := append([]db.Comment(nil), comments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Time.Before(sorted[j].CreatedAt.Time) })
	content := buildMergedForwardContent(ctx, s.TaskService, sorted, registerCR)

	view, err := s.EnsureProjectChatSession(ctx, workspaceID, projectID, callerID)
	if err != nil {
		return nil, err
	}
	if view.SessionID == "" {
		return nil, ErrTeamAgentNotConfigured
	}
	sessionID, err := util.ParseUUID(view.SessionID)
	if err != nil {
		return nil, fmt.Errorf("parse ensured session id: %w", err)
	}
	return s.sendProjectChatCore(ctx, workspaceID, projectID, sessionID, callerID, content, nil)
}

// sendProjectChatCore is the single Team Agent send kernel behind messages
// and merge-forward (SDD §4.5). Pre-transaction: presenter guard, queue
// capacity guard, Resolve and §4.3 catalog validation — a failure here leaves
// no issue, no comment and no binding behind. Then ONE transaction:
//
//	① project-chat-session|{ws}|{project} advisory
//	② session row FOR UPDATE + binding CAS (active + agent_id == the
//	   lock-internal team_agent_id re-read; never a pre-lock snapshot)
//	③ BindProjectChatContainer (idempotent; creates the container on the
//	   first send)
//	④ CreateComment -> enqueue (with the chat_config snapshot merged into
//	   task.context before the queue INSERT) -> BindUnboundDraftAttachments
//	   (ORDER BY id)
//
// commit, then broadcast. The lock order is fixed by §4.14 and must not be
// reordered. Any in-transaction error rolls back the whole send (BLOCK-003):
// no compensating deletes, no orphan container, no ghost comment, no queued
// task, no bound attachment.
func (s *IssueService) sendProjectChatCore(ctx context.Context, workspaceID, projectID, sessionID, callerID pgtype.UUID, content string, attachmentIDs []pgtype.UUID) (*SendProjectChatMessageResult, error) {
	if !workspaceID.Valid || !projectID.Valid || !sessionID.Valid || !callerID.Valid {
		return nil, fmt.Errorf("send project chat message: workspace, project, session and caller required")
	}

	// ---- pre-transaction guards + resolve + §4.3 (SDD §4.5) ----
	// The presenter guard only needs the project identity, so a synthetic
	// issue keeps it working before a container ever exists.
	synthetic := db.Issue{ProjectID: projectID, WorkspaceID: workspaceID}
	suppressPreempt, perr := s.TaskService.guardProjectChatPresenter(ctx, synthetic, callerID)
	if perr != nil {
		return nil, perr
	}
	if _, gerr := s.TaskService.guardProjectQueueCapacity(ctx, projectID, workspaceID, callerID); gerr != nil {
		return nil, gerr
	}

	session, err := s.Queries.GetProjectChatSessionByID(ctx, db.GetProjectChatSessionByIDParams{
		ID: sessionID, WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChatSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load chat session: %w", err)
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID: session.AgentID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("load team agent: %w", err)
	}
	resolved := ResolveChatConfig(
		session.BaseModel, session.ModelOverride, agent.Model,
		session.BaseThinkingLevel, session.ThinkingLevelOverride, agent.ThinkingLevel,
	)
	provider := ""
	if agent.RuntimeID.Valid {
		if rt, rerr := s.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
			ID: agent.RuntimeID, WorkspaceID: workspaceID,
		}); rerr == nil {
			provider = rt.Provider
		}
	}
	if provider == "" {
		return nil, ErrInvalidModelOrThinkingLevel
	}
	if s.ChatCatalog == nil {
		return nil, ErrInvalidModelOrThinkingLevel
	}
	catalog, err := LoadChatCatalogForConfig(ctx, s.Queries, s.ChatCatalog, agent)
	if err != nil {
		return nil, err
	}
	if err := ValidateResolvedChatConfig(resolved.Model, resolved.ThinkingLevel, provider, catalog); err != nil {
		return nil, err
	}
	// Composio overlay pre-computed before the transaction: it can do network
	// I/O and must not run with a DB transaction open (same contract as
	// SendDirectChatMessage).
	overlay := s.TaskService.buildRuntimeMCPOverlay(ctx, callerID, agent)

	// ---- one transaction (BLOCK-003) ----
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin send tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// ① project-level advisory shared with GET/PATCH/rebind/forwarding Ensure.
	if err := qtx.LockIssueDuplicateKey(ctx, projectChatSessionAdvisoryKey(workspaceID, projectID)); err != nil {
		return nil, fmt.Errorf("lock project chat session key: %w", err)
	}

	// Binding re-read under the advisory (§4.7.1: never trust a pre-lock
	// binding).
	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: projectID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("reload project under session lock: %w", err)
	}
	teamAgentID := teamAgentIDFromSettings(project.Settings)
	if !teamAgentID.Valid {
		return nil, ErrTeamAgentNotConfigured
	}

	// ② session row FOR UPDATE + binding CAS.
	locked, err := qtx.LockProjectChatSessionByID(ctx, db.LockProjectChatSessionByIDParams{
		ID: sessionID, WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChatSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock session: %w", err)
	}
	if locked.Status != "active" || locked.AgentID != teamAgentID {
		return nil, ErrChatSessionClosedOrChanged
	}

	// ③ Bind the container inside THIS transaction — the send never commits a
	// container ahead of the enqueue (SDD §4.5).
	issue, err := s.BindProjectChatContainer(ctx, qtx, tx, sessionID, workspaceID, projectID, teamAgentID, callerID)
	if err != nil {
		return nil, err
	}

	comment, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: workspaceID,
		AuthorType:  "member",
		AuthorID:    callerID,
		Content:     content,
		Type:        "comment",
	})
	if err != nil {
		return nil, fmt.Errorf("create chat comment: %w", err)
	}

	// enqueueMentionTaskWithCommentPlanTx re-runs the capacity guard
	// internally; a concurrent send can pass the front-load check and then
	// trip that inner guard, returning *ErrProjectQueueFull (TSUG-001). It is
	// returned verbatim so the handler maps it to 429 rather than the generic
	// 502. suppressPreempt keeps DD-6: owner/admin sends keep their capacity
	// exemption but lose the queue-jump priority while a presenter other than
	// themselves is active. The chat_config snapshot is the SAME resolved
	// output the §4.3 validation consumed.
	task, err := s.TaskService.enqueueMentionTaskWithCommentPlanTx(ctx, qtx, issue, teamAgentID, comment.ID, nil, false, pgtype.UUID{}, false, "", suppressPreempt, callerID, pgtype.UUID{}, pgtype.UUID{}, &resolved, &overlay)
	if err != nil {
		return nil, err
	}

	// ④ draft attachments: lock ORDER BY id, then bind inside the same tx.
	// Duplicate request ids are collapsed first so a repeat id cannot fake an
	// attachment_already_bound 409.
	if len(attachmentIDs) > 0 {
		seen := make(map[pgtype.UUID]struct{}, len(attachmentIDs))
		deduped := attachmentIDs[:0:0]
		for _, id := range attachmentIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			deduped = append(deduped, id)
		}
		if _, err := qtx.LockUnboundDraftAttachments(ctx, db.LockUnboundDraftAttachmentsParams{
			WorkspaceID: workspaceID, AttachmentIds: deduped,
		}); err != nil {
			return nil, fmt.Errorf("lock draft attachments: %w", err)
		}
		bound, err := qtx.BindUnboundDraftAttachments(ctx, db.BindUnboundDraftAttachmentsParams{
			IssueID:       issue.ID,
			CommentID:     comment.ID,
			TaskID:        task.ID,
			WorkspaceID:   workspaceID,
			AttachmentIds: deduped,
			UploaderType:  "member",
			UploaderID:    callerID,
		})
		if err != nil {
			return nil, fmt.Errorf("bind draft attachments: %w", err)
		}
		if len(bound) != len(deduped) {
			return nil, ErrAttachmentAlreadyBound
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit send: %w", err)
	}

	// Broadcast only after a successful commit (SDD §4.5 step 6): a send that
	// fails and rolls back must never first flash a ghost message to peers.
	commentRow := commentFromCreateRow(comment)
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(workspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(callerID),
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          util.UUIDToString(commentRow.ID),
				"issue_id":    util.UUIDToString(commentRow.IssueID),
				"author_type": commentRow.AuthorType,
				"author_id":   util.UUIDToString(commentRow.AuthorID),
				"content":     commentRow.Content,
				"type":        commentRow.Type,
				"created_at":  commentRow.CreatedAt.Time.Format("2006-01-02T15:04:05Z"),
			},
			"issue_title":  issue.Title,
			"issue_status": issue.Status,
		},
	})
	// The queued task event must leave in the same post-commit ordering as
	// the non-tx enqueue path (broadcast before wakeup).
	s.TaskService.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.TaskService.NotifyTaskEnqueued(ctx, task)

	return &SendProjectChatMessageResult{
		SessionID: util.UUIDToString(sessionID),
		IssueID:   util.UUIDToString(issue.ID),
		CommentID: util.UUIDToString(commentRow.ID),
		TaskID:    util.UUIDToString(task.ID),
	}, nil
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
