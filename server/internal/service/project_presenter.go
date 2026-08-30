package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Presenter grant status values (project_presenter_grant.status). A grant
// row's status IS its history — rows are never deleted, only transitioned to
// a terminal status (migration 163).
const (
	PresenterStatusPending     = "pending"
	PresenterStatusActive      = "active"
	PresenterStatusRejected    = "rejected"
	PresenterStatusReleased    = "released"
	PresenterStatusRevoked     = "revoked"
	PresenterStatusTransferred = "transferred"
)

// Presenter activity actions (CR-2026-010 SDD §4.5/DD-8). Stamped on the
// project's hidden chat container issue's activity_log so the message
// stream can replay them, and used verbatim as the inbox notification type
// for every action except "released" (no directed recipient).
const (
	PresenterActionRequested   = "presenter_requested"
	PresenterActionApproved    = "presenter_approved"
	PresenterActionRejected    = "presenter_rejected"
	PresenterActionTransferred = "presenter_transferred"
	PresenterActionRevoked     = "presenter_revoked"
	PresenterActionReleased    = "presenter_released"
)

// Presenter transition error codes (CR-2026-010 SDD §4.2). Handlers map
// these to HTTP status codes; see internal/handler/project_presenter.go.
const (
	PresenterErrRoleCannotRequest       = "role_cannot_request"
	PresenterErrRequestAlreadyPending   = "request_already_pending"
	PresenterErrNoPendingRequest        = "no_pending_request"
	PresenterErrPresenterAlreadyActive  = "presenter_already_active"
	PresenterErrNotPresenter            = "not_presenter"
	PresenterErrTargetNotMember         = "target_not_member"
	PresenterErrNoActivePresenter       = "no_active_presenter"
	PresenterErrInsufficientPermissions = "insufficient_permissions"
)

// ErrPresenterTransition is returned by every presenter grant transition for
// a rejected transition (role or state precondition failure) — infrastructure
// errors (DB down, etc.) surface as plain wrapped errors instead, never this
// type, so handlers can safely errors.As into it for the full rejection set.
type ErrPresenterTransition struct {
	Code    string
	Message string
}

func (e *ErrPresenterTransition) Error() string { return e.Message }

// PresenterState is the read-side projection for GET /presenter (SDD §4.2,
// TSUG-003). PendingRequests is only populated for owner/admin callers;
// MyRequest is populated for any caller with a pending request of their own,
// so a plain member can render "request pending" without needing the
// owner-only list.
type PresenterState struct {
	Presenter       *db.ProjectPresenterGrant
	PendingRequests []db.ProjectPresenterGrant
	MyRequest       *db.ProjectPresenterGrant
}

