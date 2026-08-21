package drift

// AIFIRST: CR-2026-049 TASK-09 — binding resolver tests (SDD §3.3 TD-B5).
// Covers SSH canonicalization, missing/ambiguous workspace.repos matches,
// provider rejection, all-or-nothing behavior, and token discipline (results
// and errors never carry token material).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/commitprefix"
)

type fakeAccess struct {
	accessByRepo map[string][]int64 // "owner/repo" -> installation ids with access
	tokens       map[int64]string
	calls        int
}

func (f *fakeAccess) ResolveRepositoryAccess(_ context.Context, installationIDs []int64, owner, repo string) (int64, error) {
	f.calls++
	ids := f.accessByRepo[owner+"/"+repo]
	var found []int64
	for _, id := range installationIDs {
		for _, ok := range ids {
			if id == ok {
				found = append(found, id)
			}
		}
	}
	switch len(found) {
	case 0:
		return 0, errors.New("repository_access_missing")
	case 1:
		return found[0], nil
	default:
		return 0, errors.New("repository_access_ambiguous")
	}
}

func (f *fakeAccess) InstallationToken(_ context.Context, installationID int64) (string, error) {
	return f.tokens[installationID], nil
}

func decls() map[string]commitprefix.RepoPrefixDecl {
	return map[string]commitprefix.RepoPrefixDecl{
		"tools": {
			ID: "tools", CanonicalURL: "https://github.com/OldBoy405/AI-First-tools.git",
			Owner: "OldBoy405", Repo: "AI-First-tools", Trunk: "custom/main",
			Prefixes: []string{"[cr] ", "feat("},
		},
		"multica": {
			ID: "multica", CanonicalURL: "https://github.com/OldBoy405/AI-First-multica.git",
			Owner: "OldBoy405", Repo: "AI-First-multica", Trunk: "main",
			Prefixes: []string{"MUL-", "feat("},
		},
	}
}

func TestResolveBindingsHappyPathWithSSHCanonicalization(t *testing.T) {
	access := &fakeAccess{
		accessByRepo: map[string][]int64{
			"OldBoy405/AI-First-tools":   {7},
			"OldBoy405/AI-First-multica": {9},
		},
		tokens: map[int64]string{7: "tok-tools", 9: "tok-multica"},
	}
	r := NewResolver(access)
	bound, err := r.ResolveBindings(context.Background(), "ws-1", decls(), []RepoData{
		// SSH form of the tools repo must canonicalize to the same owner/repo.
		{URL: "git@github.com:OldBoy405/AI-First-tools.git", Description: "tools fork"},
		{URL: "https://github.com/OldBoy405/AI-First-multica", Description: "multica fork"},
	}, []GitHubInstallation{{ID: "i1", InstallationID: 7}, {ID: "i2", InstallationID: 9}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(bound) != 2 {
		t.Fatalf("bound = %d, want 2", len(bound))
	}
	// Deterministic order: declaration id ascending (multica < tools).
	if bound[0].RepoID != "multica" || bound[0].Token != "tok-multica" || bound[0].Trunk != "main" {
		t.Errorf("bound[0] = %+v", bound[0])
	}
	if bound[1].RepoID != "tools" || bound[1].Token != "tok-tools" || bound[1].Trunk != "custom/main" {
		t.Errorf("bound[1] = %+v", bound[1])
	}
	// Prefixes bound verbatim from the declaration.
	if len(bound[1].Prefixes) != 2 || bound[1].Prefixes[0] != "[cr] " {
		t.Errorf("prefixes not verbatim: %+v", bound[1].Prefixes)
	}
}

func TestResolveBindingsMissingWorkspaceRepo(t *testing.T) {
	access := &fakeAccess{accessByRepo: map[string][]int64{}, tokens: map[int64]string{}}
	r := NewResolver(access)
	_, err := r.ResolveBindings(context.Background(), "ws-1", decls(), []RepoData{
		{URL: "https://github.com/OldBoy405/AI-First-tools.git"},
	}, []GitHubInstallation{{InstallationID: 7}})
	if !strings.Contains(err.Error(), ErrRepositoryAccessMissing.Error()) {
		t.Fatalf("err = %v, want repository_access_missing", err)
	}
}

func TestResolveBindingsProviderUnsupported(t *testing.T) {
	access := &fakeAccess{accessByRepo: map[string][]int64{}, tokens: map[int64]string{}}
	r := NewResolver(access)
	d := decls()
	// Non-GitHub canonical URL in the declaration itself → provider rejection.
	m := d["multica"]
	m.CanonicalURL = "https://gitlab.com/OldBoy405/AI-First-multica.git"
	d["multica"] = m
	_, err := r.ResolveBindings(context.Background(), "ws-1", d, []RepoData{
		{URL: "https://github.com/OldBoy405/AI-First-tools.git"},
		{URL: "https://github.com/OldBoy405/AI-First-multica.git"},
	}, []GitHubInstallation{{InstallationID: 7}})
	if !strings.Contains(err.Error(), ErrProviderUnsupported.Error()) {
		t.Fatalf("err = %v, want repository_provider_unsupported", err)
	}
}

func TestResolveBindingsAmbiguousInstallation(t *testing.T) {
	access := &fakeAccess{
		accessByRepo: map[string][]int64{"OldBoy405/AI-First-tools": {7, 8}, "OldBoy405/AI-First-multica": {7}},
		tokens:       map[int64]string{7: "a", 8: "b"},
	}
	r := NewResolver(access)
	_, err := r.ResolveBindings(context.Background(), "ws-1", decls(), []RepoData{
		{URL: "https://github.com/OldBoy405/AI-First-tools.git"},
		{URL: "https://github.com/OldBoy405/AI-First-multica.git"},
	}, []GitHubInstallation{{InstallationID: 7}, {InstallationID: 8}})
	if !strings.Contains(err.Error(), ErrRepositoryAccessAmbiguous.Error()) {
		t.Fatalf("err = %v, want repository_access_ambiguous", err)
	}
}

func TestResolveBindingsAllOrNothingAndNoTokenInErrors(t *testing.T) {
	// multica resolves, tools has no installation access: whole plan fails and
	// the error text never contains token material.
	access := &fakeAccess{
		accessByRepo: map[string][]int64{"OldBoy405/AI-First-multica": {9}},
		tokens:       map[int64]string{9: "super-secret"},
	}
	r := NewResolver(access)
	_, err := r.ResolveBindings(context.Background(), "ws-1", decls(), []RepoData{
		{URL: "https://github.com/OldBoy405/AI-First-tools.git"},
		{URL: "https://github.com/OldBoy405/AI-First-multica.git"},
	}, []GitHubInstallation{{InstallationID: 9}})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaks token material: %v", err)
	}
}
