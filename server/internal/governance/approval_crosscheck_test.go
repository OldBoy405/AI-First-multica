package governance

// AIFIRST: cross-tool seam test (CR-2026-002 TASK-08, AC-4①; extended by
// CR-2026-030 TASK-07): grants signed by the Go ApprovalService must be
// consumed by the real crctl `approve --grant` verification (Node ed25519 +
// digest recomputation). Covers approve, reject rollback, and adjacent replay
// for all four approval stages. Skips when node or crctl is unavailable;
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

const crosscheckCR = "CR-2026-001"

var crosscheckPassReview = "verdict: pass\nblockers: []\n"

type crosscheckStage struct {
	name       string
	initial    string
	approved   string
	rolledBack string
	trigger    string
	evidence   map[string]string
}

var crosscheckStages = []crosscheckStage{
	{
		name: "requirement", initial: "requirement-reviewing", approved: "requirement-approved",
		rolledBack: "drafting", trigger: "approve-requirement:reject -> write-requirement-prd",
		evidence: map[string]string{"review-annotations/requirement.yml": crosscheckPassReview},
	},
	{
		name: "tech-design", initial: "tech-design-review-pending", approved: "tech-design-reviewed",
		rolledBack: "tech-designing", trigger: "approve-tech-design:reject -> write-tech-design",
		evidence: map[string]string{"review-annotations/sdd.yml": crosscheckPassReview},
	},
	{
		name: "dev-start", initial: "task-breakdown", approved: "developing",
		rolledBack: "tech-design-reviewed", trigger: "approve-dev-start:reject -> write-dev-plan",
		evidence: map[string]string{"plan.md": "# plan\n", "review-annotations/dev-plan.yml": crosscheckPassReview},
	},
	{
		name: "code", initial: "code-reviewing", approved: "code-approved",
		rolledBack: "developing", trigger: "approve-code:reject -> implement-code",
		evidence: map[string]string{
			"review-annotations/code.yml": crosscheckPassReview,
			"test-report.md":              "---\nstatus: pass\n---\n",
		},
	},
}

func writeCrosscheckFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func crosscheckDigest(files map[string]string) string {
	evidence := make(map[string]string, len(files))
	for rel, content := range files {
		normalized := strings.ReplaceAll(content, "\r\n", "\n")
		sum := sha256.Sum256([]byte(normalized))
		path := filepath.ToSlash(filepath.Join("change-requests", crosscheckCR, rel))
		evidence[path] = "sha256:" + hex.EncodeToString(sum[:])
	}
	return CanonicalDigestFromEvidence(evidence)
}

func writeCrosscheckApproval(t *testing.T, crDir, section, digest, target string) {
	t.Helper()
	content := fmt.Sprintf("%s:\n  approver: \"alice@corp\"\n  approved-at: \"2026-08-11T10:00:00+08:00\"\n  via: crctl-approve\n  evidence-digest: \"%s\"\n  target-status: \"%s\"\n", section, digest, target)
	writeCrosscheckFile(t, crDir, "approval.yml", content)
}

