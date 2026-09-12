// Package base provides shared infrastructure for CLI-based worker adapters.
package base

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/proc"
)

// Compile-time interface compliance checks.
var (
	_ worker.SessionConn = (*Conn)(nil)
)

// Grace period for graceful worker shutdown.
// Canonical constant lives in proc.DefaultGracePeriod; referenced here for
// backward compatibility with existing callers.
const GracefulShutdownTimeout = proc.DefaultGracePeriod

// BaseWorker provides shared lifecycle methods for CLI-based worker adapters.
// Embed this struct to get Terminate/Kill/Wait/Health/LastIO/Conn for free.
type BaseWorker struct {
	Log *slog.Logger
	// Proc is the process manager. Subclasses should initialize via w.Proc = proc.New(opts).
	Proc      *proc.Manager
	Cfg       *config.Config
	Cmd       *exec.Cmd // Reserved for future use; currently Proc manages the command
	StartTime time.Time
	lastIO    atomic.Int64 // unix nano, use LastIO() and SetLastIO() accessors
	Mu        sync.Mutex
	conn      *Conn // stdin-based conn, nil for HTTP-based adapters

	// resetGen is a monotonic counter incremented before each deliberate
	// Terminate+Start cycle (e.g. session reset). forwardEvents captures the
	// value at goroutine start and checks after the recv channel closes: if the
	// current generation differs from its captured value, another reset happened
	// and the OLD forwardEvents should exit cleanly without crash handling.
	// This replaces the previous boolean flag which suffered from a race where
	// ResetSession reset the flag to false before OLD forwardEvents could check it.
	resetGen atomic.Int64
	stopped  atomic.Bool
}

// NewBaseWorker creates a new BaseWorker with the given logger and config.
func NewBaseWorker(log *slog.Logger, cfg *config.Config) *BaseWorker {
	if log == nil {
		log = slog.Default()
	}
	return &BaseWorker{
		Log: log,
		Cfg: cfg,
	}
}

// withProcResult executes fn with a snapshot of w.Proc. If Proc is nil,
// returns (zero, errNil). On success, Proc is cleared only if it is still the snapshot.
// This eliminates the repeated lock-snapshot-nil-clear pattern.
func withProcResult[T any](w *BaseWorker, fn func(*proc.Manager) (T, error), zero T, errNil error) (T, error) {
	w.Mu.Lock()
	p := w.Proc
	w.Mu.Unlock()

	if p == nil {
		return zero, errNil
	}

	result, err := fn(p)
	if err != nil {
		return result, err
	}

	w.Mu.Lock()
	// A reset may have installed a replacement while fn waited for the old
	// process. Only the operation that still owns Proc may clear it.
	if w.Proc == p {
		w.Proc = nil
	}
	w.Mu.Unlock()

	return result, nil
}

// withProc executes fn with a snapshot of w.Proc. If Proc is nil, returns nil.
func (w *BaseWorker) withProc(fn func(*proc.Manager) error) error {
	_, err := withProcResult(w, func(p *proc.Manager) (struct{}, error) {
		return struct{}{}, fn(p)
	}, struct{}{}, nil)
	return err
}

// withProcCode is the (int, error) variant of withProc, used by Wait.
func (w *BaseWorker) withProcCode(fn func(*proc.Manager) (int, error)) (int, error) {
	return withProcResult(w, fn, -1, fmt.Errorf("base: not started"))
}

// Terminate gracefully stops the worker process: SIGTERM → 5s grace → SIGKILL.
func (w *BaseWorker) Terminate(ctx context.Context) error {
	return w.withProc(func(p *proc.Manager) error {
		if err := p.Terminate(ctx, GracefulShutdownTimeout); err != nil {
			return fmt.Errorf("base: terminate: %w", err)
		}
		return nil
	})
}

// Kill immediately terminates the worker process with SIGKILL.
func (w *BaseWorker) Kill() error {
	return w.withProc(func(p *proc.Manager) error {
		if err := p.Kill(); err != nil {
			return fmt.Errorf("base: kill: %w", err)
		}
		return nil
	})
}

