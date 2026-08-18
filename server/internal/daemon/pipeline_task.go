// AIFIRST: machine-local preparation for Runner pipeline tasks (CR-2026-045).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/pkg/gitguard"
)

const pipelineCrctlUnavailable = "PIPELINE_CRCTL_UNAVAILABLE"

var pipelineCRIDPattern = regexp.MustCompile(`^CR-[0-9]{4}-[0-9]{3,}$`)

type pipelineInspectResult struct {
	OperationalWorkspace string `json:"operationalWorkspace"`
	Resources            []struct {
		Classification string `json:"classification"`
	} `json:"resources"`
}

var pipelineWorkspaceInspect = inspectPipelineWorkspace

// preparePipelineTask resolves machine-local paths after claim. The server
// cannot safely provide these paths because the daemon may run elsewhere.
func (d *Daemon) preparePipelineTask(ctx context.Context, task *Task) error {
	if task == nil || task.PipelinePrompt == "" {
		return nil
	}
	root, err := findPipelineCRRoot(d.cfg.CRWorkspaceRoots, task.PipelineCrID)
	if err != nil {
		return err
	}
	operational, err := pipelineWorkspaceInspect(ctx, root, task.PipelineCrID)
	if err != nil {
		return err
	}
	task.PipelineWorkspace = root
	task.PipelineLocalWorkDir = operational
	return nil
}

func findPipelineCRRoot(roots []string, crID string) (string, error) {
	if !pipelineCRIDPattern.MatchString(crID) {
		return "", fmt.Errorf("%s: pipeline task has invalid cr_id", pipelineCrctlUnavailable)
	}
	matches := make([]string, 0, 1)
	for _, candidate := range roots {
		root, err := filepath.Abs(strings.TrimSpace(candidate))
		if err != nil || strings.TrimSpace(candidate) == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, "change-requests", crID, "cr.md")); err == nil && !info.IsDir() {
			matches = append(matches, filepath.Clean(root))
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%s: expected exactly one CR root for %s, found %d", pipelineCrctlUnavailable, crID, len(matches))
	}
	return matches[0], nil
}

func resolvePipelineCrctl() (string, string, error) {
	rulesPath := strings.TrimSpace(os.Getenv(gitguard.EnvRulesPath))
	if rulesPath == "" {
		return "", "", errors.New(pipelineCrctlUnavailable + ": controlled-shell rules not configured")
	}
	crctlPath := filepath.Clean(filepath.Join(filepath.Dir(rulesPath), "..", "crctl", "scripts", "crctl.mjs"))
	if info, err := os.Stat(crctlPath); err != nil || info.IsDir() {
		return "", "", fmt.Errorf("%s: crctl not found beside rules", pipelineCrctlUnavailable)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return "", "", fmt.Errorf("%s: node executable unavailable", pipelineCrctlUnavailable)
	}
	return node, crctlPath, nil
}

func installPipelineCrctlLauncher(binDir string) error {
	if strings.TrimSpace(binDir) == "" {
		return errors.New(pipelineCrctlUnavailable + ": controlled-shell shim unavailable")
	}
	node, crctlPath, err := resolvePipelineCrctl()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("%s: create launcher directory: %w", pipelineCrctlUnavailable, err)
	}
	sh := fmt.Sprintf("#!/bin/sh\nexec \"%s\" \"%s\" \"$@\"\n", filepath.ToSlash(node), filepath.ToSlash(crctlPath))
	if err := os.WriteFile(filepath.Join(binDir, "crctl"), []byte(sh), 0o755); err != nil {
		return fmt.Errorf("%s: write crctl launcher: %w", pipelineCrctlUnavailable, err)
	}
	if filepath.Separator == '\\' {
		cmd := fmt.Sprintf("@echo off\r\n\"%s\" \"%s\" %%*\r\n", node, crctlPath)
		if err := os.WriteFile(filepath.Join(binDir, "crctl.cmd"), []byte(cmd), 0o755); err != nil {
			return fmt.Errorf("%s: write crctl launcher: %w", pipelineCrctlUnavailable, err)
		}
	}
	return nil
}

func configurePipelineGitEnvironment(agentEnv map[string]string, root, operational string) (string, error) {
	if agentEnv == nil {
		return "", errors.New(pipelineCrctlUnavailable + ": agent environment unavailable")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%s: resolve CR root for Git: %w", pipelineCrctlUnavailable, err)
	}
	opReal, err := filepath.EvalSymlinks(operational)
	if err != nil {
		return "", fmt.Errorf("%s: resolve operational workspace for Git: %w", pipelineCrctlUnavailable, err)
	}
	auditDir := filepath.Join(rootReal, ".crctl")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		return "", fmt.Errorf("%s: create crctl state directory: %w", pipelineCrctlUnavailable, err)
	}
	configPath := filepath.Join(auditDir, "task-gitconfig")
	config := fmt.Sprintf("[safe]\n\tdirectory = %s\n\tdirectory = %s\n", filepath.ToSlash(rootReal), filepath.ToSlash(opReal))
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return "", fmt.Errorf("%s: write Git trust config: %w", pipelineCrctlUnavailable, err)
	}
	agentEnv["GIT_CONFIG_GLOBAL"] = configPath
	agentEnv["CRCTL_OPERATIONAL_WORKSPACE"] = opReal
	return auditDir, nil
}

func pipelineCodexWritableRootConfig(auditDir string) string {
	root, _ := json.Marshal(filepath.ToSlash(auditDir))
	return "sandbox_workspace_write.writable_roots=[" + string(root) + "]"
}

func inspectPipelineWorkspace(ctx context.Context, root, crID string) (string, error) {
	node, crctlPath, err := resolvePipelineCrctl()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, node, crctlPath, "workspace", "inspect", crID, "--workspace", root)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: workspace inspect failed: %w", pipelineCrctlUnavailable, err)
	}
	var result pipelineInspectResult
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("%s: workspace inspect returned invalid JSON", pipelineCrctlUnavailable)
	}
	if len(result.Resources) == 0 {
		return "", errors.New(pipelineCrctlUnavailable + ": workspace inspect returned no resources")
	}
	for _, resource := range result.Resources {
		if resource.Classification != "healthy" {
			return "", fmt.Errorf("%s: workspace resource is %s", pipelineCrctlUnavailable, resource.Classification)
		}
	}
	operational, err := normalizeLocalPath(result.OperationalWorkspace)
	if err != nil {
		return "", fmt.Errorf("%s: invalid operational workspace: %w", pipelineCrctlUnavailable, err)
	}
	if err := validateLocalPath(operational); err != nil {
		return "", fmt.Errorf("%s: operational workspace rejected: %w", pipelineCrctlUnavailable, err)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%s: resolve CR root: %w", pipelineCrctlUnavailable, err)
	}
	opReal, err := filepath.EvalSymlinks(operational)
	if err != nil {
		return "", fmt.Errorf("%s: resolve operational workspace: %w", pipelineCrctlUnavailable, err)
	}
	rel, err := filepath.Rel(rootReal, opReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New(pipelineCrctlUnavailable + ": operational workspace escapes CR root")
	}
	return filepath.Clean(operational), nil
}
