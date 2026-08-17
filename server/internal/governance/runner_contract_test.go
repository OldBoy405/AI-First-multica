package governance

import (
	"strings"
	"testing"
)

func TestArchitectureRunnerFeatureFlag(t *testing.T) {
	t.Setenv("AIFIRST_ARCHITECTURE_RUNNER", "")
	if ArchitectureRunnerEnabled() {
		t.Fatal("Runner must default off so the manual route remains untouched")
	}
	for _, value := range []string{"1", "true", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AIFIRST_ARCHITECTURE_RUNNER", value)
			if !ArchitectureRunnerEnabled() {
				t.Fatalf("expected %q to enable Runner", value)
			}
		})
	}
}

func TestParseCoreRegistryFixedContract(t *testing.T) {
	registry, err := parseCoreRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Pipeline.Nodes) != 5 || registry.Pipeline.Nodes[2].Kind != "human_approval" || registry.Pipeline.Nodes[2].Ref != "" {
		t.Fatalf("unexpected fixed Core contract: %+v", registry.Pipeline.Nodes)
	}
	if registry.Pipeline.Nodes[1].ReviewLoop == nil || registry.Pipeline.Nodes[1].ReviewLoop.MaxAttempts != 3 {
		t.Fatal("reviewLoop contract missing")
	}
}

func TestRenderCorePromptOnlyDeclaredTokens(t *testing.T) {
	got, err := renderCorePrompt("CR={{inputs.cr_id}} context={{inputs.tech_context}}", "CR-2026-045", "safe data")
	if err != nil {
		t.Fatal(err)
	}
	if got != "CR=CR-2026-045 context=safe data" {
		t.Fatalf("unexpected render %q", got)
	}
	if _, err := renderCorePrompt("{{inputs.cr_id}} {{unknown}}", "CR-2026-045", ""); err == nil || !strings.Contains(err.Error(), RunnerErrContractInvalid) {
		t.Fatalf("unrendered token must fail closed, got %v", err)
	}
	if _, err := renderCorePrompt("{{inputs.cr_id}}", "bad", ""); err == nil {
		t.Fatal("invalid CR ID must fail")
	}
}
