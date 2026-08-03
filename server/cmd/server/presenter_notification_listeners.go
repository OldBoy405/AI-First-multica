package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// presenterNotifTypes maps each presenter activity action (CR-2026-010,
// service.PresenterAction* constants) to its inbox notification type. There
// is no "presenter_released" entry: release has no directed recipient (SDD
// §4.5) — every project member sees it from the activity card alone.
var presenterNotifTypes = map[string]bool{
	"presenter_requested":   true,
	"presenter_approved":    true,
	"presenter_rejected":    true,
	"presenter_transferred": true,
	"presenter_revoked":     true,
}

// registerPresenterNotificationListeners dispatches inbox notifications for
// CR-2026-010 presenter transitions. It subscribes to the same activity:created
// event registerActivityListeners' siblings use, filtered to the presenter_*
// actions TaskService.recordPresenterActivity stamps.
//
// notifyDirect lives in this package (it needs queries + bus + preference
// lookups), but the presenter service (internal/service, which cannot import
// package main) is the one that knows WHO to notify for each transition — it
// passes that as a "presenter_notify" list of recipient user ids in the
// event payload. This listener is purely a dispatch shim over that list.
func registerPresenterNotificationListeners(bus *events.Bus, queries *db.Queries) {
	bus.Subscribe(protocol.EventActivityCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		entry, ok := payload["entry"].(map[string]any)
		if !ok {
			return
		}
		action, _ := entry["action"].(string)
		if !presenterNotifTypes[action] {
			return
		}

		issueID, _ := payload["issue_id"].(string)
		recipients, _ := payload["presenter_notify"].([]string)
		if issueID == "" || len(recipients) == 0 {
			return
		}
		detailsJSON, _ := entry["details"].(json.RawMessage)

		ctx := context.Background()
		issue, err := queries.GetIssue(ctx, parseUUID(issueID))
		if err != nil {
			slog.Warn("presenter notification skipped: issue lookup failed", "issue_id", issueID, "action", action, "error", err)
			return
		}

		for _, recipientID := range recipients {
			notifyDirect(ctx, queries, bus,
				"member", recipientID,
				e.WorkspaceID, e, issueID, issue.Status,
				action, "info",
				issue.Title, "",
				detailsJSON,
			)
		}
	})
}
