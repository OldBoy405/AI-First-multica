package agent

import "testing"

// TestValidateChatConfigMatrix pins the single validation entry point for
// session-level chat configs (CR-2026-056, BLOCK-014). The thinking half runs
// through ValidateThinkingLevelWith over the same catalog, so these cases
// intentionally mirror its semantics: empty thinking accepted, codex
// empty-model fail-closed, unknown model fail-closed.
func TestValidateChatConfigMatrix(t *testing.T) {
	t.Parallel()

	catalog := Catalog{Models: []Model{
		{
			ID:      "claude-opus-5",
			Default: true,
			Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
				{Value: "high"},
				{Value: "low"},
			}},
		},
		{
			ID:      "codex-model-a",
			Default: true,
			Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
				{Value: "high"},
			}},
		},
		{
			ID: "opencode-model-b",
			Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
				{Value: "high"},
			}},
		},
	}}

	cases := []struct {
		name         string
		providerType string
		model        string
		thinking     string
		want         bool
	}{
		// Empty model + empty thinking passes for every provider: the
		// runtime-default sentinel is legal and the empty level means
		// "inherit runtime configuration".
		{name: "claude empty/empty", providerType: "claude", model: "", thinking: "", want: true},
		{name: "codex empty/empty", providerType: "codex", model: "", thinking: "", want: true},
		{name: "opencode empty/empty", providerType: "opencode", model: "", thinking: "", want: true},
		{name: "generic empty/empty", providerType: "cursor", model: "", thinking: "", want: true},

		// Codex empty model + non-empty thinking fails closed (an errored
		// catalog lookup must never pass the level through).
		{name: "codex empty/non-empty fails closed", providerType: "codex", model: "", thinking: "high", want: false},

		// Claude/opencode empty model resolve to the catalog default via
		// ValidateThinkingLevelWith.
		{name: "claude empty model resolves default", providerType: "claude", model: "", thinking: "high", want: true},
		{name: "claude empty model unsupported level", providerType: "claude", model: "", thinking: "nope", want: false},
		{name: "opencode empty model any-model level", providerType: "opencode", model: "", thinking: "high", want: true},
		{name: "opencode empty model unsupported level", providerType: "opencode", model: "", thinking: "nope", want: false},

		// Non-empty unknown models fail closed with or without a level.
		{name: "unknown model no level", providerType: "claude", model: "claude-madeup-9", thinking: "", want: false},
		{name: "unknown model with level", providerType: "claude", model: "claude-madeup-9", thinking: "high", want: false},
		{name: "unknown codex model with level", providerType: "codex", model: "codex-madeup-9", thinking: "high", want: false},

		// Known models.
		{name: "known claude model", providerType: "claude", model: "claude-opus-5", thinking: "high", want: true},
		{name: "known codex model", providerType: "codex", model: "codex-model-a", thinking: "high", want: true},
		{name: "known model unsupported level", providerType: "claude", model: "claude-opus-5", thinking: "max", want: false},

		// Claude context-window variants resolve to the base model's
		// capability catalog (ModelIDForCapabilityLookup), but the variant
		// string itself is never a catalog entry.
		{name: "claude window variant hits base model", providerType: "claude", model: "claude-opus-5[1m]", thinking: "high", want: true},
		{name: "claude window variant bad level", providerType: "claude", model: "claude-opus-5[1m]", thinking: "nope", want: false},

		// Non-claude providers keep exact-match lookup: a window-tag suffix
		// does not strip.
		{name: "codex tag suffix stays exact", providerType: "codex", model: "codex-model-a[1m]", thinking: "high", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateChatConfig(catalog, tc.providerType, tc.model, tc.thinking)
			if err != nil {
				t.Fatalf("ValidateChatConfig(%s,%q,%q) error: %v", tc.providerType, tc.model, tc.thinking, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateChatConfig(%s,%q,%q) = %v, want %v", tc.providerType, tc.model, tc.thinking, got, tc.want)
			}
		})
	}
}

// TestStaticCatalogLoaderIsIOLess pins that StaticCatalog performs no I/O:
// the returned loader is called once and must return the exact same catalog.
func TestStaticCatalogLoaderIsIOLess(t *testing.T) {
	t.Parallel()
	want := Catalog{Models: []Model{{ID: "claude-opus-5", Default: true}}}
	load := StaticCatalog(want)
	got, err := load()
	if err != nil {
		t.Fatalf("StaticCatalog loader error: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != want.Models[0].ID {
		t.Fatalf("StaticCatalog loader returned %+v, want %+v", got, want)
	}
	// Second call is equally I/O-free and stable.
	if again, err := load(); err != nil || again.Models[0].ID != want.Models[0].ID {
		t.Fatalf("StaticCatalog loader second call: %+v, %v", again, err)
	}
}
