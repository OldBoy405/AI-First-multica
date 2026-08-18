package governance

import "testing"

// AIFIRST: behavioral lock for transitions_gen.go (CR-2026-002 TASK-04 acceptance 3).
// The table DATA is kept in sync with tools dir-graph.yaml by the gen script's
// --check mode; these tests lock the SEMANTICS.

func TestTransitionTableShape(t *testing.T) {
	if got := len(Transitions); got != 50 {
		t.Fatalf("expanded transition count = %d, want 50 from the canonical 28 declarations after wildcard expansion", got)
	}
	// 15 named states; the colloquial "16 states" includes pre-registration (new),
	// which is represented as the empty string and excluded from the enum.
	if got := len(KnownStatuses); got != 15 {
		t.Fatalf("status enum size = %d, want 15", got)
	}
	for s := range TerminalStatuses {
		for _, tr := range Transitions {
			if tr.From == s {
				t.Errorf("terminal state %s must have no outgoing edge, found %s -> %s", s, tr.From, tr.To)
			}
		}
	}
}

func TestIsLegalTransition(t *testing.T) {
	cases := []struct {
		from, to, trigger string
		want              bool
	}{
		// registration ((new) is the empty string)
		{"", "drafting", "requirement-register", true},
		// mainline samples
		{"drafting", "requirement-reviewing", "review-requirement", true},
		{"requirement-reviewing", "requirement-approved", "approve-requirement", true},
		{"code-approved", "merging", "merge-feature-branch", true},
		{"writing-back", "archived", "cr-archive", true},
		// wildcard expansion: any active state can be rejected/withdrawn
		{"developing", "rejected", "cr-review-record:reject", true},
		{"merging", "withdrawn", "cr-review-record:withdraw", true},
		// empty trigger matches on (from, to) only (the commit-scan fallback channel
		// cannot recover the trigger)
		{"drafting", "requirement-reviewing", "", true},
		// illegal: skipping stages, wrong trigger, edges out of terminal states
		{"drafting", "code-approved", "approve-code", false},
		{"drafting", "requirement-reviewing", "made-up-trigger", false},
		{"archived", "drafting", "requirement-register", false},
		{"rejected", "drafting", "", false},
	}
	for _, c := range cases {
		if got := IsLegalTransition(c.from, c.to, c.trigger); got != c.want {
			t.Errorf("IsLegalTransition(%q, %q, %q) = %v, want %v", c.from, c.to, c.trigger, got, c.want)
		}
	}
}

func TestActionConstants(t *testing.T) {
	// activity_log.action is free text; the aifirst. prefix lets the governance
	// board aggregate fork-specific actions by prefix.
	for _, a := range []string{ActionGitguardDenied, ActionEvidenceDrift} {
		if len(a) < 9 || a[:8] != "aifirst." {
			t.Errorf("action %q must start with the aifirst. prefix", a)
		}
	}
}
