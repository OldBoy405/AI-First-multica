package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// DiscussionSessionAdvisoryPrefix is the project-level advisory key prefix for
// the shared Discussion session (CR-2026-059 SDD §4.1/§4.2/§4.5, decision
// D-9). Deliberately independent from the Team Agent prefix
// (ProjectChatSessionAdvisoryPrefix) — the two session classes have unrelated
// lifecycles and must not serialize each other.
const DiscussionSessionAdvisoryPrefix = "project-discussion-session"

// Discussion session kind value (migration 482 CHECK enum).
const chatSessionKindProjectShared = "project_shared"

// Discussion idempotency scope values (migration 487 CHECK enum).
const (
	discussionIdempotencyScopeMessage      = "discussion_message"
	discussionIdempotencyScopeMergeForward = "merge_forward_messages"
)

func discussionSessionAdvisoryKey(workspaceID, projectID pgtype.UUID) string {
	return strings.Join([]string{DiscussionSessionAdvisoryPrefix, util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
}

// DiscussionSessionDeps is the dependency surface of the shared Discussion
// session service functions (CR-2026-059 SDD §4). The handler assembly layer
// wires one shared instance; the service package never imports the handler
// package (layering: handler -> service -> db).
type DiscussionSessionDeps struct {
	Queries   *db.Queries  // all DB access: WithTx, session/member/idempotency/attachment queries, agent/runtime reads
	TxStarter TxStarter    // Begin() transaction entry; handlers pass TaskService.TxStarter (single implementation)
	TaskSvc   *TaskService // mergeChatConfigContext combination, SnapshotAgentDefaults, ChatCatalog port (§4.4 L1/L2 single authority)
}

// ProjectDiscussionSessionView is the resolved display payload for
// GET /api/projects/{id}/discussion (SDD §3.1/§4.1).
type ProjectDiscussionSessionView struct {
	SessionID           string
	LegacyIssueID       *string
	CoordinatorAgentID  string
	Model               string
	ThinkingLevel       string
	ModelSource         string
	ThinkingLevelSource string
}

// EnsureProjectDiscussionSession resolves (lazily creating on first use) the
// project's unique ACTIVE shared Discussion session (SDD §4.1, FR-1/FR-3/
// FR-8/FR-16). It NEVER creates the legacy container issue: legacy_issue_id is
// a read-only lookup for replay. Concurrent first-opens collapse via the
// project advisory + the 485 partial unique index (insert-conflict reselect).
func EnsureProjectDiscussionSession(ctx context.Context, deps DiscussionSessionDeps, wsID, projectID, callerID uuid.UUID) (ProjectDiscussionSessionView, error) {
	if deps.Queries == nil || deps.TxStarter == nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("ensure project discussion session: deps incomplete")
	}
	ws := pgtype.UUID{Bytes: wsID, Valid: true}
	project := pgtype.UUID{Bytes: projectID, Valid: true}
	caller := pgtype.UUID{Bytes: callerID, Valid: true}

	// Legacy container read (SDD §3.1): read-only, never created, never
	// backfilled. Safe outside the transaction.
	var legacyIssueID *string
	if legacy, lerr := deps.Queries.GetProjectDiscussionIssue(ctx, db.GetProjectDiscussionIssueParams{
		ProjectID: project, WorkspaceID: ws,
	}); lerr == nil {
		id := util.UUIDToString(legacy.ID)
		legacyIssueID = &id
	} else if !errors.Is(lerr, pgx.ErrNoRows) {
		return ProjectDiscussionSessionView{}, fmt.Errorf("load legacy discussion issue: %w", lerr)
	}

	tx, err := deps.TxStarter.Begin(ctx)
	if err != nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := deps.Queries.WithTx(tx)

	if err := qtx.LockIssueDuplicateKey(ctx, discussionSessionAdvisoryKey(ws, project)); err != nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("lock project discussion session key: %w", err)
	}
	projectRow, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: project, WorkspaceID: ws,
	})
	if err != nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("reload project under session lock: %w", err)
	}
	coordinatorUUID := discussionCoordinatorIDFromSettings(projectRow.Settings)
	routable, err := routableDiscussionCoordinator(ctx, qtx, ws, coordinatorUUID)
	if err != nil {
		return ProjectDiscussionSessionView{}, err
	}

	session, err := qtx.GetActiveProjectSharedSession(ctx, db.GetActiveProjectSharedSessionParams{
		WorkspaceID: ws, ProjectID: project,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var agentID pgtype.UUID
		var baseModel, baseThinking pgtype.Text
		if routable != nil {
			agentID = routable.ID
			baseModel, baseThinking = SnapshotAgentDefaults(*routable)
		}
		session, err = qtx.InsertProjectSharedSession(ctx, db.InsertProjectSharedSessionParams{
			WorkspaceID:       ws,
			AgentID:           agentID,
			CreatorID:         caller,
			ProjectID:         project,
			BaseModel:         baseModel,
			BaseThinkingLevel: baseThinking,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Concurrent first-open: the partial unique index collapses the
				// race; the loser reselects under the advisory.
				session, err = qtx.GetActiveProjectSharedSession(ctx, db.GetActiveProjectSharedSessionParams{
					WorkspaceID: ws, ProjectID: project,
				})
			}
			if err != nil {
				return ProjectDiscussionSessionView{}, fmt.Errorf("create project discussion session: %w", err)
			}
		}
	} else if err != nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("lookup active project discussion session: %w", err)
	}

	// GET-side self-heal (SDD §4.5, FR-26 race rule 4): the session's
	// agent_id projection must match the routable resolution under the lock.
	expectedAgent := pgtype.UUID{}
	if routable != nil {
		expectedAgent = routable.ID
	}
	if session.AgentID != expectedAgent {
		if _, err := qtx.SetChatSessionAgentID(ctx, db.SetChatSessionAgentIDParams{
			ID: session.ID, AgentID: expectedAgent,
		}); err != nil {
			return ProjectDiscussionSessionView{}, fmt.Errorf("self-heal discussion coordinator projection: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ProjectDiscussionSessionView{}, fmt.Errorf("commit session ensure: %w", err)
	}

	resolved := ResolveChatConfig(
		session.BaseModel, session.ModelOverride, pgtype.Text{},
		session.BaseThinkingLevel, session.ThinkingLevelOverride, pgtype.Text{},
	)
	return ProjectDiscussionSessionView{
		SessionID:           util.UUIDToString(session.ID),
		LegacyIssueID:       legacyIssueID,
		CoordinatorAgentID:  discussionCoordinatorSettingRaw(projectRow.Settings),
		Model:               resolved.Model,
		ModelSource:         string(resolved.ModelSource),
		ThinkingLevel:       resolved.ThinkingLevel,
		ThinkingLevelSource: string(resolved.ThinkingLevelSource),
	}, nil
}

// routableDiscussionCoordinator resolves the currently routable Coordinator
// for the configured settings UUID (SDD §4.3): the agent row exists in this
// workspace and its readiness verdict is not Blocked (archived / no runtime /
// unusable CLI all make it unroutable; an offline machine stays routable and
// work queues for it, matching AgentReadiness semantics). nil = unroutable;
// the settings UUID itself is never cleared by the read side.
func routableDiscussionCoordinator(ctx context.Context, q *db.Queries, wsID, configured pgtype.UUID) (*db.Agent, error) {
	if !configured.Valid {
		return nil, nil
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID: configured, WorkspaceID: wsID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // hard-deleted: settings keep the UUID, projection is NULL
	}
	if err != nil {
		return nil, fmt.Errorf("load discussion coordinator: %w", err)
	}
	verdict, err := AgentReadiness(ctx, q, agent)
	if err != nil {
		return nil, err
	}
	if verdict.Blocked() {
		return nil, nil
	}
	return &agent, nil
}

// discussionCoordinatorSettingRaw reads the raw coordinator UUID string out of
// the project settings bag (SDD §3.1: the GET response returns the settings
// value verbatim — stale/invalid UUIDs included; empty when unconfigured).
func discussionCoordinatorSettingRaw(settings []byte) string {
	if len(settings) == 0 {
		return ""
	}
	var bag map[string]any
	if err := json.Unmarshal(settings, &bag); err != nil {
		return ""
	}
	if v, ok := bag[ProjectSettingDiscussionCoordinatorID].(string); ok {
		return v
	}
	return ""
}

// DiscussionSendInput is the handler-validated body of a shared Discussion
// send (SDD §3.4).
type DiscussionSendInput struct {
	Content            string
	AttachmentIDs      []uuid.UUID
	CoordinatorRequest string // "none" | "mention" | "analyze" | "summarize"
}

// DiscussionSendResult is the committed outcome of a shared Discussion send:
// the message id and — for coordinator turns only — the enqueued task id.
type DiscussionSendResult struct {
	MessageID uuid.UUID  `json:"message_id"`
	TaskID    *uuid.UUID `json:"task_id"` // nil for ordinary messages
}

// DiscussionSendError is the service-level error contract the handler maps to
// the PRD error-code matrix (SDD §3.4/FR-18). A replay is not an error: it is
// signalled by the returned error being nil and the result carrying the
// first-attempt outcome.
type DiscussionSendError struct {
	Code string
}

func (e *DiscussionSendError) Error() string { return "discussion send: " + e.Code }

// Discussion send error codes (handler mapping in TASK-03).
const (
	discussionErrSessionNotFound          = "chat_session_not_found"
	discussionErrSessionClosedOrChanged   = "chat_session_closed_or_changed"
	discussionErrCoordinatorNotConfigured = "discussion_coordinator_not_configured"
	discussionErrCoordinatorUnavailable   = "discussion_coordinator_unavailable"
	discussionErrInvalidModel             = "invalid_model_or_thinking_level"
	discussionErrAttachmentAlreadyBound   = "attachment_already_bound"
	discussionErrIdempotencyKeyReused     = "idempotency_key_reused"
	// discussionErrInvocationNotAllowed reuses the dispatch invocation model
	// (ReasonInvocationNotAllowed) — no new permission enum is created.
	discussionErrInvocationNotAllowed = "invocation_not_allowed"
)

// SendDiscussionMessage writes one member message into the shared Discussion
// session, optionally enqueuing one Coordinator task, in ONE transaction
// (SDD §4.2, FR-10/FR-11/FR-12/FR-15/FR-17/FR-24): idempotency reservation,
// session lock, message, coordinator task, attachment binding and the
// idempotency finalize commit or roll back together — any failure leaves zero
// residue (the idempotency-key conflict branch carries its own replay
// semantics). The caller (handler) performs post-commit publishing.
//
// Lock order is FIXED (B-AUTH-2, never reorder):
// LockSubscriberWrites -> project-discussion-session advisory -> session row
// lock -> idempotency key / attachment row locks. revokeAndRemoveMember takes
// the same LockSubscriberWrites first, so send and revoke serialize.
func SendDiscussionMessage(ctx context.Context, deps DiscussionSessionDeps, wsID, callerID, sessionID uuid.UUID, input DiscussionSendInput, idempotencyKey string) (DiscussionSendResult, *DiscussionSendError) {
	if deps.Queries == nil || deps.TxStarter == nil || deps.TaskSvc == nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	ws := pgtype.UUID{Bytes: wsID, Valid: true}
	caller := pgtype.UUID{Bytes: callerID, Valid: true}
	sid := pgtype.UUID{Bytes: sessionID, Valid: true}

	fingerprint, err := discussionSendFingerprint(input)
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	// Pre-transaction: the Composio overlay may perform network I/O and must
	// not run with a DB transaction open (same contract as
	// SendDirectChatMessage / sendProjectChatCore). The authoritative trigger
	// decision is recomputed under the lock inside the transaction.
	var overlay *runtimeMCPOverlayData
	if pre, perr := deps.Queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{
		ID: sid, WorkspaceID: ws,
	}); perr == nil && pre.Kind == chatSessionKindProjectShared && pre.ProjectID.Valid {
		if proj, gerr := deps.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
			ID: pre.ProjectID, WorkspaceID: ws,
		}); gerr == nil {
			configured := discussionCoordinatorIDFromSettings(proj.Settings)
			if routable, rerr := routableDiscussionCoordinator(ctx, deps.Queries, ws, configured); rerr == nil && routable != nil {
				ov := deps.TaskSvc.buildRuntimeMCPOverlay(ctx, caller, *routable)
				overlay = &ov
			}
		}
	}

	tx, err := deps.TxStarter.Begin(ctx)
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	defer tx.Rollback(ctx)
	qtx := deps.Queries.WithTx(tx)

	// First lock: same (workspace, user) advisory revokeAndRemoveMember takes
	// first (B-AUTH-2) — membership revoke and send serialize, no deadlock.
	if err := qtx.LockSubscriberWrites(ctx, db.LockSubscriberWritesParams{
		WorkspaceID: ws, UserID: caller,
	}); err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	// In-transaction membership re-check: a revoke that committed before we
	// took the lock surfaces here; roll back with zero residue (AC-28 race).
	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: caller, WorkspaceID: ws,
	}); err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	// Plain pre-load pins the project for the advisory key; the locked re-read
	// below is authoritative.
	sessionPre, err := qtx.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{
		ID: sid, WorkspaceID: ws,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if sessionPre.Kind != chatSessionKindProjectShared || !sessionPre.ProjectID.Valid {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	if err := qtx.LockIssueDuplicateKey(ctx, discussionSessionAdvisoryKey(ws, sessionPre.ProjectID)); err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	locked, err := qtx.LockChatSessionInWorkspace(ctx, db.LockChatSessionInWorkspaceParams{
		ID: sid, WorkspaceID: ws,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if locked.Kind != chatSessionKindProjectShared || !locked.ProjectID.Valid ||
		locked.ProjectID != sessionPre.ProjectID {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if locked.Status != "active" {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionClosedOrChanged}
	}

	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: locked.ProjectID, WorkspaceID: ws,
	})
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	configured := discussionCoordinatorIDFromSettings(project.Settings)
	routable, err := routableDiscussionCoordinator(ctx, qtx, ws, configured)
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	trigger := detectCoordinatorTrigger(input.Content, input.CoordinatorRequest, uuid.UUID(configured.Bytes), routable)
	switch trigger.Reason {
	case "not_configured":
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrCoordinatorNotConfigured}
	case "unavailable":
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrCoordinatorUnavailable}
	}
	if trigger.NeedTask && !deps.TaskSvc.CanMemberInvokeAgent(ctx, *routable, caller, ws) {
		// Zero writes: the reservation below has not run yet.
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrInvocationNotAllowed}
	}

	// Idempotency reservation (SDD §4.6). Same fingerprint + stored body =>
	// replay; same fingerprint + NULL body => take over; different fingerprint
	// => 409 (zero writes).
	replayBody, err := reserveDiscussionIdempotency(ctx, qtx, ws, caller, sid, idempotencyKey, fingerprint)
	if err != nil {
		var reused *DiscussionSendError
		if errors.As(err, &reused) {
			return DiscussionSendResult{}, reused
		}
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if replayBody != nil {
		var replayed DiscussionSendResult
		if uerr := json.Unmarshal(replayBody, &replayed); uerr != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
		}
		// Read-only replay: roll the transaction back (nothing was written).
		return replayed, nil
	}

	// Coordinator task preflight (SDD §4.2, AC-10 enqueue arm): catalog I/O
	// while the transaction holds only the session row lock — same shape as
	// SendDirectChatMessage. A 400 here rolls back message/task/reservation.
	var taskID pgtype.UUID
	if trigger.NeedTask {
		resolved := ResolveChatConfig(
			locked.BaseModel, locked.ModelOverride, pgtype.Text{},
			locked.BaseThinkingLevel, locked.ThinkingLevelOverride, pgtype.Text{},
		)
		provider := ""
		if routable.RuntimeID.Valid {
			if rt, rerr := qtx.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
				ID: routable.RuntimeID, WorkspaceID: ws,
			}); rerr == nil {
				provider = rt.Provider
			}
		}
		if provider == "" || deps.TaskSvc.ChatCatalog == nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrInvalidModel}
		}
		catalog, cerr := LoadChatCatalogForConfig(ctx, qtx, deps.TaskSvc.ChatCatalog, *routable)
		if cerr != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrInvalidModel}
		}
		if verr := ValidateResolvedChatConfig(resolved.Model, resolved.ThinkingLevel, provider, catalog); verr != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrInvalidModel}
		}
		// Snapshot = the SAME resolved output the validation consumed; the
		// single merge seam is shared with the Private Ask path.
		chatConfig := mergeChatConfigContext(nil, resolved.Model, resolved.ThinkingLevel)

		attr := attribution.DirectHumanRun(caller, attribution.EvidenceChat, sid)
		attrSource, _, attrEvidenceKind, attrEvidenceRef := attributionCreateParams(attr)

		task, terr := qtx.CreateChatTask(ctx, db.CreateChatTaskParams{
			ID:                   dbid.NewV7(),
			AgentID:              routable.ID,
			RuntimeID:            routable.RuntimeID,
			Priority:             2, // medium priority, matches the chat enqueue paths
			ChatSessionID:        sid,
			InitiatorUserID:      caller,
			OriginatorUserID:     attr.UserID,
			AccountableUserID:    attr.AccountableUserID,
			ForceFreshSession:    pgtype.Bool{Bool: false, Valid: true},
			RuntimeMcpOverlay:    overlayJSON(overlay),
			RuntimeConnectedApps: overlayConnectedApps(overlay),
			OriginatorSource:     attrSource,
			TriggerEvidenceKind:  attrEvidenceKind,
			TriggerEvidenceRefID: attrEvidenceRef,
			Context:              chatConfig,
		})
		if terr != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
		}
		taskID = task.ID
	}

	msg, err := qtx.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID:            dbid.NewV7(),
		ChatSessionID: sid,
		Role:          "user",
		Content:       input.Content,
		TaskID:        taskID,
		MessageKind:   pgtype.Text{String: protocol.ChatMessageKindMessage, Valid: true},
		// M486 author columns: shared user messages attribute their sender.
		AuthorType: pgtype.Text{String: "member", Valid: true},
		AuthorID:   caller,
	})
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	if taskID.Valid {
		// The task's input batch owns this message the instant it exists
		// (same pattern as the direct chat send).
		if _, err := qtx.SetChatTaskInputOwnerSelf(ctx, taskID); err != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
		}
	}

	// Attachments: lock then bind inside the same transaction (SDD §4.2,
	// FR-15). Duplicate ids are rejected upstream (handler 400), so any
	// shortfall here is a genuine already-bound row.
	if len(input.AttachmentIDs) > 0 {
		ids := make([]pgtype.UUID, 0, len(input.AttachmentIDs))
		for _, id := range input.AttachmentIDs {
			ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
		}
		if _, err := qtx.LockUnboundDraftAttachments(ctx, db.LockUnboundDraftAttachmentsParams{
			WorkspaceID: ws, AttachmentIds: ids,
		}); err != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrAttachmentAlreadyBound}
		}
		bound, err := qtx.BindDraftAttachmentsToChatMessage(ctx, db.BindDraftAttachmentsToChatMessageParams{
			ChatSessionID: sid,
			ChatMessageID: msg.ID,
			TaskID:        taskID,
			WorkspaceID:   ws,
			AttachmentIds: ids,
			// Uploader gate is caller-derived from the authenticated identity
			// (requireWorkspaceMember), never from the request body.
			UploaderType: "member",
			UploaderID:   caller,
		})
		if err != nil {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrAttachmentAlreadyBound}
		}
		if len(bound) != len(ids) {
			return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrAttachmentAlreadyBound}
		}
	}

	if err := qtx.TouchChatSession(ctx, sid); err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	result := DiscussionSendResult{MessageID: uuid.UUID(msg.ID.Bytes)}
	if taskID.Valid {
		taskUUID := uuid.UUID(taskID.Bytes)
		result.TaskID = &taskUUID
	}
	body, err := json.Marshal(result)
	if err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	updated, err := qtx.FinalizeChatIdempotency(ctx, db.FinalizeChatIdempotencyParams{
		WorkspaceID:    ws,
		UserID:         caller,
		ScopeType:      discussionIdempotencyScopeMessage,
		ScopeID:        sid,
		Key:            idempotencyKey,
		ResponseStatus: 201,
		ResponseBody:   body,
	})
	if err != nil || updated != 1 {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}

	if err := tx.Commit(ctx); err != nil {
		return DiscussionSendResult{}, &DiscussionSendError{Code: discussionErrSessionNotFound}
	}
	return result, nil
}

