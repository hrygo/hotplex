package acp

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// acpConn implements worker.SessionConn for ACP workers.
// Unlike base.Conn (which wraps stdin for AEP NDJSON), acpConn only provides
// the "up" direction (readLoop → TrySend → Recv → forwardEvents).
// User input and permission responses go through client.Prompt / client.RespondPermission.
type acpConn struct {
	userID    string
	sessionID string
	log       *slog.Logger
	recvCh    chan *events.Envelope
	recvGate  base.EventGate
	mu        sync.Mutex
	closed    bool
	lastInput atomic.Pointer[string] // cached for InputRecoverer crash recovery
}

// Compile-time checks.
var (
	_ worker.SessionConn    = (*acpConn)(nil)
	_ worker.InputRecoverer = (*acpConn)(nil)
)

func newACPConn(userID, sessionID string, log *slog.Logger) *acpConn {
	return &acpConn{
		userID:    userID,
		sessionID: sessionID,
		log:       log,
		recvCh:    make(chan *events.Envelope, 256),
	}
}

// Send is not used by ACP workers — user input goes through Worker.Input → client.Prompt.
func (c *acpConn) Send(_ context.Context, msg *events.Envelope) error {
	return worker.ErrNotImplemented
}

// Recv returns the channel that forwardEvents ranges over.
func (c *acpConn) Recv() <-chan *events.Envelope {
	return c.recvCh
}

// TrySend enqueues an envelope from readLoop (backpressure-aware).
// Critical events (state/done/error/permission_request/question_request/elicitation_request)
// block until sent; droppable events (message.delta/raw) are silently discarded when full.
// EventGate synchronizes channel sends with Close, including blocked senders.
func (c *acpConn) TrySend(env *events.Envelope) bool {
	if isDroppable(env.Event.Type) {
		return c.trySendNonBlocking(env)
	}
	// Critical event: try non-blocking first, then blocking with closed-channel check.
	if c.trySendNonBlocking(env) {
		return true
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}
	return c.safeSend(env)
}

// isDroppable reports whether an event type can be silently discarded under
// backpressure. All streaming-append content (text delta, reasoning, raw) is
// droppable; the authoritative turns table reconciles on done.
// Keep in sync with: gateway/hub.isDroppable, opencodeserver.singleton.isDroppable.
func isDroppable(kind events.Kind) bool {
	return kind == events.MessageDelta || kind == events.Reasoning || kind == events.Raw
}

// trySendNonBlocking returns false if the channel is full or closing.
func (c *acpConn) trySendNonBlocking(env *events.Envelope) bool {
	return c.recvGate.TrySend(c.recvCh, env)
}

// safeSend keeps the critical-event budget, but Close wakes blocked sends before
// closing recvCh. No send/close race is hidden behind panic recovery.
func (c *acpConn) safeSend(env *events.Envelope) bool {
	return c.recvGate.SendTimeout(c.recvCh, env, 5*time.Second)
}

// Close shuts down the receive channel.
func (c *acpConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.recvGate.Close(c.recvCh)
	return nil
}

// LastInput returns the most recent user input for crash recovery re-delivery.
// Satisfies worker.InputRecoverer — Bridge's handleWorkerExit reads this to
// re-inject the last message after a worker restart.
func (c *acpConn) LastInput() string {
	if p := c.lastInput.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *acpConn) UserID() string    { return c.userID }
func (c *acpConn) SessionID() string { return c.sessionID }

func (c *acpConn) Inject(env *events.Envelope) {
	if !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) && c.log != nil {
		c.log.Warn("acp conn: inject failed, channel closed or full",
			"session_id", c.sessionID, "event_type", env.Event.Type)
	}
}