func presenterLockKey(workspaceID, projectID pgtype.UUID) string {
	return strings.Join([]string{"presenter", util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
}

func isOwnerOrAdmin(role string) bool { return role == "owner" || role == "admin" }

// requireOwner is the shared role gate for approve/reject/revoke (SDD §4.2:
// these three transitions are Owner-only, not Owner+Admin — presenter access
// itself defaults to Owner+Admin, but *governing* who holds it is Owner
// alone).
func requireOwner(ctx context.Context, qtx *db.Queries, workspaceID, userID pgtype.UUID) error {
	member, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: userID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("load caller membership: %w", err)
	}
	if member.Role != "owner" {
		return &ErrPresenterTransition{Code: PresenterErrInsufficientPermissions, Message: "only the workspace owner can perform this action"}
	}
	return nil
}

// RequestPresenter lets a plain member ask to become presenter. Owner/admin
// already drive the Team Agent by default (PRD §1) and never need to
// request — attempting to is rejected rather than silently accepted, so the
// UI can't be tricked into showing a pending state that can never resolve.
func (s *TaskService) RequestPresenter(ctx context.Context, project db.Project, callerID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		member, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID: callerID, WorkspaceID: project.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("load caller membership: %w", err)
		}
		if isOwnerOrAdmin(member.Role) {
			return &ErrPresenterTransition{Code: PresenterErrRoleCannotRequest, Message: "owner/admin already drive the Team Agent by default"}
		}
		if _, err := qtx.GetPendingPresenterGrantByUser(ctx, db.GetPendingPresenterGrantByUserParams{
			ProjectID: project.ID, UserID: callerID,
		}); err == nil {
			return &ErrPresenterTransition{Code: PresenterErrRequestAlreadyPending, Message: "a presenter request is already pending"}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check existing presenter request: %w", err)
		}
		g, err := qtx.CreatePresenterRequest(ctx, db.CreatePresenterRequestParams{
			WorkspaceID: project.WorkspaceID, ProjectID: project.ID, UserID: callerID,
		})
		if err != nil {
			return fmt.Errorf("create presenter request: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	owners, lerr := s.Queries.ListMembers(ctx, project.WorkspaceID)
	if lerr != nil {
		slog.Warn("presenter request: failed to list members for owner notification", "project_id", util.UUIDToString(project.ID), "error", lerr)
	}
	var recipients []pgtype.UUID
	for _, m := range owners {
		if m.Role == "owner" {
			recipients = append(recipients, m.UserID)
		}
	}
	s.recordPresenterActivity(ctx, project, callerID, PresenterActionRequested,
		map[string]string{"to_user_id": util.UUIDToString(callerID), "by_user_id": util.UUIDToString(callerID)},
		recipients)
	return grant, nil
}

// ApprovePresenter grants targetUserID's pending request, making them the
// project's presenter.
func (s *TaskService) ApprovePresenter(ctx context.Context, project db.Project, approverID, targetUserID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		if err := requireOwner(ctx, qtx, project.WorkspaceID, approverID); err != nil {
			return err
		}
		if _, err := qtx.GetPendingPresenterGrantByUser(ctx, db.GetPendingPresenterGrantByUserParams{
			ProjectID: project.ID, UserID: targetUserID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &ErrPresenterTransition{Code: PresenterErrNoPendingRequest, Message: "no pending presenter request for this user"}
			}
			return fmt.Errorf("load pending presenter request: %w", err)
		}
		if _, err := qtx.GetActivePresenterGrant(ctx, project.ID); err == nil {
			return &ErrPresenterTransition{Code: PresenterErrPresenterAlreadyActive, Message: "a presenter is already active for this project"}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check active presenter: %w", err)
		}
		g, err := qtx.ApprovePresenterGrant(ctx, db.ApprovePresenterGrantParams{
			ProjectID: project.ID, UserID: targetUserID, GrantedBy: approverID,
		})
		if err != nil {
			return fmt.Errorf("approve presenter grant: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	s.recordPresenterActivity(ctx, project, approverID, PresenterActionApproved,
		map[string]string{"to_user_id": util.UUIDToString(targetUserID), "by_user_id": util.UUIDToString(approverID)},
		[]pgtype.UUID{targetUserID})
	return grant, nil
}

// RejectPresenter denies targetUserID's pending request, leaving the
// project's presenter (if any) unchanged.
func (s *TaskService) RejectPresenter(ctx context.Context, project db.Project, approverID, targetUserID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		if err := requireOwner(ctx, qtx, project.WorkspaceID, approverID); err != nil {
			return err
		}
		g, err := qtx.RejectPresenterGrant(ctx, db.RejectPresenterGrantParams{
			ProjectID: project.ID, UserID: targetUserID, ResolvedBy: approverID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &ErrPresenterTransition{Code: PresenterErrNoPendingRequest, Message: "no pending presenter request for this user"}
			}
			return fmt.Errorf("reject presenter grant: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	s.recordPresenterActivity(ctx, project, approverID, PresenterActionRejected,
		map[string]string{"to_user_id": util.UUIDToString(targetUserID), "by_user_id": util.UUIDToString(approverID)},
		[]pgtype.UUID{targetUserID})
	return grant, nil
}

// TransferPresenter hands control from the current presenter (callerID)
// directly to targetUserID, without a new request/approve cycle.
func (s *TaskService) TransferPresenter(ctx context.Context, project db.Project, callerID, targetUserID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		active, err := qtx.GetActivePresenterGrant(ctx, project.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &ErrPresenterTransition{Code: PresenterErrNotPresenter, Message: "you are not the current presenter"}
			}
			return fmt.Errorf("load active presenter: %w", err)
		}
		if active.UserID != callerID {
			return &ErrPresenterTransition{Code: PresenterErrNotPresenter, Message: "you are not the current presenter"}
		}
		if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID: targetUserID, WorkspaceID: project.WorkspaceID,
		}); err != nil {
			return &ErrPresenterTransition{Code: PresenterErrTargetNotMember, Message: "transfer target is not a workspace member"}
		}
		if _, err := qtx.CloseActivePresenterGrant(ctx, db.CloseActivePresenterGrantParams{
			ProjectID: project.ID, Status: PresenterStatusTransferred, ResolvedBy: callerID,
		}); err != nil {
			return fmt.Errorf("close active presenter grant: %w", err)
		}
		g, err := qtx.CreateActivePresenterGrant(ctx, db.CreateActivePresenterGrantParams{
			WorkspaceID: project.WorkspaceID, ProjectID: project.ID, UserID: targetUserID, GrantedBy: callerID,
		})
		if err != nil {
			return fmt.Errorf("create transferred presenter grant: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	s.recordPresenterActivity(ctx, project, callerID, PresenterActionTransferred,
		map[string]string{"from_user_id": util.UUIDToString(callerID), "to_user_id": util.UUIDToString(targetUserID), "by_user_id": util.UUIDToString(callerID)},
		[]pgtype.UUID{targetUserID})
	return grant, nil
}

// RevokePresenter forcibly ends the active presenter's control (Owner only).
func (s *TaskService) RevokePresenter(ctx context.Context, project db.Project, approverID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		if err := requireOwner(ctx, qtx, project.WorkspaceID, approverID); err != nil {
			return err
		}
		g, err := qtx.CloseActivePresenterGrant(ctx, db.CloseActivePresenterGrantParams{
			ProjectID: project.ID, Status: PresenterStatusRevoked, ResolvedBy: approverID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &ErrPresenterTransition{Code: PresenterErrNoActivePresenter, Message: "no active presenter for this project"}
			}
			return fmt.Errorf("revoke presenter grant: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	s.recordPresenterActivity(ctx, project, approverID, PresenterActionRevoked,
		map[string]string{"from_user_id": util.UUIDToString(grant.UserID), "by_user_id": util.UUIDToString(approverID)},
		[]pgtype.UUID{grant.UserID})
	return grant, nil
}

// ReleasePresenter lets the current presenter voluntarily give up control.
func (s *TaskService) ReleasePresenter(ctx context.Context, project db.Project, callerID pgtype.UUID) (db.ProjectPresenterGrant, error) {
	var grant db.ProjectPresenterGrant
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockIssueDuplicateKey(ctx, presenterLockKey(project.WorkspaceID, project.ID)); err != nil {
			return fmt.Errorf("lock presenter key: %w", err)
		}
		active, err := qtx.GetActivePresenterGrant(ctx, project.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &ErrPresenterTransition{Code: PresenterErrNotPresenter, Message: "you are not the current presenter"}
			}
			return fmt.Errorf("load active presenter: %w", err)
		}
		if active.UserID != callerID {
			return &ErrPresenterTransition{Code: PresenterErrNotPresenter, Message: "you are not the current presenter"}
		}
		g, err := qtx.CloseActivePresenterGrant(ctx, db.CloseActivePresenterGrantParams{
			ProjectID: project.ID, Status: PresenterStatusReleased, ResolvedBy: callerID,
		})
		if err != nil {
			return fmt.Errorf("release presenter grant: %w", err)
		}
		grant = g
		return nil
	})
	if err != nil {
		return grant, err
	}
	// No notifyDirect recipient (SDD §4.5): releasing has no directed target,
	// every project member sees it from the activity card alone.
	s.recordPresenterActivity(ctx, project, callerID, PresenterActionReleased,
		map[string]string{"from_user_id": util.UUIDToString(callerID), "by_user_id": util.UUIDToString(callerID)},
		nil)
	return grant, nil
}

