package base

import (
	"sync"
	"time"

	"github.com/hrygo/hotplex/pkg/events"
)

// EventGate serializes sends with receive-channel closure without sharing a
// worker's stdin lock. Its zero value is ready to use. A gate owns exactly one
// channel for its entire lifetime and must not be copied or reused after Close.
// Close first wakes blocked senders, then waits for in-flight sends before
// closing the channel. Panic recovery is not a substitute for this ordering.
type EventGate struct {
	initOnce  sync.Once
	closeOnce sync.Once
	mu        sync.RWMutex
	done      chan struct{}
	closed    bool
}

func (g *EventGate) init() {
	g.initOnce.Do(func() { g.done = make(chan struct{}) })
}

// TrySend returns false when the channel is full or shutdown has started.
func (g *EventGate) TrySend(ch chan *events.Envelope, env *events.Envelope) bool {
	g.init()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return false
	}
	select {
	case <-g.done:
		return false
	default:
	}
	select {
	case ch <- env:
		return true
	case <-g.done:
		return false
	default:
		return false
	}
}

// SendTimeout preserves critical-event budgets while allowing immediate shutdown.
func (g *EventGate) SendTimeout(ch chan *events.Envelope, env *events.Envelope, timeout time.Duration) bool {
	if g.TrySend(ch, env) {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ch <- env:
		return true
	case <-g.done:
		return false
	case <-timer.C:
		return false
	}
}

// Close is idempotent and leaves accepted buffered events available to readers.
func (g *EventGate) Close(ch chan *events.Envelope) {
	g.init()
	g.closeOnce.Do(func() {
		close(g.done)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.closed = true
		if ch != nil {
			close(ch)
		}
	})
}
