package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	publicapi "github.com/multica-ai/multica/server/pkg/publicapi/v1"
)

// chatSessionKindProjectShared mirrors chat_session.kind's shared Discussion
// value (migration 482 CHECK enum).
const chatSessionKindProjectShared = "project_shared"

// DiscussionSendResponse is the POST /api/chat/sessions/{sid}/messages success
// body for shared Discussion sessions (SDD §3.4). IssueID stays nil forever —
// the shared session is issue-free by construction.
type DiscussionSendResponse struct {
	SessionID string  `json:"session_id"`
	MessageID string  `json:"message_id"`
	IssueID   *string `json:"issue_id"` // always nil
	TaskID    *string `json:"task_id"`  // ordinary messages: nil; coordinator turns: task UUID
}

// ProjectDiscussionConfigResponse is the shared Discussion PATCH config
// success body (SDD §3.2: same fields as the GET config surface).
type ProjectDiscussionConfigResponse struct {
	Model               string `json:"model"`
	ThinkingLevel       string `json:"thinking_level"`
	ModelSource         string `json:"model_source"`
	ThinkingLevelSource string `json:"thinking_level_source"`
}

// discussionSessionDeps assembles the service dependency surface for the
// shared Discussion paths (handler -> service layering; the same TaskService
// instance owns TxStarter and the catalog port).
func (h *Handler) discussionSessionDeps() service.DiscussionSessionDeps {
	return service.DiscussionSessionDeps{
		Queries:   h.Queries,
		TxStarter: h.TxStarter,
		TaskSvc:   h.TaskService,
	}
}

// sendSharedDiscussionMessage runs the shared Discussion send (SDD §3.4/§4.2,
// FR-10/FR-11/FR-15/FR-17/FR-24). The caller has already pre-loaded the
// session and confirmed kind=project_shared; this function performs the
// shared-specific input validation, delegates the transaction to
// service.SendDiscussionMessage, maps the error contract, and owns the
// post-commit event publishing (the service returns results only).
func (h *Handler) sendSharedDiscussionMessage(w http.ResponseWriter, r *http.Request, userID, workspaceID string, session db.ChatSession, req SendChatMessageRequest) {
	wsUUID := parseUUID(workspaceID)
	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Input validation (SDD §3.4, zero writes on every rejection).
	trimmed := strings.TrimSpace(req.Content)
	if trimmed == "" && len(req.AttachmentIDs) == 0 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_discussion_message", "content or attachments are required")
		return
	}
	seen := make(map[string]struct{}, len(req.AttachmentIDs))
	for _, raw := range req.AttachmentIDs {
		if _, dup := seen[raw]; dup {
			// Duplicates are rejected, never silently collapsed — a duplicate
			// id must not reach the fingerprint (SDD §4.6).
			writeErrorCode(w, http.StatusBadRequest, "invalid_discussion_message", "attachment_ids must not contain duplicates")
			return
		}
		seen[raw] = struct{}{}
	}
	attachmentIDs, ok := parseUUIDSliceOrBadRequest(w, req.AttachmentIDs, "attachment_ids")
	if !ok {
		return
	}
	coordinatorRequest := req.CoordinatorRequest
	if coordinatorRequest == "" {
		coordinatorRequest = "none"
	}
	switch coordinatorRequest {
	case "none", "mention", "analyze", "summarize":
	default:
		writeErrorCode(w, http.StatusBadRequest, "invalid_discussion_message", "coordinator_request must be one of none, mention, analyze, summarize")
		return
	}
	idempotencyKey := r.Header.Get(publicapi.HeaderIdempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > publicapi.MaxIdempotencyBytes {
		writeErrorCode(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required (at most 255 bytes)")
		return
	}

	// Pre-transaction member fast-fail (SDD §3.4); the authoritative re-check
	// runs inside the service transaction under LockSubscriberWrites.
	if _, err := h.getWorkspaceMember(r.Context(), userID, workspaceID); err != nil {
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
		return
	}

	input := service.DiscussionSendInput{
		Content:            trimmed,
		AttachmentIDs:      pgUUIDsToUUIDs(attachmentIDs),
		CoordinatorRequest: coordinatorRequest,
	}
	result, derr := service.SendDiscussionMessage(r.Context(), h.discussionSessionDeps(),
		pgToUUID(wsUUID), pgToUUID(callerUUID), pgToUUID(session.ID), input, idempotencyKey)
	if derr != nil {
		writeDiscussionSendError(w, derr.Code)
		return
	}

	// Post-commit publishing (SDD §4.2): workspace broadcast for the message,
	// then — for coordinator turns — the queued task event + daemon wakeup.
	resolvedSessionID := uuidToString(session.ID)
	authorType := "member"
	authorID := userID
	h.publishChat(protocol.EventChatMessage, workspaceID, "member", userID, resolvedSessionID, "", protocol.ChatMessagePayload{
		ChatSessionID: resolvedSessionID,
		MessageID:     result.MessageID.String(),
		Role:          "user",
		Content:       req.Content,
		AuthorType:    &authorType,
		AuthorID:      &authorID,
		CreatedAt:     timestampToString(pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}),
	}, chatSessionKindProjectShared)

	if result.TaskID != nil {
		taskUUID := result.TaskID.String()
		if task, terr := h.Queries.GetAgentTask(r.Context(), pgtype.UUID{Bytes: *result.TaskID, Valid: true}); terr == nil {
			h.TaskService.BroadcastTaskQueued(r.Context(), task)
			h.TaskService.NotifyTaskEnqueued(r.Context(), task)
		}
		writeJSON(w, http.StatusCreated, DiscussionSendResponse{
			SessionID: resolvedSessionID,
			MessageID: result.MessageID.String(),
			IssueID:   nil,
			TaskID:    &taskUUID,
		})
		return
	}
	writeJSON(w, http.StatusCreated, DiscussionSendResponse{
		SessionID: resolvedSessionID,
		MessageID: result.MessageID.String(),
		IssueID:   nil,
		TaskID:    nil,
	})
}

