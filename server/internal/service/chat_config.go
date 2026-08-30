package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChatConfigSource enumerates where an effective chat-config value came from.
// The model and the thinking level resolve independently, so the two sources
// in a response may differ.
type ChatConfigSource string

const (
	// ChatConfigSourceOverride: value comes from the session's *_override
	// column (a PATCH write).
	ChatConfigSourceOverride ChatConfigSource = "override"
	// ChatConfigSourceSessionDefault: value comes from the base_* snapshot
	// written at session creation (an empty string is a valid snapshot value
	// meaning "follow the runtime default").
	ChatConfigSourceSessionDefault ChatConfigSource = "session_default"
	// ChatConfigSourceAgentDefault: legacy Private Ask rows have no base_*
	// snapshot; the effective value is the agent's CURRENT default, resolved
	// live and never written back (FR-11).
	ChatConfigSourceAgentDefault ChatConfigSource = "agent_default"
	// ChatConfigSourceRuntimeDefault: nothing else resolved; the empty string
	// is handed to the provider CLI so it picks its own default.
	ChatConfigSourceRuntimeDefault ChatConfigSource = "runtime_default"
)

// ResolvedChatConfig is the independently-resolved effective model and
// thinking level plus their sources.
type ResolvedChatConfig struct {
	Model              string
	ModelSource        ChatConfigSource
	ThinkingLevel      string
	ThinkingLevelSource ChatConfigSource
}

// ResolveChatConfig resolves the effective model and thinking level per
// SDD §4.2: override -> base_* (session snapshot) -> agent current default
// (legacy Private Ask only) -> runtime default. NULL means "not snapshotted /
// not set"; an empty string in base_* or an agent default is a legal value
// ("follow the runtime default") and resolves with its own source.
func ResolveChatConfig(
	baseModel, modelOverride, agentModel pgtype.Text,
	baseThinking, thinkingOverride, agentThinking pgtype.Text,
) ResolvedChatConfig {
	model, modelSource := resolveChatConfigValue(baseModel, modelOverride, agentModel)
	thinking, thinkingSource := resolveChatConfigValue(baseThinking, thinkingOverride, agentThinking)
	return ResolvedChatConfig{
		Model:               model,
		ModelSource:         modelSource,
		ThinkingLevel:       thinking,
		ThinkingLevelSource: thinkingSource,
	}
}

func resolveChatConfigValue(base, override, agentDefault pgtype.Text) (string, ChatConfigSource) {
	if override.Valid {
		return override.String, ChatConfigSourceOverride
	}
	if base.Valid {
		return base.String, ChatConfigSourceSessionDefault
	}
	if agentDefault.Valid {
		return agentDefault.String, ChatConfigSourceAgentDefault
	}
	return "", ChatConfigSourceRuntimeDefault
}

// ErrInvalidModelOrThinkingLevel is the single error the four validation entry
// points (PATCH / messages / container / merge-forward) return when the
// resolved config is not valid or the catalog could not be loaded; handlers
// map it to 400 invalid_model_or_thinking_level.
var ErrInvalidModelOrThinkingLevel = errors.New("invalid model or thinking level")

// ValidateResolvedChatConfig is the service-side thin wrapper around the
// single domain implementation (agent.ValidateChatConfig, BLOCK-014). It must
// stay a pure forward: no normalization, no provider aliases, no second rule
// set. The service must never import internal/handler; catalogs arrive here
// already adapted to agent.Catalog.
func ValidateResolvedChatConfig(model, thinking, provider string, catalog agent.Catalog) error {
	ok, err := agent.ValidateChatConfig(catalog, provider, model, thinking)
	if err != nil || !ok {
		return ErrInvalidModelOrThinkingLevel
	}
	return nil
}

// ChatCatalogPort is owned by the service and implemented by the handler
// (SDD §4.3). CacheLoad returns a usable last-known-good catalog
// (cacheable && within the 24h serve window); ok=false means no usable
// snapshot, never a fallback stand-in. LiveLoad performs exactly ONE
// synchronous discovery round bounded by the caller's deadline (30s, the
// handler's modelListPendingTimeout); it must not return a pending state to
// its caller.
type ChatCatalogPort interface {
	CacheLoad(ctx context.Context, runtimeID string) (cat agent.Catalog, ok bool, err error)
	LiveLoad(ctx context.Context, runtimeID string) (agent.Catalog, error)
}

// chatConfigLiveLoadTimeout bounds the single synchronous discovery round.
// It mirrors handler.modelListPendingTimeout (30s); the handler adapter also
// enforces that same pending timeout on the store record, so the two agree.
const chatConfigLiveLoadTimeout = 30 * time.Second

// LoadChatCatalogForConfig is the one synchronous catalog decision shared by
// the four validation entry points (SDD §4.3):
//
//   - Blocked agent: reject (no override write, no issue, no enqueue).
//   - Waitable agent: cache only; LiveLoad is forbidden (no daemon round
//     trip can complete offline). Missing snapshot rejects.
//   - Available agent: cache fast path; a miss runs ONE LiveLoad round
//     under the 30s deadline; a non-cacheable result (fallback stand-in or
//     empty list) rejects.
//
// Every rejection returns ErrInvalidModelOrThinkingLevel, which the handler
// maps to 400 invalid_model_or_thinking_level.
func LoadChatCatalogForConfig(ctx context.Context, q *db.Queries, port ChatCatalogPort, agentRow db.Agent) (agent.Catalog, error) {
	verdict, err := AgentReadiness(ctx, q, agentRow)
	if err != nil {
		return agent.Catalog{}, err
	}
	runtimeID := ""
	if agentRow.RuntimeID.Valid {
		runtimeID = util.UUIDToString(agentRow.RuntimeID)
	}
	return loadChatCatalogForVerdict(ctx, port, runtimeID, verdict)
}

// loadChatCatalogForVerdict is the verdict-half of the §4.3 decision, split
// out so every branch is testable without a database.
func loadChatCatalogForVerdict(ctx context.Context, port ChatCatalogPort, runtimeID string, verdict AgentVerdict) (agent.Catalog, error) {
	if verdict.Blocked() {
		return agent.Catalog{}, ErrInvalidModelOrThinkingLevel
	}
	if !verdict.Ready() {
		// Waitable: offline is a plan, but no synchronous round trip can
		// complete — the cached last-known-good is the only acceptable
		// source (SDD §4.3).
		cat, ok, err := port.CacheLoad(ctx, runtimeID)
		if err != nil || !ok {
			return agent.Catalog{}, ErrInvalidModelOrThinkingLevel
		}
		return cat, nil
	}
	if cat, ok, _ := port.CacheLoad(ctx, runtimeID); ok {
		return cat, nil
	}
	liveCtx, cancel := context.WithTimeout(ctx, chatConfigLiveLoadTimeout)
	defer cancel()
	cat, err := port.LiveLoad(liveCtx, runtimeID)
	if err != nil {
		return agent.Catalog{}, ErrInvalidModelOrThinkingLevel
	}
	if cat.Fallback || len(cat.Models) == 0 {
		return agent.Catalog{}, ErrInvalidModelOrThinkingLevel
	}
	return cat, nil
}