// GetPresenterState is the read side for GET /presenter (TSUG-003): the
// active presenter (if any), the full pending list for owner/admin callers,
// and the caller's own pending request (any role) — not run in a
// transaction, this is a read-only projection with no serialization needs.
func (s *TaskService) GetPresenterState(ctx context.Context, project db.Project, callerID pgtype.UUID) (PresenterState, error) {
	var state PresenterState

	active, err := s.Queries.GetActivePresenterGrant(ctx, project.ID)
	switch {
	case err == nil:
		state.Presenter = &active
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return state, fmt.Errorf("load active presenter: %w", err)
	}

	member, err := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: callerID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		return state, fmt.Errorf("load caller membership: %w", err)
	}
	if isOwnerOrAdmin(member.Role) {
		pending, err := s.Queries.ListPendingPresenterGrants(ctx, project.ID)
		if err != nil {
			return state, fmt.Errorf("list pending presenter requests: %w", err)
		}
		state.PendingRequests = pending
	}

	mine, err := s.Queries.GetPendingPresenterGrantByUser(ctx, db.GetPendingPresenterGrantByUserParams{
		ProjectID: project.ID, UserID: callerID,
	})
	switch {
	case err == nil:
		state.MyRequest = &mine
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return state, fmt.Errorf("load caller presenter request: %w", err)
	}

	return state, nil
}

