package base

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// Conn implements worker.SessionConn for stdin-based workers (claudecode, opencodeserver).
type Conn struct {
	userID    string
	sessionID string
	stdin     *os.File
	// stateMu protects sessionID and stdin pointer snapshots, never pipe I/O.
	// Pointer mutation holds mu then stateMu; readers using mu alone remain safe.
	stateMu   sync.RWMutex
	recvCh    chan *events.Envelope
	recvGate  EventGate
	log       *slog.Logger
	mu        sync.Mutex
	closed    bool
	lastInput string // last user message content; used for crash recovery re-delivery
}

// NewConn creates a new stdin-based session connection.
func NewConn(log *slog.Logger, stdin *os.File, userID, sessionID string) *Conn {
	if log == nil {
		log = slog.Default()
	}
	return &Conn{
		userID:    userID,
		sessionID: sessionID,
		stdin:     stdin,
		recvCh:    make(chan *events.Envelope, 256),
		log:       log,
	}
}

// Send delivers a message to the worker runtime via stdin using NDJSON encoding.
func (c *Conn) Send(ctx context.Context, msg *events.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: "base: connection closed"}
	}

	// Write NDJSON to stdin while holding the lock to prevent interleaving
	// with ControlHandler writes on the same fd.
	if err := aep.Encode(c.stdin, msg); err != nil {
		if IsDeadProcessError(err) {
			return &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: "base: worker process is not running or stdin is closed", Cause: err}
		}
		return fmt.Errorf("base: encode: %w", err)
	}

	return nil
}

// Recv returns a channel that yields messages from the worker runtime.
func (c *Conn) Recv() <-chan *events.Envelope {
	return c.recvCh
}

func (c *Conn) TrySend(env *events.Envelope) bool {
	return c.recvGate.TrySend(c.recvCh, env)
}

// Inject enqueues a synthetic event for in-place reset signaling.
// Blocks up to 2s waiting for channel space; logs a warning if dropped.
func (c *Conn) Inject(env *events.Envelope) {
	if !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) {
		c.log.Warn("base: inject failed, channel closed or full",
			"session_id", c.SessionID(), "event_type", env.Event.Type)
	}
}

// InjectWithTimeout sends env into ch with a 2s timeout.
// Shared by Conn.Inject, ACP conn.Inject, and OCS conn.Inject
// so that internal_reset events are never silently dropped under normal load.
// Uses recover() to guard against send-on-closed-channel if Close() races with Inject.
func InjectWithTimeout(ch chan<- *events.Envelope, env *events.Envelope, log *slog.Logger, sessionID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("base: inject failed, channel closed",
				"session_id", sessionID, "event_type", env.Event.Type)
		}
	}()
	select {
	case ch <- env:
		return
	default:
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case ch <- env:
	case <-timer.C:
		log.Warn("base: inject dropped after timeout, recv channel full",
			"session_id", sessionID, "event_type", env.Event.Type)
	}
}

// WriteMu returns the mutex that protects stdin writes.
// ControlHandler should use this same mutex to serialize stdin access.
func (c *Conn) WriteMu() *sync.Mutex {
	return &c.mu
}

// Stdin returns the underlying stdin file.
//
// Deprecated: Use StdinLocked() instead. The returned *os.File is unprotected
// after the internal mutex is released, making it unsafe for concurrent writes.
func (c *Conn) Stdin() *os.File {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdin
}

// StdinLocked returns the stdin file and the protecting mutex (locked).
// Caller must unlock the returned mutex after completing all operations.
// Use for atomic stdin-read + write + SetLastInput sequences.
func (c *Conn) StdinLocked() (*os.File, *sync.Mutex) {
	c.mu.Lock()
	return c.stdin, &c.mu
}

// StdinUnlocked returns the stdin file and its protecting mutex without
// locking. The caller MUST acquire the returned mutex before writing to stdin
// (use WriteMu for the same lock). This is intended for the ctx-guarded write
// pattern where the lock is taken inside a closure that may outlive the caller
// (see base.WriteWithCtx): acquiring the lock at the call site would risk
// releasing it via defer while the write goroutine is still in flight.
//
// The pointer snapshot uses a separate, short-lived state lock. It must not
// wait behind an orphaned pipe write holding mu before WriteWithCtx can apply
// the caller's cancellation budget. CloseInput may subsequently close the file;
// callers must still hold the returned write mutex while writing.
func (c *Conn) StdinUnlocked() (*os.File, *sync.Mutex) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.stdin, &c.mu
}

// SetLastInput records the content of the most recent user message.
// Worker adapters should call this when they deliver user input through
// protocol-specific channels, so the bridge crash recovery mechanism
// can re-deliver the message after a resume failure.
func (c *Conn) SetLastInput(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastInput = content
}

// SetLastInputLocked sets lastInput. Caller must hold c.mu.
func (c *Conn) SetLastInputLocked(content string) {
	c.lastInput = content
}

// CloseInput closes the stdin pipe to signal EOF to the worker process.
// The worker will finish processing buffered input and exit.
// Safe to call multiple times; sets stdin to nil after close.
func (c *Conn) CloseInput() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateMu.Lock()
	stdin := c.stdin
	c.stdin = nil
	c.stateMu.Unlock()
	if stdin != nil {
		return stdin.Close()
	}
	return nil
}

// Close terminates the connection and releases resources.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	c.recvGate.Close(c.recvCh)

	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	return nil
}

// UserID returns the user who owns this session.
func (c *Conn) UserID() string {
	return c.userID
}

// SessionID returns the session identifier.
func (c *Conn) SessionID() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.sessionID
}

// SetSessionID updates the session identifier (for opencodeserver's session ID extraction).
func (c *Conn) SetSessionID(id string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.sessionID = id
}

// LastInput returns the content of the most recent user message.
// Used by bridge crash recovery to re-deliver input to a fresh worker after resume failure.
func (c *Conn) LastInput() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastInput
}
