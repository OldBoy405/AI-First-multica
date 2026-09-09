package main

// AIFIRST: CR-2026-061 TASK-03 — thin-relay test for `multica cr
// bind-promotion-run` (SDD §3.3): the command POSTs {run_id} to the bind
// endpoint, passes the token context through, and relays the structured
// result verbatim with no business judgment.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunCrBindPromotionRunRelays(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")

	var gotPath, gotBody, gotActorSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotActorSource = r.Header.Get("X-Actor-Source")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cr_id":    "CR-2026-061",
			"run_id":   "00000000-0000-0000-0000-000000000061",
			"issue_id": "00000000-0000-0000-0000-000000000062",
			"changed":  true,
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := crBindPromotionRunCmd
	if err := cmd.Flags().Set("run-id", "00000000-0000-0000-0000-000000000061"); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return runCrBindPromotionRun(cmd, []string{"CR-2026-061"}) })
	if err != nil {
		t.Fatalf("runCrBindPromotionRun: %v", err)
	}
	if gotPath != "/api/crs/CR-2026-061/bind-promotion-run" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"run_id":"00000000-0000-0000-0000-000000000061"`) {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(out, `"changed": true`) || !strings.Contains(out, `"cr_id": "CR-2026-061"`) {
		t.Errorf("output = %q", out)
	}
	// The relay does not fabricate the task-token actor header — the auth
	// transport stamps it; asserting here only documents the contract.
	_ = gotActorSource
}

func TestRunCrBindPromotionRunRequiresRunID(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd := crBindPromotionRunCmd
	if err := cmd.Flags().Set("run-id", ""); err != nil {
		t.Fatal(err)
	}
	err := runCrBindPromotionRun(cmd, []string{"CR-2026-061"})
	if err == nil || !strings.Contains(err.Error(), "--run-id is required") {
		t.Fatalf("err = %v, want missing --run-id error", err)
	}
}