// recordPresenterActivity is the shared notification tail for all six
// transitions (SDD §4.5/DD-8/DD-9), called after the transition's own
// transaction has committed: the grant row is the transition's authoritative
// state, so a notification failure here must never roll it back — this only
// logs and returns on any sub-step failure (CreateComment's existing
// "notification is best-effort" precedent).
//
// It writes one activity_log row on the project's hidden chat container issue
// (so the message stream can replay it) and publishes two bus events:
// activity:created (for the message-stream card, carrying a "presenter_notify"
// list of recipient user ids in its payload — inbox dispatch happens in
// cmd/server's registerPresenterNotificationListeners, since notifyDirect
// lives in package main and this service package cannot call it directly)
// and project:presenter_changed (for the chat header, SDD §5.2 — published
// unconditionally, even for request/reject which don't change who is active,
// so the frontend's pending-request badge also refreshes).
//
// details is the activity/inbox details payload; project_id is injected into
// it here (TSUG-002: inbox items for these types must route to the project's
// Chat tab, not the hidden container issue, so the frontend needs project_id
// in details regardless of action). notifyRecipients is empty for "released"
// (SDD §4.5: no directed recipient, the activity card is enough).
func (s *TaskService) recordPresenterActivity(ctx context.Context, project db.Project, actorID pgtype.UUID, action string, details map[string]string, notifyRecipients []pgtype.UUID) {
	if details == nil {
		details = map[string]string{}
	}
	details["project_id"] = util.UUIDToString(project.ID)
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		slog.Warn("presenter activity: failed to marshal details", "action", action, "error", err)
		detailsJSON = []byte("{}")
	}

	// CR-2026-056 (SDD §9 #35): presenter activity hangs on the ACTIVE
	// session's bound container (project_chat_session.issue_id), not on the
	// project's earliest project_chat issue — after a rebind that one belongs
	// to a closed session's timeline. An unbound session skips recording,
	// matching the previous "issue not found" behavior.
	session, err := s.Queries.GetActiveProjectChatSession(ctx, db.GetActiveProjectChatSessionParams{
		WorkspaceID: project.WorkspaceID, ProjectID: project.ID,
	})
	if err != nil || !session.IssueID.Valid {
		slog.Warn("presenter activity skipped: no active bound chat session",
			"project_id", util.UUIDToString(project.ID), "action", action, "error", err)
	} else if activity, err := s.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: project.WorkspaceID,
		IssueID:     session.IssueID,
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     actorID,
		Action:      action,
		Details:     detailsJSON,
	}); err != nil {
		slog.Warn("presenter activity record failed", "action", action, "error", err)
	} else {
		notify := make([]string, len(notifyRecipients))
		for i, r := range notifyRecipients {
			notify[i] = util.UUIDToString(r)
		}
		s.Bus.Publish(events.Event{
			Type:        protocol.EventActivityCreated,
			WorkspaceID: util.UUIDToString(project.WorkspaceID),
			ActorType:   "member",
			ActorID:     util.UUIDToString(actorID),
			Payload: map[string]any{
				"issue_id": util.UUIDToString(session.IssueID),
				"entry": map[string]any{
					"type":       "activity",
					"id":         util.UUIDToString(activity.ID),
					"actor_type": "member",
					"actor_id":   util.UUIDToString(actorID),
					"action":     activity.Action,
					"details":    json.RawMessage(detailsJSON),
					"created_at": activity.CreatedAt.Time.Format(time.RFC3339),
				},
				"presenter_notify": notify,
			},
		})
	}

	var presenterUserID any
	if active, aerr := s.Queries.GetActivePresenterGrant(ctx, project.ID); aerr == nil {
		presenterUserID = util.UUIDToString(active.UserID)
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventProjectPresenterChanged,
		WorkspaceID: util.UUIDToString(project.WorkspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(actorID),
		Payload: map[string]any{
			"project_id":        util.UUIDToString(project.ID),
			"presenter_user_id": presenterUserID,
		},
	})
}