// overlayJSON / overlayConnectedApps fold a possibly-nil pre-transaction MCP
// overlay into the task params (the overlay is best-effort pre-tx context).
func overlayJSON(o *runtimeMCPOverlayData) []byte {
	if o == nil {
		return nil
	}
	return o.Overlay
}

func overlayConnectedApps(o *runtimeMCPOverlayData) []byte {
	if o == nil {
		return nil
	}
	return o.ConnectedApps
}

// reserveDiscussionIdempotency inserts the reservation and resolves the
// conflict semantics (SDD §4.6). Returns a non-nil replay body when the stored
// first response must be replayed; a *DiscussionSendError for a fingerprint
// conflict. On takeover (winner committed with NULL body) the caller proceeds
// and finalizes over the winner row.
func reserveDiscussionIdempotency(ctx context.Context, qtx *db.Queries, ws, caller, sid pgtype.UUID, key, fingerprint string) ([]byte, error) {
	_, err := qtx.InsertChatIdempotencyReservation(ctx, db.InsertChatIdempotencyReservationParams{
		WorkspaceID: ws, UserID: caller,
		ScopeType: discussionIdempotencyScopeMessage, ScopeID: sid,
		Key: key, Fingerprint: fingerprint,
	})
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	winner, werr := qtx.GetChatIdempotencyByKey(ctx, db.GetChatIdempotencyByKeyParams{
		WorkspaceID: ws, UserID: caller,
		ScopeType: discussionIdempotencyScopeMessage, ScopeID: sid, Key: key,
	})
	if werr != nil {
		if errors.Is(werr, pgx.ErrNoRows) {
			// Winner rolled back; the key is free again — treat as reserved.
			return nil, nil
		}
		return nil, werr
	}
	if winner.Fingerprint != fingerprint {
		return nil, &DiscussionSendError{Code: discussionErrIdempotencyKeyReused}
	}
	if winner.ResponseBody == nil {
		// Interrupted previous execution (defensive for this scope, where
		// reservation and finalize share one transaction): take over and
		// finalize the winner row.
		return nil, nil
	}
	return winner.ResponseBody, nil
}

