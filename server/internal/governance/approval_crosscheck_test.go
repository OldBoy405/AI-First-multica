package governance

// AIFIRST: cross-tool seam test (CR-2026-002 TASK-08, AC-4①): a grant signed by
// the Go ApprovalService must pass the real crctl `approve --grant`
// verification (Node ed25519 + digest recomputation). Skips when node or the
// sibling tools checkout is unavailable (e.g. upstream CI).

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrantCrossVerifiesWithCrctl(t *testing.T) {
	crctl := os.Getenv("CRCTL_PATH")
	if crctl == "" {
		// sibling-tools convention: <repo>/../tools/skills/shared/crctl/scripts/crctl.mjs
		candidates := []string{
			filepath.Join("..", "..", "..", "..", "tools", "skills", "shared", "crctl", "scripts", "crctl.mjs"),
			filepath.Join("..", "..", "..", "..", "..", "..", "tools", "skills", "shared", "crctl", "scripts", "crctl.mjs"),
		}
		for _, c := range candidates {
			if abs, err := filepath.Abs(c); err == nil {
				if _, statErr := os.Stat(abs); statErr == nil {
					crctl = abs
					break
				}
			}
		}
	}
	nodeBin, nodeErr := exec.LookPath("node")
	if crctl == "" || nodeErr != nil {
		t.Skip("crctl.mjs or node not available; cross-tool seam covered in the e2e task instead")
	}

	// Minimal crctl workspace: backlog entry awaiting requirement approval +
	// passing review evidence.
	ws := t.TempDir()
	crDir := filepath.Join(ws, "change-requests", "CR-2026-001")
	if err := os.MkdirAll(filepath.Join(crDir, "review-annotations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "change-requests", "_backlog.yml"),
		[]byte("change-requests:\n  - id: CR-2026-001\n    status: requirement-reviewing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evidenceRel := filepath.Join("change-requests", "CR-2026-001", "review-annotations", "requirement.yml")
	evidenceContent := []byte("verdict: pass\nblockers: []\n")
	if err := os.WriteFile(filepath.Join(ws, evidenceRel), evidenceContent, 0o644); err != nil {
		t.Fatal(err)
	}
	// Init a git repo so the cascaded advance can commit.
	for _, args := range [][]string{{"init", "-b", "master"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	// Go side: sign a grant over the canonical digest of that exact evidence.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewApprovalService(nil, priv, "approval-crosscheck")
	sum := sha256.Sum256(evidenceContent) // content is LF already; normalization is a no-op
	digest := CanonicalDigestFromEvidence(map[string]string{
		"change-requests/CR-2026-001/review-annotations/requirement.yml": "sha256:" + hex.EncodeToString(sum[:]),
	})
	grant := Grant{
		V: 1, CRID: "CR-2026-001", Stage: "requirement", Decision: "approve",
		Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
		EvidenceDigest: digest, KeyID: "approval-crosscheck",
	}
	svc.signGrant(&grant)

	// Publish the public key the way the knowledge-base repo would.
	keysDir := filepath.Join(ws, ".crctl", "keys")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, "approval-crosscheck.pub"),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	grantPath := filepath.Join(ws, "grant.json")
	raw, _ := json.Marshal(grant)
	if err := os.WriteFile(grantPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(nodeBin, crctl, "approve", "CR-2026-001", "--stage", "requirement",
		"--grant", grantPath, "--workspace", ws).CombinedOutput()
	if err != nil {
		t.Fatalf("crctl approve --grant rejected a Go-signed grant: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"advanced": true`) {
		t.Fatalf("expected cascaded advance, got: %s", out)
	}
	approval, err := os.ReadFile(filepath.Join(crDir, "approval.yml"))
	if err != nil || !strings.Contains(string(approval), "server-approve") {
		t.Fatalf("approval.yml must record via: server-approve: %v\n%s", err, approval)
	}
}
