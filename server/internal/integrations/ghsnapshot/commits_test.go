package ghsnapshot

// AIFIRST: CR-2026-049 TASK-09 — commits/access extension tests (fake GitHub).
// Asserts: single-installation Contents:Read resolution, missing/ambiguous
// sentinels, sha query encoding (branch with slash is a value, not a path
// part), subject LF-normalization, structured errors without token material.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func mintTokenEndpoint(w http.ResponseWriter, r *http.Request, token string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "expires_at": "2030-01-01T00:00:00Z"})
}

func TestResolveRepositoryAccessSingleInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			mintTokenEndpoint(w, r, "tok-1")
		case strings.HasPrefix(r.URL.Path, "/repos/OldBoy405/AI-First-tools/contents/"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	id, err := c.ResolveRepositoryAccess(context.Background(), []int64{42}, "OldBoy405", "AI-First-tools")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != 42 {
		t.Errorf("installation = %d, want 42", id)
	}
}

func TestResolveRepositoryAccessMissingAndAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			mintTokenEndpoint(w, r, "tok-x")
		default:
			// No installation can read this repo.
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	_, err := c.ResolveRepositoryAccess(context.Background(), []int64{1, 2}, "OldBoy405", "AI-First-tools")
	if !errors.Is(err, ErrRepositoryAccessMissing) {
		t.Fatalf("err = %v, want repository_access_missing", err)
	}

	// Ambiguous: two installations both have access.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app/installations/") {
			mintTokenEndpoint(w, r, "tok-y")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer srv2.Close()
	c2 := newTestClient(t, srv2.URL)
	_, err = c2.ResolveRepositoryAccess(context.Background(), []int64{1, 2}, "OldBoy405", "AI-First-tools")
	if !errors.Is(err, ErrRepositoryAccessAmbiguous) {
		t.Fatalf("err = %v, want repository_access_ambiguous", err)
	}
}

func TestListCommitsEncodesBranchAndNormalizesSubject(t *testing.T) {
	var gotQuery atomic.Value
	gotQuery.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"sha": "abc123", "commit": map[string]string{"message": "feat(x): first line\r\nsecond line\r\n"}},
		})
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	commits, err := c.ListCommits(context.Background(), "tok", "o", "r", "custom/main", ListCommitsOptions{Page: 2, PerPage: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(commits) != 1 || commits[0].SHA != "abc123" || commits[0].Subject != "feat(x): first line" {
		t.Fatalf("commits = %+v", commits)
	}
	q, _ := url.ParseQuery(gotQuery.Load().(string))
	if q.Get("sha") != "custom/main" || q.Get("page") != "2" || q.Get("per_page") != "100" {
		t.Errorf("query = %s", gotQuery.Load())
	}
}

func TestListCommitsErrorsCarryNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	_, err := c.ListCommits(context.Background(), "super-secret-token", "o", "r", "main", ListCommitsOptions{Page: 1, PerPage: 100})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-token") || strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("error leaks token material: %v", err)
	}
}

func TestHeadAndPageViaCommitSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page := q.Get("page")
		if page == "1" && q.Get("per_page") == "1" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": "head-b", "commit": map[string]string{"message": "head"}}})
			return
		}
		if q.Get("sha") == "head-b" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"sha": "mid-2", "commit": map[string]string{"message": "feat(2)"}},
				{"sha": "mid-1", "commit": map[string]string{"message": "feat(1)"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	var cs CommitSource = c
	head, err := cs.Head(context.Background(), "tok", "o", "r", "main")
	if err != nil || head != "head-b" {
		t.Fatalf("head = %q err=%v", head, err)
	}
	page, err := cs.Page(context.Background(), "tok", "o", "r", "main", "head-b", 1, 100)
	if err != nil || len(page) != 2 || page[0].SHA != "mid-2" {
		t.Fatalf("page = %+v err=%v", page, err)
	}
}
