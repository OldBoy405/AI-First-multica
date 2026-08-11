package governance

// AIFIRST: cross-tool seam test (CR-2026-002 TASK-08, AC-4①; extended by
// CR-2026-030 TASK-07): grants signed by the Go ApprovalService must be
// consumed by the real crctl `approve --grant` verification (Node ed25519 +
// digest recomputation). Covers approve fresh consumption, signed reject with
// authoritative rollback, and adjacent-state replay idempotency. Skips when
// node or the sibling tools checkout is unavailable (e.g. upstream CI);
// package-level TestMain additionally skips when the dev DB is unreachable.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCrosscheckWorkspace builds the minimal crctl workspace for the
// requirement approval stage: v2 backlog entry (no status), cr.md in
// requirement-reviewing, passing review evidence, and a clean git repo.
// CR-2026-030 TASK-07: cr.md is mandatory — resolveCrState and the advance
// writer read it; the old backlog-status fallback layout is not sufficient.
func newCrosscheckWorkspace(t *testing.T) (ws, crDir string) {
	t.Helper()
	ws = t.TempDir()
	crDir = filepath.Join(ws, "change-requests", "CR-2026-001")
	if err := os.MkdirAll(filepath.Join(crDir, "review-annotations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "change-requests", "_backlog.yml"),
		[]byte("change-requests:\n  - id: CR-2026-001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	crMd := "---\nid: CR-2026-001\nstatus: requirement-reviewing\n---\n"
	if err := os.WriteFile(filepath.Join(crDir, "cr.md"), []byte(crMd), 0o644); err != nil {
		t.Fatal(err)
	}
	evidenceContent := []byte("verdict: pass\nblockers: []\n")
	evidenceRel := filepath.Join("change-requests", "CR-2026-001", "review-annotations", "requirement.yml")
	if err := os.WriteFile(filepath.Join(ws, evidenceRel), evidenceContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "dir-graph.yaml"), []byte(fmt.Sprintf("workspace:\n  tools_package_path: %q\n", filepath.ToSlash(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(os.Getenv("CRCTL_PATH"))))))))), 0o644); err != nil {
		t.Fatal(err)
	}
	// Init a git repo and commit the seed so the cascaded advance/rollback
	// commits only the ledger files they own.
	for _, args := range [][]string{{"init", "-b", "master"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "[cr] seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return ws, crDir
}

// publishCrosscheckKey writes the Ed25519 public key the way the
// knowledge-base repo would, so crctl can verify grants signed here.
func publishCrosscheckKey(t *testing.T, ws string, pub ed25519.PublicKey) {
	t.Helper()
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
}

// writeSignedGrant marshals the grant and writes it to the workspace grant
// drop point consumed by `crctl approve --grant`.
func writeSignedGrant(t *testing.T, ws string, grant Grant) string {
	t.Helper()
	grantPath := filepath.Join(ws, "grant.json")
	raw, _ := json.Marshal(grant)
	if err := os.WriteFile(grantPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return grantPath
}

// runCrctlApprove executes `crctl approve ... --grant <path>`; returns the
// combined output (exit code is asserted by the caller per scenario).
func runCrctlApprove(t *testing.T, nodeBin, crctl, ws, grantPath string) (string, error) {
	t.Helper()
	out, err := exec.Command(nodeBin, crctl, "approve", "CR-2026-001", "--stage", "requirement",
		"--grant", grantPath, "--workspace", ws).CombinedOutput()
	return string(out), err
}

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

	// signerFor builds a key pair + ApprovalService + published public key.
	signerFor := func(ws string) (*ApprovalService, ed25519.PublicKey) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		publishCrosscheckKey(t, ws, pub)
		return NewApprovalService(nil, priv, "approval-crosscheck"), pub
	}

	t.Run("approve fresh consumption", func(t *testing.T) {
		ws, crDir := newCrosscheckWorkspace(t)
		svc, _ := signerFor(ws)
		sum := sha256.Sum256([]byte("verdict: pass\nblockers: []\n")) // content is LF already
		digest := CanonicalDigestFromEvidence(map[string]string{
			"change-requests/CR-2026-001/review-annotations/requirement.yml": "sha256:" + hex.EncodeToString(sum[:]),
		})
		grant := Grant{
			V: 1, CRID: "CR-2026-001", Stage: "requirement", Decision: "approve",
			Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
			EvidenceDigest: digest, KeyID: "approval-crosscheck",
		}
		svc.signGrant(&grant)
		grantPath := writeSignedGrant(t, ws, grant)

		out, err := runCrctlApprove(t, nodeBin, crctl, ws, grantPath)
		if err != nil {
			t.Fatalf("crctl approve --grant rejected a Go-signed approve grant: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"advanced": true`) {
			t.Fatalf("expected cascaded advance, got: %s", out)
		}
		approval, err := os.ReadFile(filepath.Join(crDir, "approval.yml"))
		if err != nil || !strings.Contains(string(approval), "server-approve") {
			t.Fatalf("approval.yml must record via: server-approve: %v\n%s", err, approval)
		}
	})

	t.Run("signed reject rolls back via authoritative transition", func(t *testing.T) {
		ws, crDir := newCrosscheckWorkspace(t)
		svc, _ := signerFor(ws)
		sum := sha256.Sum256([]byte("verdict: pass\nblockers: []\n"))
		digest := CanonicalDigestFromEvidence(map[string]string{
			"change-requests/CR-2026-001/review-annotations/requirement.yml": "sha256:" + hex.EncodeToString(sum[:]),
		})
		grant := Grant{
			V: 1, CRID: "CR-2026-001", Stage: "requirement", Decision: "reject",
			Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
			EvidenceDigest: digest, KeyID: "approval-crosscheck",
		}
		svc.signGrant(&grant)
		grantPath := writeSignedGrant(t, ws, grant)

		out, err := runCrctlApprove(t, nodeBin, crctl, ws, grantPath)
		if err == nil {
			t.Fatalf("signed reject must be a non-zero business result, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "APPROVAL_DECLINED_ROLLED_BACK") {
			t.Fatalf("expected APPROVAL_DECLINED_ROLLED_BACK, got:\n%s", out)
		}
		if !strings.Contains(out, `"changed": true`) {
			t.Fatalf("fresh reject must report changed=true, got:\n%s", out)
		}
		if !strings.Contains(out, `"rolledBackTo": "drafting"`) {
			t.Fatalf("requirement reject must roll back to drafting, got:\n%s", out)
		}
		crMd, err := os.ReadFile(filepath.Join(crDir, "cr.md"))
		if err != nil || !strings.Contains(string(crMd), "status: drafting") {
			t.Fatalf("cr.md must be rolled back to drafting: %v\n%s", err, crMd)
		}

		// Adjacent-state replay of the same signed reject: grant/evidence/
		// signature re-verified, ledger already at HEAD -> changed=false.
		out2, err2 := runCrctlApprove(t, nodeBin, crctl, ws, grantPath)
		if err2 == nil {
			t.Fatalf("adjacent reject replay must stay a non-zero business result, got exit 0:\n%s", out2)
		}
		if !strings.Contains(out2, "APPROVAL_DECLINED_ROLLED_BACK") || !strings.Contains(out2, `"changed": false`) {
			t.Fatalf("adjacent reject replay must be APPROVAL_DECLINED_ROLLED_BACK/changed=false, got:\n%s", out2)
		}
	})

	t.Run("approve adjacent-state replay is idempotent", func(t *testing.T) {
		ws, _ := newCrosscheckWorkspace(t)
		svc, _ := signerFor(ws)
		sum := sha256.Sum256([]byte("verdict: pass\nblockers: []\n"))
		digest := CanonicalDigestFromEvidence(map[string]string{
			"change-requests/CR-2026-001/review-annotations/requirement.yml": "sha256:" + hex.EncodeToString(sum[:]),
		})
		grant := Grant{
			V: 1, CRID: "CR-2026-001", Stage: "requirement", Decision: "approve",
			Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
			EvidenceDigest: digest, KeyID: "approval-crosscheck",
		}
		svc.signGrant(&grant)
		grantPath := writeSignedGrant(t, ws, grant)

		if out, err := runCrctlApprove(t, nodeBin, crctl, ws, grantPath); err != nil {
			t.Fatalf("first approve must succeed: %v\n%s", err, out)
		}
		out2, err2 := runCrctlApprove(t, nodeBin, crctl, ws, grantPath)
		if err2 != nil {
			t.Fatalf("adjacent approve replay must succeed with changed=false: %v\n%s", err2, out2)
		}
		if !strings.Contains(out2, `"changed": false`) {
			t.Fatalf("adjacent approve replay must report changed=false, got:\n%s", out2)
		}
	})
}