// discussionSendFingerprint is the canonical fingerprint (SDD §4.6): trimmed
// content + attachment ids stably sorted by canonical UUID string (duplicates
// are rejected upstream and never reach the fingerprint) + coordinator_request.
// Canonical JSON field order is fixed by the struct below.
func discussionSendFingerprint(input DiscussionSendInput) (string, error) {
	ids := make([]string, 0, len(input.AttachmentIDs))
	for _, id := range input.AttachmentIDs {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	canonical, err := json.Marshal(struct {
		Content            string   `json:"content"`
		AttachmentIDs      []string `json:"attachment_ids"`
		CoordinatorRequest string   `json:"coordinator_request"`
	}{
		Content:            strings.TrimSpace(input.Content),
		AttachmentIDs:      ids,
		CoordinatorRequest: input.CoordinatorRequest,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// triggerDecision is detectCoordinatorTrigger's output (SDD §4.3).
type triggerDecision struct {
	NeedTask bool
	Reason   string // "none" | "not_configured" | "unavailable"
}

// detectCoordinatorTrigger decides whether a shared Discussion send must
// enqueue the Coordinator (SDD §4.3, FR-11). The configured UUID comes from
// project settings; routable is the currently-routable Coordinator row (nil =
// unroutable; non-nil implies its id equals configured). analyze/summarize
// take priority over mention derivation; a mention of the CONFIGURED identity
// activates it even when that agent is archived or hard-deleted (the mention
// link still carries the original UUID) — that maps to unavailable, not to an
// ordinary message. Mentions of other agents stay ordinary messages.
func detectCoordinatorTrigger(content, coordinatorRequest string, configured uuid.UUID, routable *db.Agent) triggerDecision {
	if coordinatorRequest == "analyze" || coordinatorRequest == "summarize" {
		if routable != nil {
			return triggerDecision{NeedTask: true, Reason: "none"}
		}
		if configured != uuid.Nil {
			return triggerDecision{Reason: "unavailable"}
		}
		return triggerDecision{Reason: "not_configured"}
	}
	if configured == uuid.Nil {
		return triggerDecision{Reason: "none"}
	}
	for _, m := range util.ParseMentions(content) {
		if m.Type != "agent" {
			continue
		}
		id, err := uuid.Parse(m.ID)
		if err != nil || id != configured {
			continue
		}
		if routable != nil {
			return triggerDecision{NeedTask: true, Reason: "none"}
		}
		return triggerDecision{Reason: "unavailable"}
	}
	return triggerDecision{Reason: "none"}
}

// CanMemberInvokeAgent mirrors handler.canInvokeAgent for the member-only
// shared Discussion send path (SDD §4.2): the Coordinator task is enqueued
// inside the send transaction, so the service needs the same invocation gate
// without importing the handler package. Member principal semantics only —
// the shared send surface admits members exclusively.
func (s *TaskService) CanMemberInvokeAgent(ctx context.Context, agent db.Agent, memberID, workspaceID pgtype.UUID) bool {
	userID := util.UUIDToString(memberID)
	if userID != "" && util.UUIDToString(agent.OwnerID) == userID {
		return true
	}
	if agent.PermissionMode != "public_to" {
		return false
	}
	targets, err := s.Queries.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return false
	}
	isWorkspaceMember := false
	if userID != "" {
		if _, err := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID: memberID, WorkspaceID: workspaceID,
		}); err == nil {
			isWorkspaceMember = true
		}
	}
	for _, t := range targets {
		switch t.TargetType {
		case "workspace":
			if isWorkspaceMember {
				return true
			}
		case "member":
			if userID != "" && util.UUIDToString(t.TargetID) == userID {
				return true
			}
		}
	}
	return false
}

// PatchProjectDiscussionSessionConfig applies the three-state config PATCH to
// a shared Discussion session (SDD §3.2/§4.4, FR-8/FR-9): owner/admin only,
// workspace member gate, active-status check, then the L1/L2 provider/catalog
// authority ladder — all inside ONE transaction on the locked session row
// (same shape as the Private Ask patch). It NEVER calls UpdateAgent (AC-9).
//
// Lock order (B-AUTH-2, never reorder): LockSubscriberWrites -> session row
// lock. A revoke that committed first surfaces as 404 at the membership
// re-check; the whole transaction rolls back with zero writes.
func PatchProjectDiscussionSessionConfig(ctx context.Context, deps DiscussionSessionDeps, wsID, sessionID, callerID uuid.UUID, modelPatch, thinkingPatch ChatConfigFieldPatch) (*ProjectDiscussionSessionView, error) {
	if deps.Queries == nil || deps.TxStarter == nil || deps.TaskSvc == nil {
		return nil, fmt.Errorf("patch project discussion session config: deps incomplete")
	}
	ws := pgtype.UUID{Bytes: wsID, Valid: true}
	sid := pgtype.UUID{Bytes: sessionID, Valid: true}
	caller := pgtype.UUID{Bytes: callerID, Valid: true}

	tx, err := deps.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := deps.Queries.WithTx(tx)

	// First lock: the same (workspace, user) advisory revokeAndRemoveMember
	// takes first — send/PATCH and revoke serialize (B-AUTH-2).
	if err := qtx.LockSubscriberWrites(ctx, db.LockSubscriberWritesParams{
		WorkspaceID: ws, UserID: caller,
	}); err != nil {
		return nil, ErrChatSessionNotFound
	}
	// In-transaction membership re-check (advisory-visible latest state).
	member, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: caller, WorkspaceID: ws,
	})
	if err != nil {
		return nil, ErrChatSessionNotFound
	}
	if !isOwnerOrAdmin(member.Role) {
		return nil, ErrForbiddenChatConfig
	}

	session, err := qtx.LockChatSessionInWorkspace(ctx, db.LockChatSessionInWorkspaceParams{
		ID: sid, WorkspaceID: ws,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChatSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock chat session: %w", err)
	}
	if session.Kind != chatSessionKindProjectShared || !session.ProjectID.Valid {
		return nil, ErrChatSessionNotFound
	}
	if session.Status != "active" {
		return nil, ErrChatSessionClosedOrChanged
	}

	modelOverride := applyChatConfigFieldPatch(session.ModelOverride, modelPatch)
	thinkingOverride := applyChatConfigFieldPatch(session.ThinkingLevelOverride, thinkingPatch)
	resolved := ResolveChatConfig(
		session.BaseModel, modelOverride, pgtype.Text{},
		session.BaseThinkingLevel, thinkingOverride, pgtype.Text{},
	)

	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: session.ProjectID, WorkspaceID: ws,
	})
	if err != nil {
		return nil, fmt.Errorf("reload project under session lock: %w", err)
	}
	configured := discussionCoordinatorIDFromSettings(project.Settings)

	if err := validateProjectDiscussionConfig(ctx, qtx, deps.TaskSvc, ws, configured, resolved); err != nil {
		return nil, err
	}

	updated, err := qtx.PatchChatSessionConfig(ctx, db.PatchChatSessionConfigParams{
		ID:                    sid,
		WorkspaceID:           ws,
		ModelOverride:         modelOverride,
		ThinkingLevelOverride: thinkingOverride,
	})
	if err != nil {
		return nil, fmt.Errorf("patch shared session config: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit config patch: %w", err)
	}

	view := ResolveChatConfig(
		updated.BaseModel, updated.ModelOverride, pgtype.Text{},
		updated.BaseThinkingLevel, updated.ThinkingLevelOverride, pgtype.Text{},
	)
	return &ProjectDiscussionSessionView{
		SessionID:           util.UUIDToString(updated.ID),
		CoordinatorAgentID:  discussionCoordinatorSettingRaw(project.Settings),
		Model:               view.Model,
		ModelSource:         string(view.ModelSource),
		ThinkingLevel:       view.ThinkingLevel,
		ThinkingLevelSource: string(view.ThinkingLevelSource),
	}, nil
}

