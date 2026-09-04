package realtime

import (
	"testing"
	"time"
)

// CR-2026-059 §4.7 (AC-29): server-side member disconnect + the reserved
// control envelope. These hub-level tests prove the local disconnect, the
// room cleanup and the never-fanout control branch without a Redis backend.

func newRegisteredTestClient(hub *Hub, userID, workspaceID string) *Client {
	c := &Client{
		hub:           hub,
		send:          make(chan []byte, 16),
		userID:        userID,
		workspaceID:   workspaceID,
		subscriptions: make(map[scopeKey]bool),
	}
	hub.register <- c
	// Give the Run loop a tick to auto-subscribe and register the client.
	time.Sleep(10 * time.Millisecond)
	return c
}

func clientConnected(c *Client) bool {
	select {
	case _, open := <-c.send:
		if !open {
			return false
		}
		// A real delivery; put it back for the caller's assertions.
		return true
	default:
	}
	c.hub.mu.RLock()
	defer c.hub.mu.RUnlock()
	return c.hub.clients[c]
}

func TestDisconnectWorkspaceUserClosesOnlyMatchingConnections(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	uA := newRegisteredTestClient(hub, "user-u", "ws-1")
	uB := newRegisteredTestClient(hub, "user-u", "ws-1")
	uOtherWS := newRegisteredTestClient(hub, "user-u", "ws-2")
	memberB := newRegisteredTestClient(hub, "user-b", "ws-1")

	hub.DisconnectWorkspaceUser("user-u", "ws-1")

	// The two matching sockets are closed (channel close = removeClient).
	waitClosed := func(c *Client) bool {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			select {
			case _, open := <-c.send:
				if !open {
					return true
				}
			default:
				time.Sleep(2 * time.Millisecond)
			}
		}
		return false
	}
	if !waitClosed(uA) || !waitClosed(uB) {
		t.Fatal("matching (user, workspace) connections were not closed")
	}
	if !clientConnected(uOtherWS) {
		t.Fatal("the same user's connection in another workspace must survive")
	}
	if !clientConnected(memberB) {
		t.Fatal("another member's connection in the same workspace must survive")
	}

	// Room cleanup: the user room survives because the user still holds a
	// connection in ws-2; the ws-1 workspace room survives for member B.
	// What must be gone are the two disconnected clients themselves.
	hub.mu.RLock()
	uClients := len(hub.rooms[sk(ScopeUser, "user-u")])
	bRoom := len(hub.rooms[sk(ScopeUser, "user-b")])
	wsRoom := len(hub.rooms[sk(ScopeWorkspace, "ws-1")])
	total := len(hub.clients)
	hub.mu.RUnlock()
	if uClients != 1 {
		t.Fatalf("user-u room holds %d client(s), want 1 (the ws-2 connection)", uClients)
	}
	if bRoom != 1 {
		t.Fatalf("user-b room holds %d client(s), want 1", bRoom)
	}
	if wsRoom != 1 {
		t.Fatalf("ws-1 workspace room holds %d client(s), want 1 (member B)", wsRoom)
	}
	if total != 2 {
		t.Fatalf("hub tracks %d client(s), want 2", total)
	}
}

func TestControlFrameDisconnectsAndNeverFansOut(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	client := newRegisteredTestClient(hub, "user-u", "ws-1")

	// deliverEnvelope routes the reserved type to the control branch: the
	// matching socket closes and no frame is delivered to it beforehand.
	frame := NewDisconnectWorkspaceControlFrame("ws-1")
	deliverEnvelope(hub, nil, envelope{
		EventType:   ControlFrameType,
		Scope:       ScopeUser,
		ScopeID:     "user-u",
		PayloadJSON: string(frame),
	})

	deadline := time.Now().Add(time.Second)
	closed := false
	delivered := 0
	for time.Now().Before(deadline) {
		select {
		case raw, open := <-client.send:
			if !open {
				closed = true
				break
			}
			delivered++
			_ = raw
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !closed {
		t.Fatal("control frame did not disconnect the matching socket")
	}
	if delivered != 0 {
		t.Fatalf("control frame fanned out %d frame(s) to the client, want 0", delivered)
	}

	// Malformed control frames are dropped, never fanned out.
	other := newRegisteredTestClient(hub, "user-v", "ws-1")
	deliverEnvelope(hub, nil, envelope{
		EventType:   ControlFrameType,
		Scope:       ScopeUser,
		ScopeID:     "user-v",
		PayloadJSON: `{"type":"realtime.control","action":"no_such_action"}`,
	})
	if !clientConnected(other) {
		t.Fatal("unknown control action must not disconnect clients")
	}
}

func TestHandleControlFrameParsesEnvelope(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	client := newRegisteredTestClient(hub, "user-u", "ws-1")
	hub.HandleControlFrame("user-u", NewDisconnectWorkspaceControlFrame("ws-1"))
	deadline := time.Now().Add(time.Second)
	closed := false
	for time.Now().Before(deadline) {
		select {
		case _, open := <-client.send:
			if !open {
				closed = true
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !closed {
		t.Fatal("HandleControlFrame did not disconnect the matching socket")
	}
}
