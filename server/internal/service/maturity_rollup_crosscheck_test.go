package service_test

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/service"
)

// TestMaturityReviewNodeIDs cross-checks the service-side copy of the three
// review-gate node UUIDs against the canonical governance.ReviewGateNodes map
// (service cannot import governance — governance.runner imports service, so
// the copy is the only cycle-free way to feed the SQL). Drift here would
// silently break the prototype_direct_rate and gate-first-pass metrics.
func TestMaturityReviewNodeIDs(t *testing.T) {
	want := []string{
		governance.ReviewGateNodes["requirement"].NodeID,
		governance.ReviewGateNodes["tech-design"].NodeID,
		governance.ReviewGateNodes["code"].NodeID,
	}
	got := service.MaturityReviewNodeIDs()
	if len(got) != len(want) {
		t.Fatalf("node id count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node id %d = %q, want %q", i, got[i], want[i])
		}
	}
}