// validateProjectDiscussionConfig runs the §4.4 provider/catalog authority
// ladder — the SINGLE validation shared by the shared PATCH and the coordinator
// enqueue preflight (AC-21: only ResolveChatConfig / LoadChatCatalogForConfig /
// ValidateResolvedChatConfig / runtimeVerdict are reused; no second rule set).
//
//   - L1: settings name a Coordinator whose agent row exists (routable or
//     archived) → that agent is the authority (archived → Blocked → reject).
//   - L2: unconfigured, or the configured agent was hard-deleted → the
//     workspace ready-runtime union is the authority: a pure sentinel
//     (both values empty) passes; otherwise the value must be accepted by at
//     least one ready runtime's catalog, at most one bounded 30s LiveLoad
//     round per PATCH. All rejected → fail closed.
func validateProjectDiscussionConfig(ctx context.Context, qtx *db.Queries, taskSvc *TaskService, ws, configured pgtype.UUID, resolved ResolvedChatConfig) error {
	if configured.Valid {
		agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID: configured, WorkspaceID: ws,
		})
		if err == nil {
			// L1: authority = the configured agent row.
			provider := ""
			if agent.RuntimeID.Valid {
				if rt, rerr := qtx.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
					ID: agent.RuntimeID, WorkspaceID: ws,
				}); rerr == nil {
					provider = rt.Provider
				}
			}
			if provider == "" || taskSvc == nil || taskSvc.ChatCatalog == nil {
				return ErrInvalidModelOrThinkingLevel
			}
			catalog, cerr := LoadChatCatalogForConfig(ctx, qtx, taskSvc.ChatCatalog, agent)
			if cerr != nil {
				return cerr
			}
			return ValidateResolvedChatConfig(resolved.Model, resolved.ThinkingLevel, provider, catalog)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load configured coordinator: %w", err)
		}
		// Hard-deleted configured agent: fall through to L2.
	}

	// L2: workspace ready-runtime union.
	if resolved.Model == "" && resolved.ThinkingLevel == "" {
		return nil // pure sentinel: runtime defaults take over (FR-8)
	}
	if taskSvc == nil || taskSvc.ChatCatalog == nil {
		return ErrInvalidModelOrThinkingLevel
	}
	runtimes, err := qtx.ListAgentRuntimes(ctx, ws)
	if err != nil {
		return fmt.Errorf("list agent runtimes: %w", err)
	}
	liveLoaded := false
	for _, rt := range runtimes {
		if !runtimeVerdict(rt).Ready() {
			continue
		}
		runtimeID := util.UUIDToString(rt.ID)
		catalog, ok, cerr := taskSvc.ChatCatalog.CacheLoad(ctx, runtimeID)
		if cerr != nil || !ok {
			if liveLoaded {
				continue // one bounded LiveLoad round per PATCH (SDD §4.4)
			}
			liveCtx, cancel := context.WithTimeout(ctx, chatConfigLiveLoadTimeout)
			catalog, cerr = taskSvc.ChatCatalog.LiveLoad(liveCtx, runtimeID)
			cancel()
			liveLoaded = true
			if cerr != nil || catalog.Fallback || len(catalog.Models) == 0 {
				continue
			}
		}
		if ValidateResolvedChatConfig(resolved.Model, resolved.ThinkingLevel, rt.Provider, catalog) == nil {
			return nil // any ready runtime accepting the value is enough
		}
	}
	return ErrInvalidModelOrThinkingLevel
}

