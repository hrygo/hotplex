package codexcli

import (
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// Gates exist only for active subscriptions. A caller retaining an old channel
// cannot resurrect it or close a replacement; the channel itself is its identity.
func (m *CodexAppServerManager) subscriberGate(ch chan *events.Envelope) *base.EventGate {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return m.subscriberGateLocked(ch)
}
func (m *CodexAppServerManager) subscriberGateLocked(ch chan *events.Envelope) *base.EventGate {
	if gate := m.subGates[ch]; gate != nil {
		return gate
	}
	// Also support internal fixtures that install an active channel directly.
	for _, active := range m.subscribers {
		if active == ch {
			if m.subGates == nil {
				m.subGates = make(map[chan *events.Envelope]*base.EventGate)
			}
			gate := &base.EventGate{}
			m.subGates[ch] = gate
			return gate
		}
	}
	return nil
}
func (m *CodexAppServerManager) closeSubscriber(ch chan *events.Envelope) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	m.closeSubscriberLocked(ch)
}
func (m *CodexAppServerManager) closeSubscriberLocked(ch chan *events.Envelope) {
	gate := m.subscriberGateLocked(ch)
	if gate == nil {
		return
	}
	// EventGate wakes blocked critical senders before closing recvCh. It does
	// not need subMu to complete, so this metadata lock cannot deadlock it.
	gate.Close(ch)
	for id, active := range m.subscribers {
		if active == ch {
			delete(m.subscribers, id)
			delete(m.subSessions, id)
		}
	}
	delete(m.subGates, ch)
}
func (m *CodexAppServerManager) closeAllSubscribersLocked() {
	for _, ch := range m.subscribers {
		m.closeSubscriberLocked(ch)
	}
}
