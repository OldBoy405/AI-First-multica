package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// AIFIRST: CR-2026-054 TASK-07 — terminal report replay tests (see CUSTOM.md).

type terminalReportHandler struct {
	mu               sync.Mutex
	completeStatus   map[string]int // per-task status for /complete; missing = 200
	failStatus       map[string]int // per-task status for /fail; missing = 200
	completeCount    int
	failCount        int
	lastCompleteBody map[string]any
	lastFailBody     map[string]any
}

func (h *terminalReportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	taskID := parts[len(parts)-2]
	status := 0
	if strings.HasSuffix(r.URL.Path, "/complete") {
		h.completeCount++
		h.lastCompleteBody = body
		status = h.completeStatus[taskID]
	} else if strings.HasSuffix(r.URL.Path, "/fail") {
		h.failCount++
		h.lastFailBody = body
		status = h.failStatus[taskID]
	} else {
		http.NotFound(w, r)
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":"server refused %d"}`, status)
		return
	}
	_, _ = fmt.Fprint(w, `{"ok":"1"}`)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTerminalReportFixture(t *testing.T) (*Daemon, *terminalReportHandler, *syncBuffer, func()) {
	t.Helper()
	h := &terminalReportHandler{completeStatus: map[string]int{}, failStatus: map[string]int{}}
	srv := httptest.NewServer(h)
	restore := noSleepRetry(t)
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	d := &Daemon{client: NewClient(srv.URL)}
	return d, h, buf, func() { slog.SetDefault(prev); restore(); srv.Close() }
}

func pendingEntry(t *testing.T, d *Daemon, taskID string) (terminalTaskReport, bool) {
	t.Helper()
	d.terminalRetry.mu.Lock()
	defer d.terminalRetry.mu.Unlock()
	v, ok := d.terminalRetry.pending[taskID]
	return v, ok
}

func pendingSize(d *Daemon) int {
	d.terminalRetry.mu.Lock()
	defer d.terminalRetry.mu.Unlock()
	return len(d.terminalRetry.pending)
}

func TestTerminalReportRetryEnqueueDedupConflictAndRemove(t *testing.T) {
	r := &terminalReportRetry{}
	first := terminalTaskReport{kind: terminalTaskReportComplete, taskID: "t1", output: "out-1"}
	if !r.enqueue(first) {
		t.Fatal("first enqueue must be accepted")
	}
	if r.enqueue(first) {
		t.Fatal("identical report must be deduplicated")
	}
	conflict := terminalTaskReport{kind: terminalTaskReportComplete, taskID: "t1", output: "out-2"}
	if r.enqueue(conflict) {
		t.Fatal("conflicting payload must keep first-wins and not replace")
	}
	snap := r.snapshot()
	if len(snap) != 1 || snap[0].output != "out-1" {
		t.Fatalf("snapshot = %+v, want single first-wins entry", snap)
	}
	if !r.removeIfUnchanged(snap[0]) {
		t.Fatal("stored snapshot value must be removable")
	}
	if r.removeIfUnchanged(conflict) {
		t.Fatal("stale conflicting value must not delete the entry")
	}
	if len(r.snapshot()) != 0 {
		t.Fatalf("pending = %+v, want empty after removal", r.snapshot())
	}
}

func TestReplayTerminalReportsOnceSuccessAndPermanentRemove(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["ok-task"] = 0 // 200
	h.completeStatus["perm-task"] = 400
	d.terminalRetry.enqueue(terminalTaskReport{kind: terminalTaskReportComplete, taskID: "ok-task"})
	d.terminalRetry.enqueue(terminalTaskReport{kind: terminalTaskReportComplete, taskID: "perm-task"})
	if !d.replayTerminalReportsOnce(context.Background()) {
		t.Fatal("round with only success/permanent outcomes must continue to completion")
	}
	if h.completeCount != 2 {
		t.Fatalf("completeCount = %d, want 2", h.completeCount)
	}
	if pendingSize(d) != 0 {
		t.Fatalf("pending size = %d, want 0 (success and permanent both remove)", pendingSize(d))
	}
}

func TestReplayTerminalReportsOnceTransientKeepsWorkerAlive(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["sick"] = 500
	d.terminalRetry.enqueue(terminalTaskReport{kind: terminalTaskReportComplete, taskID: "sick"})
	d.terminalRetry.enqueue(terminalTaskReport{kind: terminalTaskReportComplete, taskID: "healthy"})
	if !d.replayTerminalReportsOnce(context.Background()) {
		t.Fatal("transient failure must stop only the current round, not the replay worker")
	}
	if pendingSize(d) != 2 {
		t.Fatalf("pending size = %d, want 2 (transient retained, others untouched)", pendingSize(d))
	}

	// A later round is still available after the transient failure.
	h.completeStatus["sick"] = 0
	if !d.replayTerminalReportsOnce(context.Background()) {
		t.Fatal("replay worker must continue after a transient round")
	}
	if pendingSize(d) != 0 {
		t.Fatalf("pending size = %d, want 0 after the next round", pendingSize(d))
	}
}

func TestReplayTerminalReportsOnceStopsOnCancelledContext(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	d.terminalRetry.enqueue(terminalTaskReport{kind: terminalTaskReportComplete, taskID: "t"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d.replayTerminalReportsOnce(ctx) {
		t.Fatal("cancelled context must stop the round without draining")
	}
	if pendingSize(d) != 1 {
		t.Fatal("cancelled round must retain the pending entry (no drain)")
	}
	if h.completeCount != 0 {
		t.Fatal("cancelled round must not deliver anything")
	}
}

func TestReportTerminalTaskTransientEnqueuesAndWraps(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t1"] = 500
	report := terminalTaskReport{
		kind: terminalTaskReportComplete, taskID: "t1",
		output: "secret-output", sessionID: "secret-session", workDir: "secret-workdir",
	}
	err := d.reportTerminalTask(context.Background(), report)
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if !isTransientError(err) {
		t.Fatal("Unwrap must preserve transient classification")
	}
	var f *terminalReportFailure
	if !errors.As(err, &f) {
		t.Fatalf("error %T must wrap terminalReportFailure", err)
	}
	if f.taskID != "t1" || f.kind != terminalTaskReportComplete || f.class != terminalReportErrorTransient {
		t.Fatalf("wrapper fields = %+v", f)
	}
	stored, ok := pendingEntry(t, d, "t1")
	if !ok || stored.output != "secret-output" {
		t.Fatalf("pending entry = %+v, want immutable copy of original report", stored)
	}
	// Value-copy boundary: mutating the caller's struct must not reach pending.
	report.output = "mutated"
	if stored, _ := pendingEntry(t, d, "t1"); stored.output != "secret-output" {
		t.Fatal("pending map must hold an immutable value copy")
	}
}

func TestReportTerminalTaskPermanentNotEnqueued(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t2"] = 400
	err := d.reportTerminalTask(context.Background(), terminalTaskReport{kind: terminalTaskReportComplete, taskID: "t2", output: "o"})
	if err == nil || isTransientError(err) {
		t.Fatalf("expected permanent wrapped error, got %v", err)
	}
	if pendingSize(d) != 0 {
		t.Fatal("permanent errors must never be enqueued")
	}
}

func TestReportTaskResultCompleteTransientNoFallback(t *testing.T) {
	d, h, buf, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t3"] = 500
	d.reportTaskResult(context.Background(), "t3", TaskResult{
		Status: "completed", Comment: "done-output", BranchName: "b3", SessionID: "s3", WorkDir: "w3", DurableWorkDir: "d3",
	}, slog.New(slog.NewJSONHandler(buf, nil)))
	if h.failCount != 0 {
		t.Fatalf("failCount = %d, want 0 (transient complete must never fall back to fail)", h.failCount)
	}
	stored, ok := pendingEntry(t, d, "t3")
	if !ok || stored.kind != terminalTaskReportComplete || stored.output != "done-output" {
		t.Fatalf("pending = %+v, want enqueued complete report with original output", stored)
	}
	if strings.Contains(buf.String(), "done-output") || strings.Contains(buf.String(), "s3") || strings.Contains(buf.String(), "server refused") {
		t.Fatalf("logs leak terminal payload or cause:\n%s", buf.String())
	}
}

func TestReportTaskResultCompletePermanentFallsBackOnce(t *testing.T) {
	d, h, buf, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t4"] = 400
	d.reportTaskResult(context.Background(), "t4", TaskResult{
		Status: "completed", Comment: "agent-result-secret", BranchName: "b4", SessionID: "s4", WorkDir: "w4", DurableWorkDir: "d4",
	}, slog.New(slog.NewJSONHandler(buf, nil)))
	if h.failCount != 1 {
		t.Fatalf("failCount = %d, want exactly 1 fallback", h.failCount)
	}
	if h.lastFailBody["branch_name"] != "b4" || h.lastFailBody["session_id"] != "s4" ||
		h.lastFailBody["work_dir"] != "w4" || h.lastFailBody["durable_work_dir"] != "d4" {
		t.Fatalf("fallback fail payload lost fields: %v", h.lastFailBody)
	}
	msg, _ := h.lastFailBody["error"].(string)
	if !strings.HasPrefix(msg, "complete task failed:") || !strings.Contains(msg, "400") {
		t.Fatalf("fallback errorMessage must carry the original cause, got %q", msg)
	}
	if pendingSize(d) != 0 {
		t.Fatal("successful fallback must leave no pending entry")
	}
	if strings.Contains(buf.String(), "agent-result-secret") || strings.Contains(buf.String(), "server refused 400") {
		t.Fatalf("logs leak original cause or payload:\n%s", buf.String())
	}
}

func TestReportTaskResultFallbackFailTransientEnqueuesFail(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t5"] = 400
	h.failStatus["t5"] = 500
	d.reportTaskResult(context.Background(), "t5", TaskResult{Status: "completed", Comment: "c5"}, slog.Default())
	stored, ok := pendingEntry(t, d, "t5")
	if !ok || stored.kind != terminalTaskReportFail || !strings.Contains(stored.errorMessage, "complete task failed:") {
		t.Fatalf("pending = %+v, want enqueued fail report with preserved errorMessage", stored)
	}
	if pendingSize(d) != 1 {
		t.Fatal("only the transient fail report may be enqueued")
	}
}

func TestReportTaskResultFallbackFailPermanentNotEnqueued(t *testing.T) {
	d, h, _, stop := newTerminalReportFixture(t)
	defer stop()
	h.completeStatus["t6"] = 400
	h.failStatus["t6"] = 400
	d.reportTaskResult(context.Background(), "t6", TaskResult{Status: "completed", Comment: "c6"}, slog.Default())
	if pendingSize(d) != 0 {
		t.Fatal("permanent fallback failure must not be enqueued")
	}
}

func TestTerminalReportFailureLogValueExposesOnlyThreeFields(t *testing.T) {
	_, _, buf, stop := newTerminalReportFixture(t)
	defer stop()
	f := &terminalReportFailure{
		cause: errors.New("boom-secret"), taskID: "t7",
		kind: terminalTaskReportFail, class: terminalReportErrorPermanent,
	}
	slog.Default().Warn("probe", "error", f)
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Split(buf.String(), "\n")[0])), &rec); err != nil {
		t.Fatalf("log record not JSON: %v", err)
	}
	group, ok := rec["error"].(map[string]any)
	if !ok {
		t.Fatalf("error attr = %v, want grouped LogValue", rec["error"])
	}
	if len(group) != 3 || group["task_id"] != "t7" || group["terminal_kind"] != "fail" || group["error_class"] != "permanent" {
		t.Fatalf("error group = %v, want exactly task_id/terminal_kind/error_class", group)
	}
	if strings.Contains(buf.String(), "boom-secret") {
		t.Fatalf("log expanded the wrapped cause:\n%s", buf.String())
	}
}
