package handler

// AIFIRST: CR-2026-061 (SDD §3.1/§4.3/§4.4): POST
// /api/projects/{projectId}/discussion/promote. Fixed zero-write validation
// order (PRD FR-8): shape → project → member → capacity → session → selection
// ownership; then the single service transaction. Every error uses the
// writeErrorCode {code, error} family (FR-13); the success contract is 201
// only.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	publicapi "github.com/multica-ai/multica/server/pkg/publicapi/v1"
)

// Promotion request bounds (SDD §3.1).
const (
	promotionTitleMaxRunes       = 200
	promotionDescriptionMaxRunes = 10000
)

// PromoteProjectDiscussionRequest is the parsed promotion body (SDD §3.1).
// UpgradeToCR is *bool so a missing field (default false) is distinguishable
// from a type error (which fails the JSON decode → 400 invalid_request_body).
type PromoteProjectDiscussionRequest struct {
	SessionID     string   `json:"session_id"`
	MessageIDs    []string `json:"message_ids"`
	AttachmentIDs []string `json:"attachment_ids"`
	Title         *string  `json:"title"`
	Description   *string  `json:"description"`
	UpgradeToCR   *bool    `json:"upgrade_to_cr"`
}

// validatePromotionRequest is the pure shape validator (step 1, SDD §4.4).
// It returns the fixed error code and ok=false on the first violation:
// session shape / non-UUID elements / duplicates / empty selection →
// invalid_promotion_selection; title >200 → invalid_promotion_title;
// description >10000 → invalid_promotion_description.
func validatePromotionRequest(req *PromoteProjectDiscussionRequest) (code string, ok bool) {
	sessionID, err := util.ParseUUID(req.SessionID)
	if err != nil || !sessionID.Valid {
		return "invalid_promotion_selection", false
	}
	seen := make(map[pgtype.UUID]struct{}, len(req.MessageIDs)+len(req.AttachmentIDs))
	messageIDs := make([]pgtype.UUID, 0, len(req.MessageIDs))
	for _, raw := range req.MessageIDs {
		id, err := util.ParseUUID(raw)
		if err != nil || !id.Valid {
			return "invalid_promotion_selection", false
		}
		if _, dup := seen[id]; dup {
			// Promotion rejects duplicates wholesale (AC-2), unlike
			// merge-forward's silent dedup.
			return "invalid_promotion_selection", false
		}
		seen[id] = struct{}{}
		messageIDs = append(messageIDs, id)
	}
	attachmentIDs := make([]pgtype.UUID, 0, len(req.AttachmentIDs))
	for _, raw := range req.AttachmentIDs {
		id, err := util.ParseUUID(raw)
		if err != nil || !id.Valid {
			return "invalid_promotion_selection", false
		}
		if _, dup := seen[id]; dup {
			return "invalid_promotion_selection", false
		}
		seen[id] = struct{}{}
		attachmentIDs = append(attachmentIDs, id)
	}
	if len(messageIDs) == 0 && len(attachmentIDs) == 0 {
		return "invalid_promotion_selection", false
	}
	if req.Title != nil && utf8.RuneCountInString(*req.Title) > promotionTitleMaxRunes {
		return "invalid_promotion_title", false
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > promotionDescriptionMaxRunes {
		return "invalid_promotion_description", false
	}
	return "", true
}

// HandlePromoteProjectDiscussion runs the promotion endpoint's zero-write
// pre-steps in the fixed PRD order and then delegates to the single
// PromoteDiscussion transaction.
func (h *Handler) HandlePromoteProjectDiscussion(w http.ResponseWriter, r *http.Request) {
	projectUUID, err := util.ParseUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_project_id", "project id must be a UUID")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	// Step 1: request shape + Idempotency-Key header (SDD §4.4).
	var req PromoteProjectDiscussionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
		return
	}
	idempotencyKey := r.Header.Get(publicapi.HeaderIdempotencyKey)
	if strings.TrimSpace(idempotencyKey) == "" {
		writeErrorCode(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
		return
	}
	if len(idempotencyKey) > publicapi.MaxIdempotencyBytes {
		writeErrorCode(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be at most 255 bytes")
		return
	}
	if code, ok := validatePromotionRequest(&req); !ok {
		writeErrorCode(w, http.StatusBadRequest, code, "invalid promotion selection")
		return
	}

	// Step 2: project existence.
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID:          projectUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "project_not_found", "project not found")
		return
	}

	callerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request_body", "invalid user id")
		return
	}

	// Step 3: membership gate — deliberately BEFORE session resolution so a
	// non-member never learns whether the session exists (FR-8/§7.1).
	if _, err := h.getWorkspaceMember(r.Context(), userID, uuidToString(wsUUID)); err != nil {
		writeErrorCode(w, http.StatusForbidden, "forbidden_promotion", "discussion promotion requires workspace membership")
		return
	}

	// Step 4: issue-create capacity preflight (D-6); the authoritative check
	// stays inside the transaction (AllocateIssueNumber).
	if err := service.CheckIssueCreateCapacity(r.Context(), h.Queries, h.Entitlements, wsUUID); err != nil {
		var limitErr *service.IssueLimitReachedError
		if errors.As(err, &limitErr) {
			writeErrorCode(w, http.StatusForbidden, "forbidden_promotion", "workspace issue limit reached")
			return
		}
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", "issue capacity check failed")
		return
	}

	// Step 5: the project's ACTIVE shared Discussion session must match the
	// requested session_id exactly (FR-3).
	session, err := h.Queries.GetActiveProjectSharedSession(r.Context(), db.GetActiveProjectSharedSessionParams{
		WorkspaceID: wsUUID,
		ProjectID:   projectUUID,
	})
	requestedSessionID, _ := util.ParseUUID(req.SessionID)
	if err != nil || session.ID != requestedSessionID {
		writeErrorCode(w, http.StatusNotFound, "chat_session_not_found", "chat session not found")
		return
	}

	// Step 6: source selection ownership — every message must belong to the
	// session; every attachment must be bound to a message of the session
	// (draft attachments have chat_message_id NULL and never match).
	messageIDs := make([]pgtype.UUID, 0, len(req.MessageIDs))
	for _, raw := range req.MessageIDs {
		id, _ := util.ParseUUID(raw)
		message, merr := h.Queries.GetChatMessageInWorkspace(r.Context(), db.GetChatMessageInWorkspaceParams{
			ID:          id,
			WorkspaceID: wsUUID,
		})
		if merr != nil || message.ChatSessionID != session.ID {
			writeErrorCode(w, http.StatusBadRequest, "invalid_promotion_selection", "every message must belong to this Discussion session")
			return
		}
		messageIDs = append(messageIDs, id)
	}
	attachmentIDs := make([]pgtype.UUID, 0, len(req.AttachmentIDs))
	if len(req.AttachmentIDs) > 0 {
		for _, raw := range req.AttachmentIDs {
			id, _ := util.ParseUUID(raw)
			attachmentIDs = append(attachmentIDs, id)
		}
		found, aerr := h.Queries.ListAttachmentsForPromotion(r.Context(), db.ListAttachmentsForPromotionParams{
			AttachmentIds: attachmentIDs,
			ChatSessionID: session.ID,
		})
		if aerr != nil || len(found) != len(attachmentIDs) {
			writeErrorCode(w, http.StatusBadRequest, "invalid_promotion_selection", "every attachment must be bound to a message of this Discussion session")
			return
		}
	}

	upgradeToCR := false
	if req.UpgradeToCR != nil {
		upgradeToCR = *req.UpgradeToCR
	}

	result, err := h.IssueService.PromoteDiscussion(r.Context(), service.PromoteDiscussionParams{
		WorkspaceID:    wsUUID,
		ProjectID:      project.ID,
		SessionID:      session.ID,
		CallerID:       callerUUID,
		MessageIDs:     messageIDs,
		AttachmentIDs:  attachmentIDs,
		Title:          req.Title,
		Description:    req.Description,
		UpgradeToCR:    upgradeToCR,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		var limitErr *service.IssueLimitReachedError
		switch {
		case errors.Is(err, service.ErrIdempotencyKeyReused):
			writeErrorCode(w, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used with a different request")
		case errors.Is(err, service.ErrPromotionRunCreateFailed):
			writeErrorCode(w, http.StatusBadGateway, "pipeline_run_create_failed", "promotion pipeline run creation failed")
		case errors.Is(err, service.ErrInvalidPromotionSelection):
			writeErrorCode(w, http.StatusBadRequest, "invalid_promotion_selection", "invalid promotion selection")
		case errors.Is(err, service.ErrPromotionForbidden):
			writeErrorCode(w, http.StatusForbidden, "forbidden_promotion", "discussion promotion requires workspace membership")
		case errors.Is(err, service.ErrProjectNotFound):
			writeErrorCode(w, http.StatusNotFound, "project_not_found", "project not found")
		case errors.As(err, &limitErr):
			writeErrorCode(w, http.StatusForbidden, "forbidden_promotion", "workspace issue limit reached")
		default:
			writeErrorCode(w, http.StatusInternalServerError, "internal_error", "promotion failed")
		}
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"issue_id":     uuidToString(result.IssueID),
		"issue_number": result.IssueNumber,
		"session_id":   uuidToString(result.SessionID),
		"source_refs": map[string]any{
			"session_id":     uuidToString(result.SourceRefs.SessionID),
			"message_ids":    uuidStringsOrEmpty(result.SourceRefs.MessageIDs),
			"attachment_ids": uuidStringsOrEmpty(result.SourceRefs.AttachmentIDs),
		},
		"created":       result.Created,
		"upgrade_to_cr": result.UpgradeToCR,
		"run_id":        uuidToPtr(result.RunID),
	})
}
