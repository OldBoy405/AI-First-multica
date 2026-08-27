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

const crosscheckKBRepo = "ai-first-platform-docs"

const crosscheckDevPlanSubject = "830f1b4341935bd5c9e5c40908cce2a5e92b5b476c7cb8d50ae58511b4fc45ff"

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
		evidence: map[string]string{"plan.md": "# plan\n", "review-annotations/dev-plan.yml": crosscheckPassReview + "subject-sha256: " + crosscheckDevPlanSubject + "\n"},
	},
	// The code stage additionally re-verifies machine-injected release-subjects
	// (a multi-repo worktree seam), so it lives in a dedicated test below rather
	// than in this single-repo fixture loop.
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
	writeCrosscheckFile(t, ws, "change-requests/_backlog.yml", "schema: cr-backlog/v2\nchange-requests:\n  - id: "+crosscheckCR+"\n")
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
		devEvidence := map[string]string{"plan.md": "# plan\n", "review-annotations/dev-plan.yml": crosscheckPassReview + "subject-sha256: " + crosscheckDevPlanSubject + "\n"}
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
	return signCrosscheckGrantWithDigest(t, ws, stage.name, decision, crosscheckDigest(stage.evidence))
}

func signCrosscheckGrantWithDigest(t *testing.T, ws, stage, decision, digest string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publishCrosscheckKey(t, ws, pub)
	grant := Grant{
		V: 1, CRID: crosscheckCR, Stage: stage, Decision: decision,
		Approver: "alice@corp", ApprovedAt: time.Now().Format(time.RFC3339),
		EvidenceDigest: digest, KeyID: "approval-crosscheck",
	}
	NewApprovalService(nil, priv, "approval-crosscheck", nil, nil).signGrant(&grant)
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

// ─── code stage：machine-injected release-subjects seam (CR-2026-030 TASK-07) ──
//
// The code approval gate re-verifies review-annotations/code.yml#release-subjects
// against the local knowledge-base worktree: active repositories must match the
// snapshot, every controlled artifact (PRD/SDD/plan/tasks) must hash to the
// declared value (CRLF→LF), and the KB reviewed-source-sha must be an ancestor
// of the current HEAD with only whitelisted post-review paths in between.
// This dedicated test builds a real KB repo + linked worktree so the same
// crctl process exercises that seam end-to-end (approve + reject).

func runGitCrosscheck(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func sha256HexCrosscheck(s string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(s, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}

// writeCrosscheckUpstreamApprovals seeds the three approvals that must already
// exist before a code approve can pass its target gate: requirement,
// tech-design, and development-start (each with its review evidence + canonical
// digest). Without these the code-approved gate fails on the upstream chain.
func writeCrosscheckUpstreamApprovals(t *testing.T, crDir string) {
	t.Helper()
	reqEvidence := map[string]string{"review-annotations/requirement.yml": crosscheckPassReview}
	writeCrosscheckFile(t, crDir, "review-annotations/requirement.yml", crosscheckPassReview)
	techEvidence := map[string]string{"review-annotations/sdd.yml": crosscheckPassReview}
	writeCrosscheckFile(t, crDir, "review-annotations/sdd.yml", crosscheckPassReview)
	devPlan := crosscheckPassReview + "subject-sha256: " + crosscheckDevPlanSubject + "\n"
	devEvidence := map[string]string{"plan.md": "# plan\n", "review-annotations/dev-plan.yml": devPlan}
	writeCrosscheckFile(t, crDir, "review-annotations/dev-plan.yml", devPlan)

	sec := func(section, digest, target string) string {
		return fmt.Sprintf("%s:\n  approver: \"alice@corp\"\n  approved-at: \"2026-08-11T10:00:00+08:00\"\n  via: crctl-approve\n  evidence-digest: \"%s\"\n  target-status: \"%s\"\n", section, digest, target)
	}
	var b strings.Builder
	b.WriteString(sec("requirement", crosscheckDigest(reqEvidence), "requirement-approved"))
	b.WriteString(sec("tech-design", crosscheckDigest(techEvidence), "tech-design-reviewed"))
	b.WriteString(sec("development-start", crosscheckDigest(devEvidence), "developing"))
	writeCrosscheckFile(t, crDir, "approval.yml", b.String())
}

// newCrosscheckCodeWorkspace builds the code-stage authority: a knowledge-base
// repo whose dir-graph.yaml declares a single active repo, a linked worktree on
// requirement/CR-2026-001 holding the controlled artifacts, and a code.yml whose
// machine-injected release-subjects block matches the committed worktree HEAD.
// Returns (installRoot, worktree, codeYmlContent, testReportContent).
func newCrosscheckCodeWorkspace(t *testing.T, crctl string) (ws, worktree, codeYml, testReport string) {
	t.Helper()
	ws = t.TempDir()
	toolsRoot := crctl
	for range 5 {
		toolsRoot = filepath.Dir(toolsRoot)
	}
	graph := fmt.Sprintf("workspace:\n  tools_package_path: %q\nrepositories:\n  - id: %s\n    path: \".\"\n    trunk: master\n    role: knowledge-base\n",
		filepath.ToSlash(toolsRoot), crosscheckKBRepo)
	writeCrosscheckFile(t, ws, "dir-graph.yaml", graph)
	// Keep runtime artifacts (grant.json, .crctl/keys, ledger tmp) out of the
	// tracked worktree so classifyRepoWorkspace sees a clean tree.
	writeCrosscheckFile(t, ws, ".gitignore", ".crctl/\ngrant.json\n")

	runGitCrosscheck(t, ws, "init", "-b", "master")
	runGitCrosscheck(t, ws, "config", "user.email", "t@t")
	runGitCrosscheck(t, ws, "config", "user.name", "t")
	runGitCrosscheck(t, ws, "add", "-A")
	runGitCrosscheck(t, ws, "commit", "-m", "[cr] seed")

	branch := "requirement/" + crosscheckCR
	runGitCrosscheck(t, ws, "branch", branch, "master")
	worktree = filepath.Join(ws, ".rayai-worktrees", "knowledge-base", "requirement", crosscheckCR)
	runGitCrosscheck(t, ws, "worktree", "add", worktree, branch)

	crRel := filepath.Join("change-requests", crosscheckCR)
	testReport = "---\nstatus: pass\n---\n"
	writeCrosscheckFile(t, worktree, "change-requests/_backlog.yml", "schema: cr-backlog/v2\nchange-requests:\n  - id: "+crosscheckCR+"\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "cr.md"), "---\nid: "+crosscheckCR+"\nstatus: code-reviewing\n---\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "test-report.md"), testReport)
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "prd.md"), "# prd\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "sdd.md"), "# sdd\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "plan.md"), "# plan\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "tasks", "_index.yml"), "tasks:\n  - id: "+crosscheckCR+"-TASK-01\n")
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "tasks", "TASK-01.md"), "# TASK-01\n")
	writeCrosscheckUpstreamApprovals(t, filepath.Join(worktree, crRel))
	// Reviewed-source commit: everything the code review was performed against
	// except the review annotation itself (test-report is part of that reviewed
	// surface, not a post-review change).
	runGitCrosscheck(t, worktree, "add", "-A")
	runGitCrosscheck(t, worktree, "commit", "-m", "[cr] code under review")
	reviewedSha := runGitCrosscheck(t, worktree, "rev-parse", "HEAD")

	// Controlled artifacts in POSIX path order, matching collectControlledArtifacts.
	type artifact struct{ path, content string }
	base := "change-requests/" + crosscheckCR + "/"
	artifacts := []artifact{
		{base + "plan.md", "# plan\n"},
		{base + "prd.md", "# prd\n"},
		{base + "sdd.md", "# sdd\n"},
		{base + "tasks/TASK-01.md", "# TASK-01\n"},
		{base + "tasks/_index.yml", "tasks:\n  - id: " + crosscheckCR + "-TASK-01\n"},
	}
	var digestLines []string
	rs := "release-subjects:\n  version: 1\n  repositories:\n"
	rs += fmt.Sprintf("    - repo: %s\n      remote-ref: refs/heads/%s\n      reviewed-source-sha: %s\n", crosscheckKBRepo, branch, reviewedSha)
	rs += "  artifacts:\n    algorithm: sha256\n    canonicalization: crlf-to-lf+path-sort\n    files:\n"
	for _, a := range artifacts {
		h := sha256HexCrosscheck(a.content)
		digestLines = append(digestLines, a.path+":"+h)
		rs += fmt.Sprintf("      - { path: %s, sha256: %s }\n", a.path, h)
	}
	rs += "    digest: " + sha256HexCrosscheck(strings.Join(digestLines, "\n")) + "\n"

	codeYml = crosscheckPassReview + rs
	writeCrosscheckFile(t, worktree, filepath.Join(crRel, "review-annotations", "code.yml"), codeYml)
	runGitCrosscheck(t, worktree, "add", "-A")
	runGitCrosscheck(t, worktree, "commit", "-m", "[cr] review code")
	return ws, worktree, codeYml, testReport
}

