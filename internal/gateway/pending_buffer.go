package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// maxPendingPerSession caps buffered supplements per session to bound memory.
const maxPendingPerSession = 20

// maxAcceptedSupplementsPerSession bounds reconnect/idempotency memory while
// retaining enough recent client message IDs to suppress normal retry bursts.
const maxAcceptedSupplementsPerSession = 128

type pendingEntry struct {
	content    string
	envelope   *events.Envelope
	receivedAt int64
	count      int
}

// PendingBuffer holds user supplements that arrived while a session was busy,
// keyed by sessionID. Only used for workers that do NOT implement
// MidTurnInjector (acp/ocs fallback); mid-turn-capable workers inject directly.
type PendingBuffer struct {
	mu         sync.Mutex
	items      map[string][]pendingEntry
	accepted   map[string]*acceptedSupplements
	lifecycles map[string]*pendingLifecycle
}

type pendingReplayToken struct {
	lifecycle *pendingLifecycle
	count     int
}

type pendingLifecycle struct {
	ctx      context.Context
	cancel   context.CancelFunc
	inFlight int
}

type supplementDisposition uint8

const (
	supplementNew supplementDisposition = iota
	supplementInjected
	supplementBuffered
	// supplementQueued means the input is durably queued and will be dispatched
	// later. It is a committed disposition like the others: a retry must see
	// the same durable answer rather than being admitted a second time.
	supplementQueued
	supplementNormal
	supplementConflict
	supplementCapacity
)

type supplementRecord struct {
	mu          sync.Mutex
	payloadHash string
	state       supplementDisposition
}

type acceptedSupplements struct {
	records map[string]*supplementRecord
	order   []string
}

type supplementLease struct {
	buffer    *PendingBuffer
	sessionID string
	messageID string
	record    *supplementRecord
}

func NewPendingBuffer() *PendingBuffer {
	return &PendingBuffer{
		items:      make(map[string][]pendingEntry),
		accepted:   make(map[string]*acceptedSupplements),
		lifecycles: make(map[string]*pendingLifecycle),
	}
}

func (p *PendingBuffer) ensureLifecycleLocked(sessionID string) {
	if p.lifecycles[sessionID] == nil {
		ctx, cancel := context.WithCancel(context.Background())
		p.lifecycles[sessionID] = &pendingLifecycle{ctx: ctx, cancel: cancel}
	}
}

// BeginSupplement serializes concurrent retries of one client message ID. A
// duplicate waits for the first attempt to commit or abort before observing its
// disposition, so it can never receive a successful ACK for provisional work.
func (p *PendingBuffer) BeginSupplement(sessionID, clientMessageID, payloadHash string) (*supplementLease, supplementDisposition) {
	for {
		p.mu.Lock()
		p.ensureLifecycleLocked(sessionID)
		set := p.accepted[sessionID]
		if set == nil {
			set = &acceptedSupplements{records: make(map[string]*supplementRecord)}
			p.accepted[sessionID] = set
		}
		record := set.records[clientMessageID]
		if record == nil {
			// Reclaim the oldest committed records before rejecting. Provisional
			// records are not in order and are never evicted out from under their
			// owner/waiters, so concurrent first attempts remain strictly bounded.
			for len(set.records) >= maxAcceptedSupplementsPerSession && len(set.order) > 0 {
				oldest := set.order[0]
				set.order = set.order[1:]
				delete(set.records, oldest)
			}
			if len(set.records) >= maxAcceptedSupplementsPerSession {
				p.mu.Unlock()
				return nil, supplementCapacity
			}
			record = &supplementRecord{payloadHash: payloadHash, state: supplementNew}
			record.mu.Lock()
			set.records[clientMessageID] = record
			p.mu.Unlock()
			return &supplementLease{buffer: p, sessionID: sessionID, messageID: clientMessageID, record: record}, supplementNew
		}
		p.mu.Unlock()

		record.mu.Lock()
		if record.state == supplementNew {
			// The prior owner aborted and removed this record while we waited.
			record.mu.Unlock()
			continue
		}
		if record.payloadHash != payloadHash {
			record.mu.Unlock()
			return nil, supplementConflict
		}
		state := record.state
		record.mu.Unlock()
		return nil, state
	}
}

func (l *supplementLease) Commit(state supplementDisposition) {
	l.record.state = state
	p := l.buffer
	p.mu.Lock()
	set := p.accepted[l.sessionID]
	if set != nil && set.records[l.messageID] == l.record {
		set.order = append(set.order, l.messageID)
		for len(set.order) > maxAcceptedSupplementsPerSession {
			oldest := set.order[0]
			set.order = set.order[1:]
			delete(set.records, oldest)
		}
	}
	p.mu.Unlock()
	l.record.mu.Unlock()
}

func (l *supplementLease) Abort() {
	p := l.buffer
	p.mu.Lock()
	if set := p.accepted[l.sessionID]; set != nil && set.records[l.messageID] == l.record {
		delete(set.records, l.messageID)
		if len(set.records) == 0 {
			delete(p.accepted, l.sessionID)
		}
	}
	p.mu.Unlock()
	// supplementNew tells any waiter to retry against the map after it wakes.
	l.record.state = supplementNew
	l.record.mu.Unlock()
}

