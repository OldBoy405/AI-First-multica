package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AIFIRST: CR-2026-054 TASK-05 — process-local terminal report replay core.
// See CUSTOM.md for the fork-customization ledger entry.

// terminalReportReplayPeriod is the fixed cadence of the single replay
// worker. A round that is cut short by a transient error is retried from a
// fresh snapshot on the next tick; a slow round simply delays the next one.
const terminalReportReplayPeriod = 30 * time.Second

// terminalReportErrorClass classifies the final error of one terminal
// delivery attempt.
type terminalReportErrorClass uint8

const (
	terminalReportErrorTransient terminalReportErrorClass = iota + 1
	terminalReportErrorPermanent
)

// classifyTerminalError maps the existing transport-level predicate onto the
// two stable classes used by the replay set and the safe error wrapper.
func classifyTerminalError(err error) terminalReportErrorClass {
	if isTransientError(err) {
		return terminalReportErrorTransient
	}
	return terminalReportErrorPermanent
}

// terminalReportFailure is the safe error wrapper returned by
// reportTerminalTask when the final delivery attempt fails. Unwrap keeps the
// original cause available for errors.As / isTransientError and for the
// existing complete→fail fallback path, while LogValue reduces the structured
// log representation to three stable sanitized fields so the original cause,
// errorMessage, output, session/workdir and the full terminal payload never
// reach the logger.
type terminalReportFailure struct {
	cause  error
	taskID string
	kind   terminalTaskReportKind
	class  terminalReportErrorClass
}

// Error preserves the functional text of the original cause. The existing
// complete→fail fallback uses it to build the server-side FailTask
// errorMessage; it must never be handed to a logger directly.
func (f *terminalReportFailure) Error() string {
	if f == nil || f.cause == nil {
		return "terminal report delivery failed"
	}
	return f.cause.Error()
}

// Unwrap restores the original cause so structured classification keeps
// working through the wrapper.
func (f *terminalReportFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.cause
}

// LogValue exposes exactly three stable fields — task_id, terminal_kind,
// error_class — and nothing else, so slog cannot expand the wrapped cause.
func (f *terminalReportFailure) LogValue() slog.Value {
	if f == nil {
		return slog.GroupValue()
	}
	return slog.GroupValue(
		slog.String("task_id", f.taskID),
		slog.String("terminal_kind", terminalKindString(f.kind)),
		slog.String("error_class", terminalErrorClassString(f.class)),
	)
}

func terminalKindString(kind terminalTaskReportKind) string {
	switch kind {
	case terminalTaskReportComplete:
		return "complete"
	case terminalTaskReportFail:
		return "fail"
	default:
		return "unknown"
	}
}

func terminalErrorClassString(class terminalReportErrorClass) string {
	switch class {
	case terminalReportErrorTransient:
		return "transient"
	case terminalReportErrorPermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// copyImmutable returns a value copy of the report. Every field of
// terminalTaskReport is a Go value type (enums, booleans, strings), so the
// assignment itself is the copy; the helper exists so a future refactor that
// introduces slice/map/pointer fields has a single boundary to convert into a
// deep copy instead of storing a caller-mutable reference in pending.
func copyImmutable(report terminalTaskReport) terminalTaskReport {
	return report
}

// terminalReportRetry is a zero-value-ready, process-local pending set for
// terminal reports whose final delivery attempt ended in a transient error.
// One entry per task ID, first-wins on conflicting payloads. No persistence,
// capacity limit, backpressure or worker scheduling — the daemon root context
// owns the single replay goroutine, and a process exit drops the set while
// existing orphan recovery converges tasks the daemon never reached.
type terminalReportRetry struct {
	mu      sync.Mutex
	pending map[string]terminalTaskReport // task ID -> first accepted immutable report
	once    sync.Once                     // starts the single replay loop
}

// enqueue records the first report accepted for a task. An identical report
// is silently deduplicated; a conflicting payload keeps the first value
// (first-wins) and is reported through the sanitized replay log. Reports true
// when the entry was newly accepted (the caller uses that to start the loop).
func (r *terminalReportRetry) enqueue(report terminalTaskReport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = make(map[string]terminalTaskReport)
	}
	prev, ok := r.pending[report.taskID]
	if ok {
		if prev == report {
			return false
		}
		logTerminalReplay("conflict", report.taskID, report.kind, nil)
		return false
	}
	r.pending[report.taskID] = report
	return true
}

// snapshot returns a value copy of the current pending set.
func (r *terminalReportRetry) snapshot() []terminalTaskReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]terminalTaskReport, 0, len(r.pending))
	for _, report := range r.pending {
		out = append(out, report)
	}
	return out
}

