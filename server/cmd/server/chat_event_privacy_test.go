package main

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestChatEventFailsClosedWithoutRecipient pins the fail-closed half of
// CR-2026-008's privacy contract (the happy path is covered by
// TestRegisterListeners_TaskChatGoToWorkspace): an event that carries a
// ChatSessionID but no ChatRecipientID must be dropped entirely — a missed
// event self-heals through invalidate/refetch, a leaked private payload
// cannot be unsent.
func TestChatEventFailsClosedWithoutRecipient(t *testing.T) {
	bus := events.New()
	fb := &fakeBroadcaster{}
	registerListeners(bus, fb)

	bus.Publish(events.Event{
		Type:          protocol.EventChatDone,
		WorkspaceID:   "ws-1",
		ActorType:     "system",
		ChatSessionID: "sess-orphan",
		Payload:       map[string]any{"content": "orphaned"},
	})

	if len(fb.workspaceCalls) != 0 || len(fb.userCalls) != 0 || len(fb.scopeCalls) != 0 || fb.broadcastCalled != 0 {
		t.Fatalf("recipient-less chat event was delivered: ws=%+v users=%+v scope=%+v broadcast=%d",
			fb.workspaceCalls, fb.userCalls, fb.scopeCalls, fb.broadcastCalled)
	}
}

// TestChatEventSharedKindRoutesToWorkspace pins the CR-2026-059 §3.7 routing
// contract: a project_shared session event fans out to the workspace room;
// private/empty kinds keep the creator-only recipient path; a shared event
// without a workspace hint is dropped (fail-closed toward the narrower
// delivery).
func TestChatEventSharedKindRoutesToWorkspace(t *testing.T) {
	bus := events.New()
	fb := &fakeBroadcaster{}
	registerListeners(bus, fb)

	bus.Publish(events.Event{
		Type:            protocol.EventChatMessage,
		WorkspaceID:     "ws-1",
		ActorType:       "member",
		ChatSessionID:   "sess-shared",
		ChatRecipientID: "creator-1",
		ChatSessionKind: "project_shared",
		Payload:         map[string]any{"content": "visible to the team"},
	})
	if len(fb.workspaceCalls) != 1 || fb.workspaceCalls[0].workspaceID != "ws-1" {
		t.Fatalf("shared chat event did not reach the workspace room: %+v", fb.workspaceCalls)
	}
	if len(fb.userCalls) != 0 {
		t.Fatalf("shared chat event must not use the per-user path: %+v", fb.userCalls)
	}

	bus.Publish(events.Event{
		Type:            protocol.EventChatMessage,
		WorkspaceID:     "ws-1",
		ActorType:       "member",
		ChatSessionID:   "sess-private",
		ChatRecipientID: "creator-1",
		ChatSessionKind: "private",
		Payload:         map[string]any{"content": "private"},
	})
	if len(fb.userCalls) != 1 || fb.userCalls[0].userID != "creator-1" {
		t.Fatalf("private chat event lost the recipient path: %+v", fb.userCalls)
	}
	if len(fb.workspaceCalls) != 1 {
		t.Fatalf("private chat event leaked to the workspace room: %+v", fb.workspaceCalls)
	}

	// Fail-closed: shared kind without a workspace hint is dropped entirely.
	bus.Publish(events.Event{
		Type:            protocol.EventChatMessage,
		ActorType:       "member",
		ChatSessionID:   "sess-shared-orphan",
		ChatRecipientID: "creator-1",
		ChatSessionKind: "project_shared",
		Payload:         map[string]any{"content": "where does this go?"},
	})
	if len(fb.workspaceCalls) != 1 || len(fb.userCalls) != 1 {
		t.Fatalf("workspace-less shared event was delivered: ws=%+v users=%+v", fb.workspaceCalls, fb.userCalls)
	}
}
