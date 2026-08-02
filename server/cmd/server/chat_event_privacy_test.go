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