// writeDiscussionSendError maps service.DiscussionSendError codes to the PRD
// error-code matrix (SDD §3.4/FR-18).
func writeDiscussionSendError(w http.ResponseWriter, code string) {
	switch code {
	case "chat_session_not_found":
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
	case "chat_session_closed_or_changed":
		writeErrorCode(w, http.StatusConflict, "chat_session_closed_or_changed", "session closed or the project's Coordinator changed")
	case "discussion_coordinator_not_configured":
		writeErrorCode(w, http.StatusConflict, "discussion_coordinator_not_configured", "project has no Discussion Coordinator configured")
	case "discussion_coordinator_unavailable":
		writeErrorCode(w, http.StatusConflict, "discussion_coordinator_unavailable", "the Discussion Coordinator is unavailable")
	case "invalid_model_or_thinking_level":
		writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "invalid model or thinking level")
	case "attachment_already_bound":
		writeErrorCode(w, http.StatusConflict, "attachment_already_bound", "a draft attachment is already bound")
	case "idempotency_key_reused":
		writeErrorCode(w, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used with a different request")
	case "invocation_not_allowed":
		// Same dispatch reason as every other invocation gate (MUL-3963); no
		// new permission enum is introduced.
		writeErrorCode(w, http.StatusForbidden, "invocation_not_allowed", "you are not allowed to trigger this agent")
	default:
		writeError(w, http.StatusInternalServerError, "failed to send discussion message")
	}
}

// patchSharedDiscussionConfig runs the shared Discussion PATCH config branch
// (SDD §3.2/§4.4, FR-9): owner/admin gate + the L1/L2 provider/catalog
// authority ladder live inside service.PatchProjectDiscussionSessionConfig.
func (h *Handler) patchSharedDiscussionConfig(w http.ResponseWriter, r *http.Request, wsUUID, sessionUUID, callerUUID pgtype.UUID, modelPatch, thinkingPatch service.ChatConfigFieldPatch) {
	view, err := service.PatchProjectDiscussionSessionConfig(r.Context(), h.discussionSessionDeps(),
		pgToUUID(wsUUID), pgToUUID(sessionUUID), pgToUUID(callerUUID), modelPatch, thinkingPatch)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrChatSessionNotFound):
			writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
		case errors.Is(err, service.ErrForbiddenChatConfig):
			writeErrorCode(w, http.StatusForbidden, "forbidden_chat_config", "chat config requires owner or admin")
		case errors.Is(err, service.ErrChatSessionClosedOrChanged):
			writeErrorCode(w, http.StatusConflict, "chat_session_closed_or_changed", "session closed or the project's Coordinator changed")
		case errors.Is(err, service.ErrInvalidModelOrThinkingLevel):
			writeErrorCode(w, http.StatusBadRequest, "invalid_model_or_thinking_level", "invalid model or thinking level")
		default:
			writeError(w, http.StatusInternalServerError, "failed to update shared discussion config")
		}
		return
	}
	writeJSON(w, http.StatusOK, ProjectDiscussionConfigResponse{
		Model:               view.Model,
		ModelSource:         view.ModelSource,
		ThinkingLevel:       view.ThinkingLevel,
		ThinkingLevelSource: view.ThinkingLevelSource,
	})
}