// Wait blocks until the worker process exits, returning the exit code.
func (w *BaseWorker) Wait() (int, error) {
	return w.withProcCode(func(p *proc.Manager) (int, error) {
		code, err := p.Wait()
		if err != nil {
			return code, fmt.Errorf("base: wait: %w", err)
		}
		return code, nil
	})
}

// Health returns a snapshot of the worker's runtime health.
func (w *BaseWorker) Health(typ worker.WorkerType) worker.WorkerHealth {
	w.Mu.Lock()
	defer w.Mu.Unlock()

	health := worker.WorkerHealth{
		Type:      typ,
		SessionID: "",
		Running:   false,
		Healthy:   true,
		Uptime:    "0s",
	}

	if w.conn != nil {
		health.SessionID = w.conn.SessionID()
	}

	if w.Proc == nil {
		return health
	}

	health.PID = w.Proc.PID()
	health.Running = w.Proc.IsRunning()

	if !w.StartTime.IsZero() {
		health.Uptime = time.Since(w.StartTime).Round(time.Second).String()
	}

	return health
}

// LastIO returns the time of the last I/O activity (input sent or output received).
func (w *BaseWorker) LastIO() time.Time {
	nano := w.lastIO.Load()
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// SetLastIO atomically stores the last I/O time.
func (w *BaseWorker) SetLastIO(t time.Time) {
	w.lastIO.Store(t.UnixNano())
}

// SetConn sets the session connection after Start.
func (w *BaseWorker) SetConn(c *Conn) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	w.conn = c
}

// SetConnLocked sets the session connection without acquiring the mutex.
// Caller must hold w.Mu.
func (w *BaseWorker) SetConnLocked(c *Conn) {
	w.conn = c
}

// Conn returns the session connection, or nil if not started.
func (w *BaseWorker) Conn() worker.SessionConn {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.conn == nil {
		return nil
	}
	return w.conn
}

// IncResetGeneration increments the reset generation counter and returns the new value.
// Called by Bridge.ResetSession before the Terminate+Start cycle.
func (w *BaseWorker) IncResetGeneration() int64 { return w.resetGen.Add(1) }

// LoadResetGeneration returns the current reset generation counter.
// forwardEvents captures this at goroutine start to detect resets.
func (w *BaseWorker) LoadResetGeneration() int64 { return w.resetGen.Load() }

// IsStopped returns true if the worker was stopped by user via StopCurrentTurn.
func (w *BaseWorker) IsStopped() bool { return w.stopped.Load() }

// MarkStopped marks the worker as stopped by user.
func (w *BaseWorker) MarkStopped() { w.stopped.Store(true) }

// ClearStopped unmarks a worker previously marked stopped. Used when a stop
// attempt fails (OCS abort HTTP error, ACP cancel RPC failure, codex InterruptTurn
// failure, claudecode kill failure): the gateway rolls back its stop fence and
// sends an error, so the turn is still running and its legitimate terminal event
// must flow normally rather than being suppressed or misread as a user-stop by
// the crash fallback.
func (w *BaseWorker) ClearStopped() { w.stopped.Store(false) }

// BeginTurn clears the user-stop marker for a new primary turn. Called by each
// adapter's Input once the input is confirmed as primary content (DispatchMetadata
// returned handled=false). Adapters with fast sends (claudecode, codexcli) call
// it after the protocol send succeeds; adapters with blocking sends that span
// the whole turn (acp Prompt, OCS message POST) capture the prior stopped
// state, clear it before the send, and restore it if the send fails. Either
// way, a failed send leaves the marker set so the previous stop is preserved
// (the bridge crash fallback must not re-run a stopped turn), and a successful
// send leaves it cleared for the new turn. The marker is NOT cleared for
// interaction-response metadata, metadata dispatch errors, or absent
// connections, and NOT in InjectMidTurn.
func (w *BaseWorker) BeginTurn() { w.stopped.Store(false) }
