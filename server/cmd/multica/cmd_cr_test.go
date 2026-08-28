package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AIFIRST: CR-2026-053 TASK-08 (SDD §3.3) — thin-wrapper CLI tests. The
// command must relay exactly the CR-ID + task token to the binding endpoint
// and pass the structured result through; it must not construct any identity
// body fields. Tests exercise the REAL crBindCurrentTaskCmd (flag
// registration included) — a synthetic command with hand-registered flags
// masked the missing --output registration.

func TestRunCrBindCurrentTaskSuccess(t *testing.T) {
	const crID = "CR-2026-053"
	const taskID = "22222222-2222-4222-8222-222222222222"
	const issueID = "33333333-3333-4333-8333-333333333333"
	const projectID = "44444444-4444-4444-8444-444444444444"
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Body != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k := range body {
				t.Errorf("request body must be empty, got field %q", k)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cr_id": crID, "task_id": taskID, "issue_id": issueID,
			"project_id": projectID, "changed": true,
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := crBindCurrentTaskCmd
	_ = cmd.Flags().Set("output", "json")
	out, err := captureStdout(t, func() error { return runCrBindCurrentTask(cmd, []string{crID}) })
	if err != nil {
		t.Fatalf("bind should succeed: %v", err)
	}
	if gotPath != "/api/crs/"+crID+"/bind-current-task" {
		t.Errorf("unexpected path %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer mat_") {
		t.Errorf("expected mat_ task token, got %q", gotAuth)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got["cr_id"] != crID || got["changed"] != true {
		t.Errorf("unexpected passthrough: %v", got)
	}
}

func TestRunCrBindCurrentTaskReplayChangedFalse(t *testing.T) {
	const crID = "CR-2026-053"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cr_id": crID, "task_id": "22222222-2222-4222-8222-222222222222",
			"issue_id":   "33333333-3333-4333-8333-333333333333",
			"project_id": "44444444-4444-4444-8444-444444444444",
			"changed":    false,
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := crBindCurrentTaskCmd
	_ = cmd.Flags().Set("output", "json")
	out, err := captureStdout(t, func() error { return runCrBindCurrentTask(cmd, []string{crID}) })
	if err != nil {
		t.Fatalf("same-value replay must succeed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got["changed"] != false {
		t.Errorf("expected changed=false passthrough, got %v", got["changed"])
	}
}

func TestRunCrBindCurrentTaskConflictError(t *testing.T) {
	const crID = "CR-2026-053"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "TASK_CR_CONFLICT"})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := crBindCurrentTaskCmd
	err := runCrBindCurrentTask(cmd, []string{crID})
	if err == nil {
		t.Fatalf("expected a conflict error")
	}
	if !strings.Contains(err.Error(), "TASK_CR_CONFLICT") {
		t.Errorf("error should surface the server code, got %v", err)
	}
}

func TestRunCrBindCurrentTaskRequiresArg(t *testing.T) {
	// exactArgs(1) must reject a missing CR-ID before RunE ever runs.
	if err := exactArgs(1)(crBindCurrentTaskCmd, []string{}); err == nil {
		t.Fatalf("expected an error for missing CR-ID")
	}
	if err := exactArgs(1)(crBindCurrentTaskCmd, []string{"CR-2026-053", "extra"}); err == nil {
		t.Fatalf("expected an error for extra args")
	}
}
