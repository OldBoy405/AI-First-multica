package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

func seedPipelineCRRoot(t *testing.T, crID string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "change-requests", crID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cr.md"), []byte("id: "+crID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFindPipelineCRRootCardinality(t *testing.T) {
	const crID = "CR-2026-045"
	root := seedPipelineCRRoot(t, crID)
	if got, err := findPipelineCRRoot([]string{root}, crID); err != nil || got != root {
		t.Fatalf("single root: got=%q err=%v", got, err)
	}
	if _, err := findPipelineCRRoot(nil, crID); err == nil || !strings.Contains(err.Error(), pipelineCrctlUnavailable) {
		t.Fatalf("zero roots must fail closed: %v", err)
	}
	if _, err := findPipelineCRRoot([]string{root}, "../../escape"); err == nil {
		t.Fatal("invalid CR ID must fail before any filesystem lookup")
	}
	other := seedPipelineCRRoot(t, crID)
	if _, err := findPipelineCRRoot([]string{root, other}, crID); err == nil {
		t.Fatal("two matching roots must fail closed")
	}
}

func TestPreparePipelineTaskHydratesMachineLocalPaths(t *testing.T) {
	const crID = "CR-2026-045"
	root := seedPipelineCRRoot(t, crID)
	operational := filepath.Join(root, ".rayai-worktrees", "knowledge-base", "requirement", crID)
	if err := os.MkdirAll(operational, 0o755); err != nil {
		t.Fatal(err)
	}
	old := pipelineWorkspaceInspect
	pipelineWorkspaceInspect = func(_ context.Context, gotRoot, gotCR string) (string, error) {
		if gotRoot != root || gotCR != crID {
			t.Fatalf("inspect args root=%q cr=%q", gotRoot, gotCR)
		}
		return operational, nil
	}
	t.Cleanup(func() { pipelineWorkspaceInspect = old })
	d := &Daemon{cfg: Config{CRWorkspaceRoots: []string{root}}}
	task := Task{PipelinePrompt: "do it", PipelineCrID: crID}
	if err := d.preparePipelineTask(context.Background(), &task); err != nil {
		t.Fatal(err)
	}
	if task.PipelineWorkspace != root || task.PipelineLocalWorkDir != operational {
		t.Fatalf("paths not hydrated: %+v", task)
	}
	assignment, err := localDirectoryAssignmentForTask(task, "daemon-test")
	if err != nil || assignment == nil || assignment.AbsPath != operational {
		t.Fatalf("pipeline LocalWorkDir not routed through local-directory seam: %+v err=%v", assignment, err)
	}
}

func TestConfigurePipelineGitEnvironment(t *testing.T) {
	root := t.TempDir()
	operational := filepath.Join(root, ".rayai-worktrees", "knowledge-base", "requirement", "CR-2026-045")
	if err := os.MkdirAll(operational, 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GIT_CONFIG_COUNT": "99", "GIT_CONFIG_KEY_0": "unsafe.override"}
	auditDir, err := configurePipelineGitEnvironment(env, root, operational)
	if err != nil {
		t.Fatal(err)
	}
	if env["GIT_CONFIG_GLOBAL"] != filepath.Join(root, ".crctl", "task-gitconfig") {
		t.Fatalf("pipeline Git config is not scoped to task state: %#v", env)
	}
	if env["CRCTL_OPERATIONAL_WORKSPACE"] != operational {
		t.Fatalf("operational workspace not exported: %#v", env)
	}
	config, err := os.ReadFile(env["GIT_CONFIG_GLOBAL"])
	if err != nil || !strings.Contains(string(config), filepath.ToSlash(root)) || !strings.Contains(string(config), filepath.ToSlash(operational)) {
		t.Fatalf("safe directory config is not scoped to validated paths: config=%q err=%v", config, err)
	}
	if info, err := os.Stat(auditDir); err != nil || !info.IsDir() {
		t.Fatalf("audit directory not created: path=%q err=%v", auditDir, err)
	}
	wantConfig := "sandbox_workspace_write.writable_roots=[\"" + filepath.ToSlash(auditDir) + "\"]"
	if got := pipelineCodexWritableRootConfig(auditDir); got != wantConfig {
		t.Fatalf("Codex writable root config=%q, want %q", got, wantConfig)
	}
}

func TestInstallPipelineCrctlLauncher(t *testing.T) {
	tools := t.TempDir()
	rulesPath := filepath.Join(tools, "skills", "shared", "controlled-shell", "rules.json")
	crctlPath := filepath.Join(tools, "skills", "shared", "crctl", "scripts", "crctl.mjs")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(crctlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crctlPath, []byte("console.log(process.argv.slice(2).join('|'));\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(gitguard.EnvRulesPath, rulesPath)

	binDir := t.TempDir()
	if err := installPipelineCrctlLauncher(binDir); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(binDir, "crctl")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		launcher += ".cmd"
		cmd = exec.Command("cmd", "/c", launcher, "alpha", "beta")
	} else {
		cmd = exec.Command(launcher, "alpha", "beta")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher failed: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "alpha|beta" {
		t.Fatalf("launcher output=%q, want alpha|beta", got)
	}
}

func TestPipelinePromptDoesNotEnterIssueWorkflow(t *testing.T) {
	prompt := BuildPrompt(Task{PipelinePrompt: "run the fixed skill", PipelineCrID: "CR-2026-045", IssueID: "should-not-render"}, "claude")
	for _, forbidden := range []string{"multica issue get", "quick-create", "comment history"} {
		if strings.Contains(strings.ToLower(prompt), strings.ToLower(forbidden)) {
			t.Fatalf("pipeline prompt leaked %q workflow: %s", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, "run the fixed skill") {
		t.Fatal("fixed pipeline instruction missing")
	}
}
