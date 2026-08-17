package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
