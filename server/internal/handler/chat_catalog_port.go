package handler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ChatCatalogPort implements service.ChatCatalogPort over the handler's model
// catalog cache and pending-request store (CR-2026-056, SDD §4.3). It is the
// only place where handler-side wire shapes (ModelEntry, ModelListRequest)
// are adapted into agent.Catalog; the service never imports internal/handler.
//
//   - CacheLoad serves the last-known-good snapshot (cacheable && within the
//     24h serve window). ok=false means no usable snapshot — never a fallback
//     stand-in, never an empty list.
//   - LiveLoad enqueues ONE pending model-list request, nudges the daemon,
//     and blocks until the store record reaches a terminal state or the
//     caller's deadline (30s, modelListPendingTimeout) expires. It must not
//     return a pending state to its caller.
type ChatCatalogPort struct {
	cache  ModelCatalogCache
	store  ModelListStore
	notify func(runtimeID string)
}

// NewChatCatalogPort wires a catalog port over the given cache/store. notify
// nudges the daemon to heartbeat now after a pending request is enqueued;
// nil disables the nudge (best-effort by design — the scheduled heartbeat
// remains the correctness path).
func NewChatCatalogPort(cache ModelCatalogCache, store ModelListStore, notify func(runtimeID string)) *ChatCatalogPort {
	return &ChatCatalogPort{cache: cache, store: store, notify: notify}
}

func (p *ChatCatalogPort) CacheLoad(ctx context.Context, runtimeID string) (agent.Catalog, bool, error) {
	if p == nil || p.cache == nil || runtimeID == "" {
		return agent.Catalog{}, false, nil
	}
	snapshot, err := p.cache.Get(ctx, runtimeID)
	if err != nil {
		slog.Warn("chat config catalog cache read failed", "error", err, "runtime_id", runtimeID)
		return agent.Catalog{}, false, err
	}
	// A stored snapshot is by construction a real discovery result —
	// fallback catalogs never enter the cache (MUL-5549).
	if snapshot == nil || !cacheableModelCatalog(snapshot.Models, snapshot.Supported, false) {
		return agent.Catalog{}, false, nil
	}
	if snapshot.Age(time.Now()) >= modelCatalogServeWindow {
		return agent.Catalog{}, false, nil
	}
	return modelEntriesToCatalog(snapshot.Models), true, nil
}

func (p *ChatCatalogPort) LiveLoad(ctx context.Context, runtimeID string) (agent.Catalog, error) {
	if p == nil || p.store == nil || runtimeID == "" {
		return agent.Catalog{}, fmt.Errorf("model list store unavailable")
	}
	req, err := p.store.Create(ctx, runtimeID)
	if err != nil {
		return agent.Catalog{}, fmt.Errorf("enqueue model list request: %w", err)
	}
	if p.notify != nil {
		p.notify(runtimeID)
	}

	// One synchronous round: poll until the record is terminal or the
	// caller's deadline expires. The store's own pending timeout (30s)
	// transitions the record to timeout if the daemon never claims it.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := p.store.Get(ctx, req.ID)
		if err != nil {
			return agent.Catalog{}, fmt.Errorf("read model list request: %w", err)
		}
		if got == nil {
			return agent.Catalog{}, fmt.Errorf("model list request vanished")
		}
		switch got.Status {
		case ModelListCompleted:
			if !got.Supported {
				return agent.Catalog{}, fmt.Errorf("runtime does not support per-agent model selection")
			}
			// Fallback stand-ins are a failed discovery wearing a completed
			// label: never validate a config against them (SDD §4.3).
			if !cacheableModelCatalog(got.Models, got.Supported, got.Fallback) {
				return agent.Catalog{}, fmt.Errorf("non-cacheable model catalog (fallback or empty)")
			}
			// Warm the cache so the next validation hits the fast path
			// (handler-side Put after a successful live round, SDD §4.3).
			if p.cache != nil {
				if err := p.cache.Put(ctx, runtimeID, got.Models, got.Supported); err != nil {
					slog.Warn("chat config catalog cache write failed", "error", err, "runtime_id", runtimeID)
				}
			}
			return modelEntriesToCatalog(got.Models), nil
		case ModelListFailed, ModelListTimeout:
			reason := got.Error
			if reason == "" {
				reason = string(got.Status)
			}
			return agent.Catalog{}, fmt.Errorf("model discovery %s: %s", got.Status, reason)
		}
		select {
		case <-ctx.Done():
			return agent.Catalog{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// modelEntriesToCatalog adapts the handler's wire shape to the domain
// catalog. It is deliberately the only place that performs this conversion.
func modelEntriesToCatalog(models []ModelEntry) agent.Catalog {
	out := make([]agent.Model, 0, len(models))
	for _, m := range models {
		am := agent.Model{
			ID:       m.ID,
			Label:    m.Label,
			Provider: m.Provider,
			Default:  m.Default,
		}
		if m.Thinking != nil {
			am.Thinking = &agent.ModelThinking{
				DefaultLevel:    m.Thinking.DefaultLevel,
				SupportedLevels: make([]agent.ThinkingLevel, 0, len(m.Thinking.SupportedLevels)),
			}
			for _, l := range m.Thinking.SupportedLevels {
				am.Thinking.SupportedLevels = append(am.Thinking.SupportedLevels, agent.ThinkingLevel{
					Value:       l.Value,
					Label:       l.Label,
					Description: l.Description,
				})
			}
		}
		if m.ServiceTiers != nil {
			am.ServiceTiers = make([]agent.ModelServiceTier, 0, len(m.ServiceTiers))
			for _, t := range m.ServiceTiers {
				am.ServiceTiers = append(am.ServiceTiers, agent.ModelServiceTier{
					ID:          t.ID,
					Name:        t.Name,
					Description: t.Description,
				})
			}
		}
		out = append(out, am)
	}
	return agent.Catalog{Models: out}
}

// WireChatCatalog connects the handler-implemented catalog port to the
// services that validate chat configs (CR-2026-056). Called once from
// cmd/server/router.go after the model list store and catalog cache are
// finalized (in-memory or Redis).
func (h *Handler) WireChatCatalog() {
	port := NewChatCatalogPort(h.ModelCatalogCache, h.ModelListStore, func(runtimeID string) {
		h.requestDaemonPendingWork(runtimeID, protocol.PendingWorkKindModelList)
	})
	h.ChatCatalog = port
	h.IssueService.ChatCatalog = port
	h.TaskService.ChatCatalog = port
}