// newCrosscheckWorkspace builds the minimum authoritative ledgers and gate
// evidence for one approval stage. The resolved crctl path is the only source
// used to derive tools_package_path, including sibling-checkout fallback.
func newCrosscheckWorkspace(t *testing.T, crctl string, stage crosscheckStage) (ws, crDir string) {
	t.Helper()
	ws = t.TempDir()
	crDir = filepath.Join(ws, "change-requests", crosscheckCR)
	writeCrosscheckFile(t, ws, "change-requests/_backlog.yml", "change-requests:\n  - id: "+crosscheckCR+"\n")
	writeCrosscheckFile(t, crDir, "cr.md", "---\nid: "+crosscheckCR+"\nstatus: "+stage.initial+"\n---\n")
	for rel, content := range stage.evidence {
		writeCrosscheckFile(t, crDir, rel, content)
	}

	switch stage.name {
	case "tech-design":
		requirementEvidence := map[string]string{"review-annotations/requirement.yml": crosscheckPassReview}
		writeCrosscheckFile(t, crDir, "review-annotations/requirement.yml", crosscheckPassReview)
		writeCrosscheckApproval(t, crDir, "requirement", crosscheckDigest(requirementEvidence), "requirement-approved")
	case "dev-start":
		techEvidence := map[string]string{"review-annotations/sdd.yml": crosscheckPassReview}
		writeCrosscheckFile(t, crDir, "review-annotations/sdd.yml", crosscheckPassReview)
		writeCrosscheckFile(t, crDir, "sdd.md", "# sdd\n")
		writeCrosscheckFile(t, crDir, "tasks/_index.yml", "tasks:\n  - id: CR-2026-001-TASK-01\n")
		writeCrosscheckFile(t, crDir, "tasks/TASK-01.md", "# TASK-01\n")
		writeCrosscheckApproval(t, crDir, "tech-design", crosscheckDigest(techEvidence), "tech-design-reviewed")
	case "code":
		devEvidence := map[string]string{"plan.md": "# plan\n", "review-annotations/dev-plan.yml": crosscheckPassReview}
		writeCrosscheckFile(t, crDir, "plan.md", "# plan\n")
		writeCrosscheckFile(t, crDir, "review-annotations/dev-plan.yml", crosscheckPassReview)
		writeCrosscheckFile(t, crDir, "tasks/_index.yml", "tasks:\n  - id: CR-2026-001-TASK-01\n")
		writeCrosscheckFile(t, crDir, "tasks/TASK-01.md", "# TASK-01\n")
		writeCrosscheckApproval(t, crDir, "development-start", crosscheckDigest(devEvidence), "developing")
	}

	toolsRoot := crctl
	for range 5 {
		toolsRoot = filepath.Dir(toolsRoot)
	}
	writeCrosscheckFile(t, ws, "dir-graph.yaml", fmt.Sprintf("workspace:\n  tools_package_path: %q\n", filepath.ToSlash(toolsRoot)))

	for _, args := range [][]string{{"init", "-b", "master"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-m", "[cr] seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return ws, crDir
}

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
	if err := os.WriteFile(filepath.Join(keysDir, "approval-crosscheck.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSignedGrant(t *testing.T, ws string, grant Grant) string {
	t.Helper()
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	grantPath := filepath.Join(ws, "grant.json")
	if err := os.WriteFile(grantPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return grantPath
}

func signCrosscheckGrant(t *testing.T, ws string, stage crosscheckStage, decision string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publishCrosscheckKey(t, ws, pub)
	grant := Grant{
		V: 1, CRID: crosscheckCR, Stage: stage.name, Decision: decision,
		Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
		EvidenceDigest: crosscheckDigest(stage.evidence), KeyID: "approval-crosscheck",
	}
	NewApprovalService(nil, priv, "approval-crosscheck").signGrant(&grant)
	return writeSignedGrant(t, ws, grant)
}

func runCrctlApprove(nodeBin, crctl, ws, stage, grantPath string) (string, error) {
	out, err := exec.Command(nodeBin, crctl, "approve", crosscheckCR, "--stage", stage, "--grant", grantPath, "--workspace", ws).CombinedOutput()
	return string(out), err
}

func resolveCrosscheckCrctl(t *testing.T) (string, string) {
	t.Helper()
	crctl := os.Getenv("CRCTL_PATH")
	if crctl == "" {
		candidates := []string{
			filepath.Join("..", "..", "..", "..", "tools", "skills", "shared", "crctl", "scripts", "crctl.mjs"),
			filepath.Join("..", "..", "..", "..", "..", "..", "tools", "skills", "shared", "crctl", "scripts", "crctl.mjs"),
		}
		for _, candidate := range candidates {
			abs, err := filepath.Abs(candidate)
			if err != nil {
				continue
			}
			if _, err := os.Stat(abs); err == nil {
				crctl = abs
				break
			}
		}
	}
	nodeBin, err := exec.LookPath("node")
	if crctl == "" || err != nil {
		t.Skip("crctl.mjs or node not available; cross-tool seam covered in the e2e task instead")
	}
	return nodeBin, crctl
}

func TestGrantCrossVerifiesWithCrctl(t *testing.T) {
	nodeBin, crctl := resolveCrosscheckCrctl(t)
	for _, stage := range crosscheckStages {
		stage := stage
		t.Run(stage.name+" approve and adjacent replay", func(t *testing.T) {
			ws, crDir := newCrosscheckWorkspace(t, crctl, stage)
			grantPath := signCrosscheckGrant(t, ws, stage, "approve")

			out, err := runCrctlApprove(nodeBin, crctl, ws, stage.name, grantPath)
			if err != nil {
				t.Fatalf("fresh approve failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, `"advanced": true`) || !strings.Contains(out, `"to": "`+stage.approved+`"`) {
				t.Fatalf("fresh approve did not reach %s:\n%s", stage.approved, out)
			}
			approval, err := os.ReadFile(filepath.Join(crDir, "approval.yml"))
			if err != nil || !strings.Contains(string(approval), "server-approve") {
				t.Fatalf("approval.yml must record server-approve: %v\n%s", err, approval)
			}

			out, err = runCrctlApprove(nodeBin, crctl, ws, stage.name, grantPath)
			if err != nil || !strings.Contains(out, `"changed": false`) {
				t.Fatalf("adjacent approve replay must be idempotent: %v\n%s", err, out)
			}
		})

		t.Run(stage.name+" reject rollback and adjacent replay", func(t *testing.T) {
			ws, crDir := newCrosscheckWorkspace(t, crctl, stage)
			grantPath := signCrosscheckGrant(t, ws, stage, "reject")

			out, err := runCrctlApprove(nodeBin, crctl, ws, stage.name, grantPath)
			if err == nil || !strings.Contains(out, "APPROVAL_DECLINED_ROLLED_BACK") || !strings.Contains(out, `"changed": true`) || !strings.Contains(out, `"rolledBackTo": "`+stage.rolledBack+`"`) || !strings.Contains(out, `"trigger": "`+stage.trigger+`"`) {
				t.Fatalf("fresh reject must roll back via %s: %v\n%s", stage.trigger, err, out)
			}
			crMd, readErr := os.ReadFile(filepath.Join(crDir, "cr.md"))
			if readErr != nil || !strings.Contains(string(crMd), "status: "+stage.rolledBack) {
				t.Fatalf("cr.md must be rolled back to %s: %v\n%s", stage.rolledBack, readErr, crMd)
			}

			out, err = runCrctlApprove(nodeBin, crctl, ws, stage.name, grantPath)
			if err == nil || !strings.Contains(out, "APPROVAL_DECLINED_ROLLED_BACK") || !strings.Contains(out, `"changed": false`) {
				t.Fatalf("adjacent reject replay must be idempotent: %v\n%s", err, out)
			}
		})
	}
}
