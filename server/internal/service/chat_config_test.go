package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func text(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{String: "", Valid: true}
	}
	return pgtype.Text{String: v, Valid: true}
}

func nullText() pgtype.Text { return pgtype.Text{} }

// TestResolveChatConfigPriority pins the SDD §4.2 resolution ladder for both
// the model and the thinking level independently (sources may differ).
func TestResolveChatConfigPriority(t *testing.T) {
	t.Parallel()

	t.Run("model and thinking resolve independently", func(t *testing.T) {
		t.Parallel()
		got := ResolveChatConfig(
			text("base-model"), text("override-model"), text("agent-model"),
			nullText(), nullText(), text("agent-thinking"),
		)
		if got.Model != "override-model" || got.ModelSource != ChatConfigSourceOverride {
			t.Fatalf("model = %q (%s), want override-model (override)", got.Model, got.ModelSource)
		}
		if got.ThinkingLevel != "agent-thinking" || got.ThinkingLevelSource != ChatConfigSourceAgentDefault {
			t.Fatalf("thinking = %q (%s), want agent-thinking (agent_default)", got.ThinkingLevel, got.ThinkingLevelSource)
		}
	})

	cases := []struct {
		name   string
		base   pgtype.Text
		over   pgtype.Text
		agent  pgtype.Text
		value  string
		source ChatConfigSource
	}{
		{name: "override wins", base: text("b"), over: text("o"), agent: text("a"), value: "o", source: ChatConfigSourceOverride},
		{name: "base when no override", base: text("b"), over: nullText(), agent: text("a"), value: "b", source: ChatConfigSourceSessionDefault},
		{name: "empty base is a legal snapshot", base: text(""), over: nullText(), agent: text("a"), value: "", source: ChatConfigSourceSessionDefault},
		{name: "agent default for legacy rows", base: nullText(), over: nullText(), agent: text("a"), value: "a", source: ChatConfigSourceAgentDefault},
		{name: "empty agent default is legal", base: nullText(), over: nullText(), agent: text(""), value: "", source: ChatConfigSourceAgentDefault},
		{name: "runtime default last", base: nullText(), over: nullText(), agent: nullText(), value: "", source: ChatConfigSourceRuntimeDefault},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveChatConfig(tc.base, tc.over, tc.agent, tc.base, tc.over, tc.agent)
			if got.Model != tc.value || got.ModelSource != tc.source {
				t.Fatalf("model = %q (%s), want %q (%s)", got.Model, got.ModelSource, tc.value, tc.source)
			}
			if got.ThinkingLevel != tc.value || got.ThinkingLevelSource != tc.source {
				t.Fatalf("thinking = %q (%s), want %q (%s)", got.ThinkingLevel, got.ThinkingLevelSource, tc.value, tc.source)
			}
		})
	}
}

