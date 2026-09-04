package events

import (
	"log/slog"
	"sync"
)

// Event represents a domain event published by handlers or services.
type Event struct {
	Type        string // e.g. "issue:created", "inbox:new"
	WorkspaceID string // routes to correct Hub room
	ActorType   string // "member", "agent", or "system"
	ActorID     string
	Payload     any // JSON-serializable, same shape as current WS payloads

	// Optional scope hints used by the realtime fanout layer to route the
	// event to a more specific scope than `workspace:{WorkspaceID}`. When set
	// these tell the listener which Redis stream / Hub room to publish on
	// without re-deserializing Payload. See MUL-1138 phase 1.
	TaskID        string
	ChatSessionID string

	// ChatRecipientID is the chat session creator's user id. Chat sessions
	// are creator-private (the HTTP layer and ScopeAuthorizer both enforce
	// creator-only access), so the WS bridge delivers every event that
	// carries a ChatSessionID exclusively to this user via SendToUser —
	// never on the workspace fanout (CR-2026-008). Producers that set
	// ChatSessionID MUST set this too; the bridge fails closed (drops the
	// event with an ERROR log) rather than fall back to broadcasting.
	ChatRecipientID string

	// ChatSessionKind mirrors chat_session.kind at production time
	// (CR-2026-059 §3.7). Realtime routing uses it to fan shared-session
	// events out to the workspace room. Producers MUST set it together with
	// ChatSessionID; the bridge treats an empty value as private (fail-closed
	// toward the narrower delivery).
	ChatSessionKind string
}

// Handler is a function that processes an event.
type Handler func(Event)

// Bus is an in-process synchronous pub/sub event bus.
type Bus struct {
	mu             sync.RWMutex
	listeners      map[string][]Handler
	globalHandlers []Handler
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{
		listeners: make(map[string][]Handler),
	}
}

// Subscribe registers a handler for a given event type.
// Handlers are called synchronously in registration order.
func (b *Bus) Subscribe(eventType string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listeners[eventType] = append(b.listeners[eventType], h)
}

// SubscribeAll registers a handler that receives ALL events regardless of type.
// Global handlers are called after type-specific handlers.
func (b *Bus) SubscribeAll(h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.globalHandlers = append(b.globalHandlers, h)
}

// Publish dispatches an event to all registered handlers for that event type.
// Type-specific handlers run first, then global (SubscribeAll) handlers.
// Each handler is called synchronously. Panics in individual handlers are
// recovered so one failing handler does not prevent others from executing.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	handlers := b.listeners[e.Type]
	globals := b.globalHandlers
	b.mu.RUnlock()

	for _, h := range handlers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in event listener", "event_type", e.Type, "recovered", r)
				}
			}()
			h(e)
		}()
	}

	for _, h := range globals {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in global event listener", "event_type", e.Type, "recovered", r)
				}
			}()
			h(e)
		}()
	}
}