func pendingCount(items []pendingEntry) int {
	total := 0
	for _, item := range items {
		total += item.count
	}
	return total
}

// Append adds a supplement if the per-session capacity (including a replay
// currently being delivered) has room. Already acknowledged entries are never
// deduplicated or evicted.
func (p *PendingBuffer) Append(sessionID, content string, env *events.Envelope) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLifecycleLocked(sessionID)
	lifecycle := p.lifecycles[sessionID]
	items := p.items[sessionID]
	if pendingCount(items)+lifecycle.inFlight >= maxPendingPerSession {
		return false
	}
	items = append(items, pendingEntry{content: content, envelope: events.Clone(env), receivedAt: time.Now().UnixMilli(), count: 1})
	p.items[sessionID] = items
	return true
}

// DrainAndMerge atomically removes and returns the merged supplement for a
// session. A single entry is returned verbatim; multiple are joined with a
// header and 1-based numbering. repr is the last entry's envelope (template
// for replay). ok is false if nothing was buffered.
func (p *PendingBuffer) DrainAndMerge(sessionID string) (string, *events.Envelope, bool) {
	merged, repr, token, ok := p.DrainForReplay(sessionID)
	if ok {
		p.CompleteReplay(sessionID, token)
	}
	return merged, repr, ok
}

// DrainForReplay also returns a lifecycle token. A failed async replay may be
// requeued only while this token is current; reset/terminate/shutdown invalidates
// it so stale goroutines cannot resurrect cleared content.
func (p *PendingBuffer) DrainForReplay(sessionID string) (string, *events.Envelope, pendingReplayToken, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	token := pendingReplayToken{lifecycle: p.lifecycles[sessionID]}
	items := p.items[sessionID]
	if len(items) == 0 {
		delete(p.items, sessionID)
		return "", nil, token, false
	}
	delete(p.items, sessionID)
	count := pendingCount(items)
	if token.lifecycle != nil {
		token.lifecycle.inFlight += count
	}
	token.count = count
	repr := items[len(items)-1].envelope
	if len(items) == 1 {
		return items[0].content, repr, token, true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "（以下是上一轮执行期间追加的 %d 条消息，请一并处理）\n", len(items))
	for i, it := range items {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(it.content))
	}
	return b.String(), repr, token, true
}

// RequeueIfCurrent puts a failed replay back ahead of supplements that arrived
// later, but only if no lifecycle cleanup invalidated its drain token.
func (p *PendingBuffer) RequeueIfCurrent(sessionID string, token pendingReplayToken, content string, env *events.Envelope) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if token.lifecycle == nil || token.lifecycle != p.lifecycles[sessionID] {
		return false
	}
	token.lifecycle.inFlight -= token.count
	entry := pendingEntry{content: content, envelope: events.Clone(env), receivedAt: time.Now().UnixMilli(), count: token.count}
	items := p.items[sessionID]
	items = append([]pendingEntry{entry}, items...)
	p.items[sessionID] = items
	return true
}

func (p *PendingBuffer) CompleteReplay(sessionID string, token pendingReplayToken) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if token.lifecycle != nil && token.lifecycle == p.lifecycles[sessionID] {
		token.lifecycle.inFlight -= token.count
	}
}

func (p *PendingBuffer) ReplayContext(sessionID string, token pendingReplayToken) (context.Context, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if token.lifecycle == nil || token.lifecycle != p.lifecycles[sessionID] {
		return nil, false
	}
	return token.lifecycle.ctx, true
}

func (p *PendingBuffer) Clear(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lifecycle := p.lifecycles[sessionID]; lifecycle != nil {
		lifecycle.cancel()
	}
	delete(p.items, sessionID)
	delete(p.accepted, sessionID)
	delete(p.lifecycles, sessionID)
}

func (p *PendingBuffer) ClearAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, lifecycle := range p.lifecycles {
		lifecycle.cancel()
	}
	p.items = make(map[string][]pendingEntry)
	p.accepted = make(map[string]*acceptedSupplements)
	p.lifecycles = make(map[string]*pendingLifecycle)
}

// cloneForReplay builds a fresh Input envelope for replay: new id + new
// client_message_id (avoids UNIQUE(session_id, client_message_id) dedup),
// replaced content, reused session/owner/metadata, seq=0 (re-assigned by hub).
// Lives in gateway (not pkg/events) because it needs aep.NewID and pkg/events
// cannot import pkg/aep (cycle).
func cloneForReplay(env *events.Envelope, content string) *events.Envelope {
	c := events.Clone(env) // deep-copies Event.Data map + Metadata
	c.ID = aep.NewID()
	c.Seq = 0
	if data, ok := c.Event.Data.(map[string]any); ok {
		data["content"] = content
		data["client_message_id"] = aep.NewID()
	}
	return c
}
