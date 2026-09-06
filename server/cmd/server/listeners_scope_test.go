package main

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeBroadcaster records every fanout call so tests can assert which scope a
// given event landed on.
type fakeBroadcaster struct {
	mu              sync.Mutex
	scopeCalls      []scopeCall
	workspaceCalls  []workspaceCall
	userCalls       []userCall
	broadcastCalled int
}

// DisconnectWorkspaceUser records the CR-2026-059 control-frame call; the
// listener tests only assert fanout routing, so nothing is recorded here.
func (f *fakeBroadcaster) DisconnectWorkspaceUser(userID, workspaceID string) {}

func TestRegisterListeners_ChatSessionCreatedGoesOnlyToCreator(t *testing.T) {
	bus := events.New()
	fb := &fakeBroadcaster{}
	registerListeners(bus, fb)

	bus.Publish(events.Event{
		Type: protocol.EventChatSessionCreated, WorkspaceID: "ws-1",
		ActorType: "member", ActorID: "creator-1", ChatSessionID: "chat-1",
		Payload: protocol.ChatSessionCreatedPayload{
			WorkspaceID: "ws-1", ChatSessionID: "chat-1", CreatorID: "creator-1",
			Title: "private opening title",
		},
	})

	if len(fb.workspaceCalls) != 0 {
		t.Fatalf("private Chat create reached workspace fanout: %+v", fb.workspaceCalls)
	}
	if len(fb.userCalls) != 1 || fb.userCalls[0].userID != "creator-1" {
		t.Fatalf("creator fanout = %+v, want creator-1 once", fb.userCalls)
	}
	var frame struct {
		Payload protocol.ChatSessionCreatedPayload `json:"payload"`
	}
	if err := json.Unmarshal(fb.userCalls[0].msg, &frame); err != nil {
		t.Fatalf("decode creator frame: %v", err)
	}
	if frame.Payload.WorkspaceID != "ws-1" || frame.Payload.ChatSessionID != "chat-1" {
		t.Fatalf("creator payload = %+v", frame.Payload)
	}
}

func TestRegisterListeners_ChatSessionTitleUpdateGoesOnlyToCreator(t *testing.T) {
	bus := events.New()
	fb := &fakeBroadcaster{}
	registerListeners(bus, fb)

	bus.Publish(events.Event{
		Type: protocol.EventChatSessionUpdated, WorkspaceID: "ws-1",
		ActorType: "member", ActorID: "creator-1", ChatSessionID: "chat-1",
		Payload: protocol.ChatSessionUpdatedPayload{
			ChatSessionID: "chat-1", Title: "private derived title",
		},
	})

	if len(fb.workspaceCalls) != 0 {
		t.Fatalf("private Chat title reached workspace fanout: %+v", fb.workspaceCalls)
	}
	if len(fb.userCalls) != 1 || fb.userCalls[0].userID != "creator-1" {
		t.Fatalf("creator fanout = %+v, want creator-1 once", fb.userCalls)
	}
}

type scopeCall struct {
	scopeType, scopeID string
	msg                []byte
}
type workspaceCall struct {
	workspaceID string
	msg         []byte
}
type userCall struct {
	userID  string
	msg     []byte
	exclude []string
}

func (f *fakeBroadcaster) BroadcastToScope(scopeType, scopeID string, message []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopeCalls = append(f.scopeCalls, scopeCall{scopeType, scopeID, message})
}
func (f *fakeBroadcaster) BroadcastToWorkspace(workspaceID string, message []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaceCalls = append(f.workspaceCalls, workspaceCall{workspaceID, message})
}
func (f *fakeBroadcaster) SendToUser(userID string, message []byte, excludeWorkspace ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls = append(f.userCalls, userCall{userID, message, excludeWorkspace})
}
func (f *fakeBroadcaster) Broadcast(message []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastCalled++
}

// TestRegisterListeners_TaskChatGoToWorkspace pins two contracts:
//
//   - must-fix #1 from the PR #1429 review: until the WS client supports
//     scope-subscribe and reconnect-replay, high-frequency TASK events keep
//     going through workspace fanout — BroadcastToScope with no client-side
//     subscriber would silently drop them.
//   - CR-2026-008: events bound to a chat session are creator-private and go
//     to the session creator via SendToUser, never the workspace fanout.
func TestRegisterListeners_TaskChatGoToWorkspace(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		taskID    string
		chatID    string
		recipient string
	}{
		{"task:message with TaskID", protocol.EventTaskMessage, "task-1", "", ""},
		{"task:progress with TaskID", protocol.EventTaskProgress, "task-2", "", ""},
		{"chat:message with ChatSessionID", protocol.EventChatMessage, "", "chat-1", "user-1"},
		{"chat:done with ChatSessionID", protocol.EventChatDone, "", "chat-2", "user-1"},
		{"chat:session_read with ChatSessionID", protocol.EventChatSessionRead, "", "chat-3", "user-1"},
		{"chat-task task:message with both hints", protocol.EventTaskMessage, "task-3", "chat-4", "user-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := events.New()
			fb := &fakeBroadcaster{}
			registerListeners(bus, fb)

			bus.Publish(events.Event{
				Type:            tc.eventType,
				WorkspaceID:     "ws-1",
				TaskID:          tc.taskID,
				ChatSessionID:   tc.chatID,
				ChatRecipientID: tc.recipient,
				Payload:         map[string]any{"hello": "world"},
			})

			if len(fb.scopeCalls) != 0 {
				t.Fatalf("expected no BroadcastToScope calls (must-fix #1: keep workspace fanout until client lands), got %+v", fb.scopeCalls)
			}
			if tc.chatID != "" {
				// Creator-private delivery: SendToUser only (CR-2026-008).
				if len(fb.workspaceCalls) != 0 {
					t.Fatalf("chat event leaked to workspace fanout: %+v", fb.workspaceCalls)
				}
				if len(fb.userCalls) != 1 || fb.userCalls[0].userID != tc.recipient {
					t.Fatalf("expected exactly 1 SendToUser(%q), got %+v", tc.recipient, fb.userCalls)
				}
				return
			}
			if len(fb.workspaceCalls) != 1 {
				t.Fatalf("expected exactly 1 BroadcastToWorkspace call, got %d", len(fb.workspaceCalls))
			}
			if fb.workspaceCalls[0].workspaceID != "ws-1" {
				t.Fatalf("expected workspace ws-1, got %q", fb.workspaceCalls[0].workspaceID)
			}
		})
	}
}