// TestValidateResolvedChatConfigForwards pins the thin-wrapper contract: the
// only rule set is agent.ValidateChatConfig (BLOCK-014).
func TestValidateResolvedChatConfigForwards(t *testing.T) {
	t.Parallel()
	catalog := agent.Catalog{Models: []agent.Model{
		{ID: "claude-opus-5", Default: true, Thinking: &agent.ModelThinking{
			SupportedLevels: []agent.ThinkingLevel{{Value: "high"}},
		}},
	}}
	if err := ValidateResolvedChatConfig("claude-opus-5", "high", "claude", catalog); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := ValidateResolvedChatConfig("claude-madeup-9", "high", "claude", catalog); !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("unknown model: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	if err := ValidateResolvedChatConfig("", "high", "codex", catalog); !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("codex empty model + non-empty thinking: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	// Empty model + empty thinking is the legal runtime-default sentinel.
	if err := ValidateResolvedChatConfig("", "", "codex", catalog); err != nil {
		t.Fatalf("empty sentinel rejected: %v", err)
	}
}

// fakeChatCatalogPort is a scripted ChatCatalogPort for the §4.3 decision
// tests; it records whether LiveLoad was ever reached.
type fakeChatCatalogPort struct {
	cacheResult agent.Catalog
	cacheOK     bool
	cacheErr    error
	liveResult  agent.Catalog
	liveErr     error
	liveCalls   int
}

func (f *fakeChatCatalogPort) CacheLoad(_ context.Context, _ string) (agent.Catalog, bool, error) {
	return f.cacheResult, f.cacheOK, f.cacheErr
}

func (f *fakeChatCatalogPort) LiveLoad(_ context.Context, _ string) (agent.Catalog, error) {
	f.liveCalls++
	return f.liveResult, f.liveErr
}

func sampleCatalog() agent.Catalog {
	return agent.Catalog{Models: []agent.Model{{ID: "claude-opus-5", Default: true}}}
}

// TestLoadChatCatalogForVerdict pins the synchronous catalog decision table
// (SDD §4.3): blocked rejects; waitable uses cache only; available takes the
// cache fast path and falls back to exactly one live round; non-cacheable
// live results reject.
func TestLoadChatCatalogForVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("blocked rejects without touching the port", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheResult: sampleCatalog(), cacheOK: true}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentBlocked})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("blocked: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
		if port.liveCalls != 0 {
			t.Fatalf("blocked verdict must not call LiveLoad (calls=%d)", port.liveCalls)
		}
	})

	t.Run("waitable cache hit succeeds", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheResult: sampleCatalog(), cacheOK: true}
		cat, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentWaitable})
		if err != nil || len(cat.Models) != 1 {
			t.Fatalf("waitable cache hit: %v, %+v", err, cat)
		}
		if port.liveCalls != 0 {
			t.Fatalf("waitable verdict must not call LiveLoad (calls=%d)", port.liveCalls)
		}
	})

	t.Run("waitable cache miss rejects without LiveLoad", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheResult: sampleCatalog(), cacheOK: false}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentWaitable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("waitable cache miss: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
		if port.liveCalls != 0 {
			t.Fatalf("waitable verdict must not call LiveLoad (calls=%d)", port.liveCalls)
		}
	})

	t.Run("waitable cache error rejects without LiveLoad", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheErr: errors.New("redis down")}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentWaitable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("waitable cache error: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
		if port.liveCalls != 0 {
			t.Fatalf("waitable verdict must not call LiveLoad (calls=%d)", port.liveCalls)
		}
	})

	t.Run("available cache fast path", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheResult: sampleCatalog(), cacheOK: true}
		cat, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if err != nil || len(cat.Models) != 1 {
			t.Fatalf("available cache hit: %v, %+v", err, cat)
		}
		if port.liveCalls != 0 {
			t.Fatalf("cache hit must not call LiveLoad (calls=%d)", port.liveCalls)
		}
	})

	t.Run("available live round succeeds", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheOK: false, liveResult: sampleCatalog()}
		cat, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if err != nil || len(cat.Models) != 1 {
			t.Fatalf("available live round: %v, %+v", err, cat)
		}
		if port.liveCalls != 1 {
			t.Fatalf("live round must run exactly once (calls=%d)", port.liveCalls)
		}
	})

	t.Run("live load error rejects", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheOK: false, liveErr: errors.New("daemon timeout")}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("live error: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
	})

	t.Run("live empty catalog rejects", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheOK: false, liveResult: agent.Catalog{Models: []agent.Model{}}}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("live empty: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
	})

	t.Run("live fallback catalog rejects", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheOK: false, liveResult: agent.Catalog{Models: []agent.Model{{ID: "stand-in"}}, Fallback: true}}
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("live fallback: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
	})

	t.Run("live deadline is enforced", func(t *testing.T) {
		t.Parallel()
		port := &fakeChatCatalogPort{cacheOK: false, liveErr: context.DeadlineExceeded}
		start := time.Now()
		_, err := loadChatCatalogForVerdict(ctx, port, "rt-1", AgentVerdict{Availability: AgentAvailable})
		if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
			t.Fatalf("deadline: got %v, want ErrInvalidModelOrThinkingLevel", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("live round took %s; the 30s ctx deadline alone should bound it", elapsed)
		}
	})
}

// TestLoadChatCatalogForConfigBlockedWithoutDB covers the full entry point for
// the one verdict that needs no database round trip: an archived agent is
// blocked before any query.
func TestLoadChatCatalogForConfigBlockedWithoutDB(t *testing.T) {
	t.Parallel()
	port := &fakeChatCatalogPort{cacheResult: sampleCatalog(), cacheOK: true}
	archived := db.Agent{ArchivedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	_, err := LoadChatCatalogForConfig(context.Background(), nil, port, archived)
	if !errors.Is(err, ErrInvalidModelOrThinkingLevel) {
		t.Fatalf("archived agent: got %v, want ErrInvalidModelOrThinkingLevel", err)
	}
	if port.liveCalls != 0 {
		t.Fatalf("blocked verdict must not call LiveLoad (calls=%d)", port.liveCalls)
	}
}
