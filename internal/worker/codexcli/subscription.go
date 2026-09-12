package codexcli

import (
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// codexSubscription owns one receive channel for its entire lifetime. The
// manager and appConn share this object; removing a routing entry cannot
// transfer ownership of its channel to a replacement subscription.
type codexSubscription struct {
	ch   chan *events.Envelope
	gate base.EventGate
}

func (s *codexSubscription) close() { s.gate.Close(s.ch) }