func TestGrantCrossVerifiesWithCrctlCodeStage(t *testing.T) {
	nodeBin, crctl := resolveCrosscheckCrctl(t)

	codeEvidence := func(codeYml, testReport string) map[string]string {
		return map[string]string{"review-annotations/code.yml": codeYml, "test-report.md": testReport}
	}

	t.Run("code approve with release-subjects and adjacent replay", func(t *testing.T) {
		_, worktree, codeYml, testReport := newCrosscheckCodeWorkspace(t, crctl)
		digest := crosscheckDigest(codeEvidence(codeYml, testReport))
		grantPath := signCrosscheckGrantWithDigest(t, worktree, "code", "approve", digest)

		out, err := runCrctlApprove(nodeBin, crctl, worktree, "code", grantPath)
		if err != nil {
			t.Fatalf("code approve failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"advanced": true`) || !strings.Contains(out, `"to": "code-approved"`) {
			t.Fatalf("code approve did not reach code-approved:\n%s", out)
		}
		approval, err := os.ReadFile(filepath.Join(worktree, "change-requests", crosscheckCR, "approval.yml"))
		if err != nil || !strings.Contains(string(approval), "server-approve") || !strings.Contains(string(approval), "release-subjects") {
			t.Fatalf("approval.yml must record server-approve + release-subjects: %v\n%s", err, approval)
		}

		out, err = runCrctlApprove(nodeBin, crctl, worktree, "code", grantPath)
		if err != nil || !strings.Contains(out, `"changed": false`) {
			t.Fatalf("adjacent code approve replay must be idempotent: %v\n%s", err, out)
		}
	})

	t.Run("code reject rollback and adjacent replay", func(t *testing.T) {
		_, worktree, codeYml, testReport := newCrosscheckCodeWorkspace(t, crctl)
		digest := crosscheckDigest(codeEvidence(codeYml, testReport))
		grantPath := signCrosscheckGrantWithDigest(t, worktree, "code", "reject", digest)

		out, err := runCrctlApprove(nodeBin, crctl, worktree, "code", grantPath)
		if err == nil || !strings.Contains(out, "APPROVAL_DECLINED_ROLLED_BACK") || !strings.Contains(out, `"changed": true`) || !strings.Contains(out, `"rolledBackTo": "developing"`) {
			t.Fatalf("code reject must roll back to developing: %v\n%s", err, out)
		}
		crMd, readErr := os.ReadFile(filepath.Join(worktree, "change-requests", crosscheckCR, "cr.md"))
		if readErr != nil || !strings.Contains(string(crMd), "status: developing") {
			t.Fatalf("cr.md must be rolled back to developing: %v\n%s", readErr, crMd)
		}

		out, err = runCrctlApprove(nodeBin, crctl, worktree, "code", grantPath)
		if err == nil || !strings.Contains(out, "APPROVAL_DECLINED_ROLLED_BACK") || !strings.Contains(out, `"changed": false`) {
			t.Fatalf("adjacent code reject replay must be idempotent: %v\n%s", err, out)
		}
	})
}