// loadChatMessagesShared lists messages for a shared Discussion session
// (SDD §3.3/FR-22): member gate, page-object response for BOTH endpoints,
// archived sessions stay readable (200). limit/cursor violations map to
// invalid_cursor.
func (h *Handler) loadChatMessagesShared(w http.ResponseWriter, r *http.Request, userID, workspaceID string, session db.ChatSession) {
	if _, err := h.getWorkspaceMember(r.Context(), userID, workspaceID); err != nil {
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
		return
	}
	limit, beforeCreatedAt, beforeID, err := parseChatMessagesPageParams(r)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}

	messages, err := h.Queries.ListChatMessagesPage(r.Context(), db.ListChatMessagesPageParams{
		ChatSessionID:   session.ID,
		Limit:           int32(limit + 2),
		BeforeCreatedAt: beforeCreatedAt,
		BeforeID:        beforeID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat messages")
		return
	}
	messages = visibleChatMessages(messages)
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	var nextCursor *ChatMessagesCursorResponse
	if hasMore && len(messages) > 0 {
		oldest := messages[len(messages)-1]
		nextCursor = &ChatMessagesCursorResponse{
			CreatedAt: oldest.CreatedAt.Time.Format(time.RFC3339Nano),
			ID:        uuidToString(oldest.ID),
		}
	}
	// SQL fetches newest windows first; reverse for chronological order.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	messageIDs := make([]pgtype.UUID, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	groupedAtt := h.groupChatMessageAttachments(r.Context(), workspaceID, messageIDs)

	resp := make([]ChatMessageResponse, len(messages))
	for i, m := range messages {
		resp[i] = chatMessageToResponse(m, groupedAtt[uuidToString(m.ID)])
	}
	// Both /messages and /messages/page return the page object for shared
	// sessions — never a bare array (SDD §3.3).
	writeJSON(w, http.StatusOK, ChatMessagesPageResponse{
		Messages:   resp,
		Limit:      limit,
		HasMore:    hasMore,
		NextCursor: nextCursor,
	})
}

// sharedSessionMemberGate is the shared read/write admission helper for the
// §3.6 endpoint closure. memberOnly=true adds the owner/admin role gate (403
// forbidden_chat_config). Non-members get 404 chat_session_not_found (existence
// is never confirmed, FR-17).
func (h *Handler) sharedSessionMemberGate(w http.ResponseWriter, r *http.Request, userID, workspaceID string, memberOnly bool) (db.Member, bool) {
	member, err := h.getWorkspaceMember(r.Context(), userID, workspaceID)
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
		return db.Member{}, false
	}
	if memberOnly && !roleAllowed(member.Role, "owner", "admin") {
		writeErrorCode(w, http.StatusForbidden, "forbidden_chat_config", "chat config requires owner or admin")
		return db.Member{}, false
	}
	return member, true
}

// pgToUUID converts a pgtype.UUID into google/uuid.UUID (trusted round-trip).
func pgToUUID(u pgtype.UUID) uuid.UUID {
	return uuid.UUID(u.Bytes)
}

// pgUUIDsToUUIDs maps a slice of pgtype.UUID into google/uuid.UUID.
func pgUUIDsToUUIDs(ids []pgtype.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, pgToUUID(id))
	}
	return out
}

// sharedSessionAdmission is the §3.6 closure admission helper for endpoints
// gated with gatePublicChatSessionForUser: shared sessions use member access
// (owner/admin when sharedRequiresRole); private sessions keep the existing
// creator-gated flow byte-for-byte.
func (h *Handler) loadChatSessionForPublicGate(w http.ResponseWriter, r *http.Request, userID, workspaceID, sessionID string, sharedRequiresRole bool) (db.ChatSession, bool) {
	if wsUUID, werr := util.ParseUUID(workspaceID); werr == nil {
		if sidUUID, serr := util.ParseUUID(sessionID); serr == nil {
			if session, lerr := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{
				ID: sidUUID, WorkspaceID: wsUUID,
			}); lerr == nil && session.Kind == chatSessionKindProjectShared {
				if _, ok := h.sharedSessionMemberGate(w, r, userID, workspaceID, sharedRequiresRole); !ok {
					return db.ChatSession{}, false
				}
				return session, true
			}
		}
	}
	return h.gatePublicChatSessionForUser(w, r, userID, workspaceID, sessionID)
}

// loadChatSessionForOwnerGate is the loadChatSessionForUser counterpart of
// loadChatSessionForPublicGate (same §3.6 closure, different private gate).
func (h *Handler) loadChatSessionForOwnerGate(w http.ResponseWriter, r *http.Request, userID, workspaceID, sessionID string, sharedRequiresRole bool) (db.ChatSession, bool) {
	if wsUUID, werr := util.ParseUUID(workspaceID); werr == nil {
		if sidUUID, serr := util.ParseUUID(sessionID); serr == nil {
			if session, lerr := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{
				ID: sidUUID, WorkspaceID: wsUUID,
			}); lerr == nil && session.Kind == chatSessionKindProjectShared {
				if _, ok := h.sharedSessionMemberGate(w, r, userID, workspaceID, sharedRequiresRole); !ok {
					return db.ChatSession{}, false
				}
				return session, true
			}
		}
	}
	return h.loadChatSessionForUser(w, r, userID, workspaceID, sessionID)
}