// binding change (bind / replace / unbind) in ONE transaction under the
// Discussion session advisory (SDD §4.5, FR-26): the settings bag is the write
// authority (written first), then the session's agent_id projection is updated
// in place — first binding backfills base_* with the new agent's defaults,
// replacement never re-snapshots, unbind (newAgentID == nil) clears the
// projection to NULL. History stays in the same session (decision D-5).
func UpdateProjectSettingsWithDiscussionCoordinator(ctx context.Context, deps DiscussionSessionDeps, wsID, projectID uuid.UUID, newAgentID *uuid.UUID) error {
	if deps.Queries == nil || deps.TxStarter == nil {
		return fmt.Errorf("update project settings with discussion coordinator: deps incomplete")
	}
	ws := pgtype.UUID{Bytes: wsID, Valid: true}
	project := pgtype.UUID{Bytes: projectID, Valid: true}

	patch := map[string]any{}
	if newAgentID == nil {
		patch[ProjectSettingDiscussionCoordinatorID] = nil // jsonb concatenation with null deletes the key
	} else {
		patch[ProjectSettingDiscussionCoordinatorID] = newAgentID.String()
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode coordinator settings patch: %w", err)
	}

	tx, err := deps.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := deps.Queries.WithTx(tx)

	if err := qtx.LockIssueDuplicateKey(ctx, discussionSessionAdvisoryKey(ws, project)); err != nil {
		return fmt.Errorf("lock project discussion session key: %w", err)
	}
	// Write authority first (SDD §4.5).
	if _, err := qtx.UpdateProjectSettings(ctx, db.UpdateProjectSettingsParams{
		ID: project, WorkspaceID: ws, Patch: patchJSON,
	}); err != nil {
		return fmt.Errorf("update project settings: %w", err)
	}

	session, err := qtx.GetActiveProjectSharedSession(ctx, db.GetActiveProjectSharedSessionParams{
		WorkspaceID: ws, ProjectID: project,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit coordinator bind: %w", err)
		}
		return nil // no active session yet: the next GET projects the binding
	}
	if err != nil {
		return fmt.Errorf("load active project discussion session: %w", err)
	}

	if newAgentID != nil {
		agentID := pgtype.UUID{Bytes: *newAgentID, Valid: true}
		// First binding on a session that never snapshotted: backfill base_*
		// with the new agent's defaults (never overwrites an existing
		// snapshot; replacement bindings keep the original base_*).
		if !session.BaseModel.Valid && !session.BaseThinkingLevel.Valid {
			agent, aerr := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
				ID: agentID, WorkspaceID: ws,
			})
			if aerr != nil {
				return fmt.Errorf("load coordinator agent for snapshot: %w", aerr)
			}
			baseModel, baseThinking := SnapshotAgentDefaults(agent)
			if _, aerr := qtx.BackfillChatSessionBaseIfNull(ctx, db.BackfillChatSessionBaseIfNullParams{
				ID: session.ID, WorkspaceID: ws,
				BaseModel: baseModel, BaseThinkingLevel: baseThinking,
			}); aerr != nil {
				return fmt.Errorf("backfill discussion session base: %w", aerr)
			}
		}
		if _, err := qtx.SetChatSessionAgentID(ctx, db.SetChatSessionAgentIDParams{
			ID: session.ID, AgentID: agentID,
		}); err != nil {
			return fmt.Errorf("project coordinator onto session: %w", err)
		}
	} else {
		// Unbind: clear the projection to NULL (legal since migration 481).
		if _, err := qtx.SetChatSessionAgentID(ctx, db.SetChatSessionAgentIDParams{
			ID: session.ID, AgentID: pgtype.UUID{},
		}); err != nil {
			return fmt.Errorf("clear coordinator projection: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit coordinator bind: %w", err)
	}
	return nil
}