// removeIfUnchanged deletes the entry only when the stored report still
// equals the snapshot value, so a stale round can never delete a newer value
// that raced in after the snapshot was taken.
func (r *terminalReportRetry) removeIfUnchanged(report terminalTaskReport) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[report.taskID] != report {
		return false
	}
	delete(r.pending, report.taskID)
	return true
}

// logTerminalReplay writes one sanitized replay log line. When err is nil the
// record carries task_id, terminal_kind and a fixed event name; otherwise the
// error attribute is the safe terminalReportFailure wrapper, so the original
// cause can never be formatted into the log.
func logTerminalReplay(event string, taskID string, kind terminalTaskReportKind, err error) {
	attrs := []any{"task_id", taskID, "terminal_kind", terminalKindString(kind), "event", event}
	if err != nil {
		attrs = append(attrs, "error", &terminalReportFailure{cause: err, taskID: taskID, kind: kind, class: classifyTerminalError(err)})
	}
	slog.Default().Warn("terminal report replay", attrs...)
}

// deliverTerminalTaskReport sends exactly one terminal report of the given
// kind through the existing client with its finite retry contract. It never
// enqueues and never decides complete→fail fallback; the replay loop calls
// this directly so a transient replay failure cannot recurse back into
// reportTerminalTask and re-enqueue the same payload.
func (d *Daemon) deliverTerminalTaskReport(ctx context.Context, report terminalTaskReport) error {
	switch report.kind {
	case terminalTaskReportComplete:
		return d.client.CompleteTask(ctx, report.taskID, report.output, report.branchName, report.sessionID, report.workDir, report.sessionRolloutMissing, report.retiredSessionID, report.durableWorkDir)
	case terminalTaskReportFail:
		return d.client.FailTask(ctx, report.taskID, report.errorMessage, report.sessionID, report.workDir, report.branchName, report.failureReason, report.sessionRolloutMissing, report.retiredSessionID, report.durableWorkDir)
	default:
		return fmt.Errorf("unsupported terminal task report kind %d", report.kind)
	}
}

// terminalReplayLoop runs the single 30-second replay worker until the daemon
// root context is cancelled. A transient delivery failure ends only the current
// round; the next ticker starts another snapshot round.
func (d *Daemon) terminalReplayLoop(ctx context.Context) {
	ticker := time.NewTicker(terminalReportReplayPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !d.replayTerminalReportsOnce(ctx) {
				return
			}
		}
	}
}

// replayTerminalReportsOnce delivers one snapshot round with the single
// worker. Success and permanent failures remove the entry (re-checking the
// stored value so a stale snapshot can never delete a newer conflicting
// value); the first transient failure keeps the entry, logs the safe error
// and stops the round so an unhealthy endpoint is not hammered until the
// next fixed period.
// replayTerminalReportsOnce returns false only when the worker should stop
// because its root context is cancelled. A transient delivery failure stops
// the current round but leaves the worker alive for the next fixed tick.
func (d *Daemon) replayTerminalReportsOnce(ctx context.Context) bool {
	for _, report := range d.terminalRetry.snapshot() {
		if ctx.Err() != nil {
			return false
		}
		deliverCtx, cancel := context.WithTimeout(ctx, terminalTaskReportTimeout)
		err := d.deliverTerminalTaskReport(deliverCtx, report)
		cancel()
		switch {
		case err == nil:
			d.terminalRetry.removeIfUnchanged(report)
			logTerminalReplay("replayed", report.taskID, report.kind, nil)
		case ctx.Err() != nil:
			return false
		case isTransientError(err):
			logTerminalReplay("retry-later", report.taskID, report.kind, err)
			return true
		default:
			d.terminalRetry.removeIfUnchanged(report)
			logTerminalReplay("dropped", report.taskID, report.kind, err)
		}
	}
	return true
}
