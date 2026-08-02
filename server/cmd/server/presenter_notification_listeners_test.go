package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestPresenterNotifications is CR-2026-010 TASK-04's end-to-end proof (SDD
// §4.5/DD-8/DD-9, acceptance criteria 1-3): every transition writes one
// activity_log row and publishes activity:created + project:presenter_changed
// exactly once; five of the six transitions (all but release) fan out an
// inbox_item to their SDD §4.5 recipient(s); a multi-owner project's request
// notifies every owner.
func TestPresenterNotifications(t *testing.T) {
	ctx := context.Background()
	pool := testPool
	queries := db.New(pool)

	bus := events.New()
	registerPresenterNotificationListeners(bus, queries)
	var activityEvents, presenterChangedEvents int
	bus.Subscribe(protocol.EventActivityCreated, func(e events.Event) { activityEvents++ })
	bus.Subscribe(protocol.EventProjectPresenterChanged, func(e events.Event) { presenterChangedEvents++ })

	svc := service.NewTaskService(queries, pool, nil, bus)
	fx := createPresenterNotificationFixture(t, ctx, pool)
	project := db.Project{ID: util.MustParseUUID(fx.projectID), WorkspaceID: util.MustParseUUID(fx.workspaceID)}

	assertActivity := func(t *testing.T, action string, wantCount int) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2`, fx.issueID, action).Scan(&n); err != nil {
			t.Fatalf("count activity_log rows for %s: %v", action, err)
		}
		if n != wantCount {
			t.Fatalf("activity_log[%s]: want %d row(s), got %d", action, wantCount, n)
		}
	}
	assertInboxCount := func(t *testing.T, recipientID, notifType string, want int) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2`, recipientID, notifType).Scan(&n); err != nil {
			t.Fatalf("count inbox_item rows for %s/%s: %v", recipientID, notifType, err)
		}
		if n != want {
			t.Fatalf("inbox_item[recipient=%s type=%s]: want %d row(s), got %d", recipientID, notifType, want, n)
		}
	}
	assertInboxProjectID := func(t *testing.T, recipientID, notifType string) {
		t.Helper()
		var detailsRaw []byte
		if err := pool.QueryRow(ctx, `
			SELECT details FROM inbox_item WHERE recipient_id = $1 AND type = $2
			ORDER BY created_at DESC LIMIT 1
		`, recipientID, notifType).Scan(&detailsRaw); err != nil {
			t.Fatalf("load inbox_item details for %s/%s: %v", recipientID, notifType, err)
		}
		var details map[string]string
		if err := json.Unmarshal(detailsRaw, &details); err != nil {
			t.Fatalf("unmarshal inbox_item details: %v", err)
		}
		if details["project_id"] != fx.projectID {
			t.Fatalf("inbox_item[%s/%s].details.project_id = %q, want %q (TSUG-002)", recipientID, notifType, details["project_id"], fx.projectID)
		}
	}

	// 1. Request: fans out to both owners.
	if _, err := svc.RequestPresenter(ctx, project, util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("RequestPresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionRequested, 1)
	assertInboxCount(t, fx.owner1ID, "presenter_requested", 1)
	assertInboxCount(t, fx.owner2ID, "presenter_requested", 1)
	assertInboxProjectID(t, fx.owner1ID, "presenter_requested")

	// 2. Approve: notifies the requester.
	if _, err := svc.ApprovePresenter(ctx, project, util.MustParseUUID(fx.owner1ID), util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("ApprovePresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionApproved, 1)
	assertInboxCount(t, fx.memberAID, "presenter_approved", 1)

	// 3. Transfer: notifies the new presenter, not the outgoing one.
	if _, err := svc.TransferPresenter(ctx, project, util.MustParseUUID(fx.memberAID), util.MustParseUUID(fx.memberBID)); err != nil {
		t.Fatalf("TransferPresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionTransferred, 1)
	assertInboxCount(t, fx.memberBID, "presenter_transferred", 1)
	assertInboxCount(t, fx.memberAID, "presenter_transferred", 0)

	// 4. Revoke: notifies the just-revoked presenter (memberB).
	if _, err := svc.RevokePresenter(ctx, project, util.MustParseUUID(fx.owner1ID)); err != nil {
		t.Fatalf("RevokePresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionRevoked, 1)
	assertInboxCount(t, fx.memberBID, "presenter_revoked", 1)

	// 5. Reject: request again then reject, notifies the requester.
	if _, err := svc.RequestPresenter(ctx, project, util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("RequestPresenter (2nd): %v", err)
	}
	if _, err := svc.RejectPresenter(ctx, project, util.MustParseUUID(fx.owner1ID), util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("RejectPresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionRejected, 1)
	assertInboxCount(t, fx.memberAID, "presenter_rejected", 1)

	// 6. Release: memberA's prior request is now 'rejected' (terminal), so a
	// fresh request/approve cycle is needed before release.
	if _, err := svc.RequestPresenter(ctx, project, util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("RequestPresenter (3rd): %v", err)
	}
	if _, err := svc.ApprovePresenter(ctx, project, util.MustParseUUID(fx.owner1ID), util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("ApprovePresenter (2nd): %v", err)
	}
	inboxBefore := countAllInboxItems(t, ctx, pool, fx.workspaceID)
	if _, err := svc.ReleasePresenter(ctx, project, util.MustParseUUID(fx.memberAID)); err != nil {
		t.Fatalf("ReleasePresenter: %v", err)
	}
	assertActivity(t, service.PresenterActionReleased, 1)
	if got := countAllInboxItems(t, ctx, pool, fx.workspaceID); got != inboxBefore {
		t.Fatalf("release must not create an inbox_item: before=%d after=%d", inboxBefore, got)
	}

	// activity:created and project:presenter_changed each fire once per
	// transition: request, approve, transfer, revoke, request, reject,
	// request, approve, release = 9 transitions total.
	const wantTransitions = 9
	if activityEvents != wantTransitions {
		t.Fatalf("activity:created publish count = %d, want %d", activityEvents, wantTransitions)
	}
	if presenterChangedEvents != wantTransitions {
		t.Fatalf("project:presenter_changed publish count = %d, want %d", presenterChangedEvents, wantTransitions)
	}
}

type presenterNotificationFixture struct {
	workspaceID string
	projectID   string
	issueID     string
	owner1ID    string
	owner2ID    string
	memberAID   string
	memberBID   string
}

func createPresenterNotificationFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) presenterNotificationFixture {
	t.Helper()

	suffix := time.Now().UnixNano()
	slug := fmt.Sprintf("presenter-notif-%d", suffix)

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Presenter Notification Test", slug, "temporary CR-2026-010 TASK-04 test workspace", "PNT").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	makeUser := func(label string) string {
		email := fmt.Sprintf("presenter-notif-%s-%d@multica.ai", label, suffix)
		var userID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
		`, "Presenter Notif "+label, email).Scan(&userID); err != nil {
			t.Fatalf("create user %s: %v", label, err)
		}
		return userID
	}
	owner1ID := makeUser("owner1")
	owner2ID := makeUser("owner2")
	memberAID := makeUser("membera")
	memberBID := makeUser("memberb")

	for _, m := range []struct{ id, role string }{
		{owner1ID, "owner"}, {owner2ID, "owner"}, {memberAID, "member"}, {memberBID, "member"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)
		`, workspaceID, m.id, m.role); err != nil {
			t.Fatalf("create member %s: %v", m.role, err)
		}
	}

	var projectID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, workspaceID, "Presenter Notification Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_id, creator_type, number, position, origin_type)
		VALUES ($1, $2, 'Team Agent Chat', 'todo', 'medium', $3, 'member', $4, 0, 'project_chat')
		RETURNING id
	`, workspaceID, projectID, owner1ID, 940000+suffix%100000).Scan(&issueID); err != nil {
		t.Fatalf("create chat issue: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM inbox_item WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM activity_log WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project_presenter_grant WHERE project_id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM project WHERE id = $1`, projectID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		for _, uid := range []string{owner1ID, owner2ID, memberAID, memberBID} {
			pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, uid)
		}
	})

	return presenterNotificationFixture{
		workspaceID: workspaceID,
		projectID:   projectID,
		issueID:     issueID,
		owner1ID:    owner1ID,
		owner2ID:    owner2ID,
		memberAID:   memberAID,
		memberBID:   memberBID,
	}
}

func countAllInboxItems(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("count inbox_item: %v", err)
	}
	return n
}
