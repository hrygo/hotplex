// Package gateway implements the WebSocket gateway that speaks AEP v1 to clients.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

const seqBarrierShards = 256

// isReadTimeout reports whether err is a read deadline exceeded error.
func isReadTimeout(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// isDroppable reports whether an event kind can be silently dropped under
// backpressure. All streaming-append content (text delta, reasoning, raw) is
// droppable — losing a few frames only affects streaming cosmetics; the
// authoritative turns table is the source of truth and reconciles on done.
// Non-droppable events (state/done/error/...) use guaranteed delivery.
// Keep in sync with: opencodeserver.singleton.isDroppable, acp.conn.isDroppable.
func isDroppable(kind events.Kind) bool {
	return kind == events.MessageDelta || kind == events.Reasoning || kind == events.Raw
}

// broadcastQueueSize returns the broadcast channel buffer size from config.
// A value of 0 means unbounded (not recommended for production).
func broadcastQueueSize(cfg *config.Config) int {
	if cfg.Gateway.BroadcastQueueSize < 1 {
		return 256 // default
	}
	return cfg.Gateway.BroadcastQueueSize
}

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 32 * 1024
)

// SessionWriter is the minimal interface satisfied by both *Conn and
// platform connection wrappers. It is used as the value type in the
// sessions routing map.
type SessionWriter interface {
	// WriteCtx writes an envelope directly to the connection. Used for
	// control events and init_ack where init-phase buffering must be bypassed.
	WriteCtx(ctx context.Context, env *events.Envelope) error
	// RouteWrite writes an envelope through the Hub routing path. Handles
	// init-phase buffering (for WS conns) and droppable semantics internally.
	RouteWrite(ctx context.Context, env *events.Envelope) error
	// RouteWriteData writes pre-encoded JSON bytes through the Hub routing path.
	// This avoids redundant re-encoding when the same message is sent to N
	// connections. The caller provides the event type for metrics and droppable
	// dispatch. Implementations that cannot consume raw bytes may decode and
	// re-encode internally.
	RouteWriteData(data []byte, eventType events.Kind) error
	Close() error
	// PreferEnvelope returns true if the connection prefers receiving original
	// envelopes (via RouteWrite) over pre-encoded bytes (via RouteWriteData).
	// Platform connections (pcEntry) return true to preserve json:"-" fields;
	// WebSocket connections return false to benefit from pre-encoded bytes.
	PreferEnvelope() bool
}

// platformWriteAsync is implemented by pcEntry. It admits a terminal event
// without blocking on the platform write result; the write loop reports the
// real result on the caller-supplied channel.
type platformWriteAsync interface {
	EnqueueWrite(ctx context.Context, env *events.Envelope, result chan<- error) error
}

// Hub is the central message router and connection registry.
// All WebSocket connections and session→connection mappings are managed here.
type Hub struct {
	log      *slog.Logger
	cfgStore *config.ConfigStore

	upgrader websocket.Upgrader

	mu       sync.RWMutex
	conns    map[*Conn]struct{}                // all active connections
	sessions map[string]map[SessionWriter]bool // sessionID → connections
	// webchatOwners is intentionally separate from sessions. sessions is an
	// outbound subscriber table and can contain Slack/Feishu writers alongside
	// a WebChat connection; it cannot express exclusive WebSocket ownership.
	webchatOwners map[string]*Conn // sessionID → sole initialized WebChat owner
	// everHadConn tracks sessions that have had at least one connection
	// registered (WS or platform). Used to suppress "event dropped" debug
	// log noise for sessions that never had connections (e.g. cron jobs).
	everHadConn map[string]bool

	// Incoming messages from all connections.
	broadcast chan *EnvelopeWithConn

	// connCount tracks the number of active WebSocket connections for the OTel gauge.
	connCount atomic.Int64

	// Sequence generation per session
	seqGen      *SeqGen
	seqBarriers [seqBarrierShards]sync.RWMutex
	// seqOrderMu serializes seq allocation + broadcast enqueue per session.
	// Concurrent producers (forwarder goroutine vs handler goroutine) that
	// allocate a seq and enqueue separately can deliver events to the client
	// out of seq order, violating the AEP strictly-increasing contract
	// (surfaced by contract test C04-double-stop). SendToSession allocates
	// under this lock so allocation order == enqueue order.
	seqOrderMu sync.Map // sessionID → *sync.Mutex
	// seqSessionExists rejects late producers after durable session deletion.
	// It is configured once at startup; nil keeps standalone/test hubs permissive.
	seqSessionExists func(sessionID string) bool
	// seqHydrator reads persisted event seqs to seed SeqGen on reconnect
	// (issue #879). nil when eventstore is disabled — SeqGen then restarts at 1.
	seqHydrator SeqHydrator
	// seqFlusher drains pending collector writes before reading LatestSeq
	// during hydration (issue #894: stale LatestSeq on async capture flush).
	// nil when eventstore is disabled — hydration skips the flush.
	seqFlusher SeqFlusher

	// Shutdown signals.
	ctx    context.Context
	cancel context.CancelFunc

	// LogHandler is an optional callback invoked by routeMessage for each forwarded event.
	// Use it to capture events into an external ring buffer (e.g. /admin/logs).
	// If nil, no events are captured.
	LogHandler func(level, msg, sessionID string)

	// InitThrottle prevents handshake loops.
	InitThrottle *handshakeThrottle

	// auditCollector is propagated to each Conn at creation time (issue #833).
	auditCollector *audit.Collector
}

// EnvelopeWithConn pairs a message with its originating connection.
type EnvelopeWithConn struct {
	Env  *events.Envelope
	Conn *Conn
	// routeCtx is retained only for terminal platform writes, which need to
	// surface their actual delivery result to the original caller.
	routeCtx context.Context
	result   chan<- error
	// afterDrain is called (blocking) by Run after routeMessage finishes processing this item.
	// Tests use it to synchronize against the drain goroutine.
	afterDrain func()
}

func (m *EnvelopeWithConn) complete(err error) {
	if m.result == nil {
		return
	}
	select {
	case m.result <- err:
	default:
	}
}

// asyncTerminal reports whether the caller of SendToSession is waiting on the
// real platform write result of a terminal event. For such messages the result
// is completed by the platform write loop (or by routeMessage when nothing was
// enqueued), never synchronously by the router.
func (m *EnvelopeWithConn) asyncTerminal() bool {
	return m.result != nil && m.Env != nil && isTerminalPlatformEvent(m.Env.Event.Type)
}

// NewHub creates a new Hub.
func NewHub(log *slog.Logger, cfgStore *config.ConfigStore) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if cfgStore == nil {
		panic("gateway: Hub requires ConfigStore")
	}
	cfg := cfgStore.Load()
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		log:           log,
		cfgStore:      cfgStore,
		conns:         make(map[*Conn]struct{}),
		sessions:      make(map[string]map[SessionWriter]bool),
		webchatOwners: make(map[string]*Conn),
		everHadConn:   make(map[string]bool),
		seqGen:        NewSeqGen(),
		broadcast:     make(chan *EnvelopeWithConn, broadcastQueueSize(cfg)),
		ctx:           ctx,
		cancel:        cancel,
		InitThrottle:  newHandshakeThrottle(),
	}

	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  cfg.Gateway.ReadBufferSize,
		WriteBufferSize: cfg.Gateway.WriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			// Non-browser clients (Go SDK, CLI tooling) send no Origin; a
			// missing Origin is allowed and only a present-but-unlisted one
			// is refused. See security.CheckWebSocketOrigin (D05).
			return security.CheckWebSocketOrigin(
				r.Header.Get("Origin"),
				h.cfgStore.Load().Security.AllowedOrigins,
			)
		},
	}
	go h.Run()

	// Register OTel observable gauge for connection count.
	_, _ = observability.Meter().RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(observability.GatewayConnections(), h.connCount.Load())
		h.mu.RLock()
		o.ObserveInt64(observability.GatewayWebChatSessionOwnerConnections(), int64(len(h.webchatOwners)))
		h.mu.RUnlock()
		return nil
	}, observability.GatewayConnections(), observability.GatewayWebChatSessionOwnerConnections())

	return h
}

// SetAuditCollector sets the audit collector propagated to each new Conn.
func (h *Hub) SetAuditCollector(ac *audit.Collector) {
	h.auditCollector = ac
}

// RegisterConn registers a new WebSocket connection.
func (h *Hub) RegisterConn(conn *Conn) {
	h.mu.Lock()
	h.conns[conn] = struct{}{}
	h.mu.Unlock()
	h.connCount.Add(1)
	h.log.Debug("gateway: conn registered", "conn_id", conn.connID, "remote", conn.RemoteAddr(), "session_id", conn.sessionID)
}

// UnregisterConn removes a connection and cleans up session routing mappings.
// Sequence counters outlive connections because collector writes are async and
// a reconnect must continue above values that may not be persisted yet.
func (h *Hub) UnregisterConn(conn *Conn) {
	h.mu.Lock()
	_, registered := h.conns[conn]
	delete(h.conns, conn)
	for sid := range h.sessions {
		h.removeSession(sid, conn)
	}
	// ReadPump releases its owner before calling UnregisterConn. Keep this
	// defensive cleanup for alternate teardown paths so a dead connection can
	// never leak a session owner lease.
	for sid, owner := range h.webchatOwners {
		if owner == conn {
			delete(h.webchatOwners, sid)
		}
	}
	h.mu.Unlock()
	if registered {
		h.connCount.Add(-1)
	}
	h.log.Debug("gateway: conn unregistered", "conn_id", conn.connID, "remote", conn.RemoteAddr(), "session_id", conn.sessionID)
}

// TryAcquireWebChatOwner atomically registers conn as the sole initialized
// WebChat owner for sessionID. It intentionally does not alter outbound
// subscriptions: caller joins the route only after successfully acquiring.
func (h *Hub) TryAcquireWebChatOwner(sessionID string, conn *Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, exists := h.webchatOwners[sessionID]; exists {
		if !existing.Closed() {
			return false
		}
		h.log.Info("gateway: replacing closed WebChat owner connection",
			"session_id", sessionID,
			"old_conn_id", existing.connID,
			"new_conn_id", conn.connID,
		)
	}
	h.webchatOwners[sessionID] = conn
	return true
}

// IsWebChatOwner reports whether conn still owns sessionID's WebChat ingress.
func (h *Hub) IsWebChatOwner(sessionID string, conn *Conn) bool {
	h.mu.RLock()
	owner := h.webchatOwners[sessionID]
	h.mu.RUnlock()
	return owner == conn
}

// webChatOwnerConnID returns the current owner identifier for structured
// diagnostics. The value is opaque and never derived from credentials.
func (h *Hub) webChatOwnerConnID(sessionID string) string {
	h.mu.RLock()
	owner := h.webchatOwners[sessionID]
	h.mu.RUnlock()
	if owner == nil {
		return ""
	}
	return owner.connID
}

// ReleaseWebChatOwner removes conn only when it is the current owner. Its
// return value fences lifecycle cleanup for stale, candidate, and rejected
// connections that must never transition the session to idle.
func (h *Hub) ReleaseWebChatOwner(sessionID string, conn *Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.webchatOwners[sessionID] != conn {
		return false
	}
	delete(h.webchatOwners, sessionID)
	return true
}

// JoinSession subscribes conn to receive outbound events for a session. WebChat
// ownership is enforced independently by TryAcquireWebChatOwner, so platform
// subscribers are retained and this method never removes another writer.
func (h *Hub) JoinSession(sessionID string, conn *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = make(map[SessionWriter]bool)
	}
	h.sessions[sessionID][conn] = true
	h.everHadConn[sessionID] = true
}

// LeaveSession unsubscribes conn from a session. The session sequence counter
// is retained for the Hub lifetime; a connection is not a session lifecycle.
func (h *Hub) LeaveSession(sessionID string, conn *Conn) {
	h.mu.Lock()
	h.removeSession(sessionID, conn)
	h.mu.Unlock()
}

// removeSession removes conn from sessionID and cleans up empty sessions.
// Caller must hold h.mu.
func (h *Hub) removeSession(sessionID string, conn SessionWriter) {
	if conns, ok := h.sessions[sessionID]; ok {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(h.sessions, sessionID)
			delete(h.everHadConn, sessionID)
		}
	}
}

// snapshotConns returns a snapshot of all SessionWriters subscribed to a session.
// The snapshot is taken under RLock and is safe to iterate without holding the lock.
func (h *Hub) snapshotConns(sessionID string) []SessionWriter {
	h.mu.RLock()
	sessionConns := h.sessions[sessionID]
	conns := make([]SessionWriter, 0, len(sessionConns))
	for conn := range sessionConns {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()
	return conns
}

// JoinPlatformSession subscribes a PlatformConn to receive events for a session.
// Unlike JoinSession, it does not register the connection in h.conns (no WS tracking)
// and does not remove stale connections (platform SDK handles its own lifecycle).
// Deduplicates: if the same PlatformConn is already subscribed, this is a no-op.
func (h *Hub) JoinPlatformSession(sessionID string, pc messaging.PlatformConn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = make(map[SessionWriter]bool)
	}

	for sw := range h.sessions[sessionID] {
		if pce, ok := sw.(*pcEntry); ok && pce.pc == pc {
			select {
			case <-pce.done:
				delete(h.sessions[sessionID], sw)
				h.log.Info("gateway: replaced dead platform conn entry",
					"session_id", sessionID)
			default:
				return
			}
		}
	}

	h.sessions[sessionID][newPCEntry(h.ctx, pc, defaultPCEntryConfig(h.cfgStore.Load()), h.log)] = true
	h.everHadConn[sessionID] = true
}

// sendBroadcast sends to the broadcast channel. Returns false if the hub is
// shutting down (ctx cancelled). Uses select with ctx.Done() instead of
// close(channel)+recover() to avoid the send-on-closed-channel data race.
//
// Used for guaranteed-delivery events (state/done/error/...). Never use this
// for droppable events — a full broadcast channel would block Hub.Run and
// cascade backpressure to unrelated sessions. Use trySendBroadcast instead.
func (h *Hub) sendBroadcast(msg *EnvelopeWithConn) (sent bool) {
	select {
	case h.broadcast <- msg:
		return true
	case <-h.ctx.Done():
		return false
	}
}

// trySendBroadcast sends to the broadcast channel without blocking. Returns
// false immediately if the channel is full. Used for droppable events
// (message.delta/reasoning/raw) so a slow session cannot stall Hub.Run and
// cause cross-session delta loss. The dropped delta is cosmetic — the UI
// reconciles against the authoritative turns table on done.
func (h *Hub) trySendBroadcast(msg *EnvelopeWithConn) (sent bool) {
	select {
	case h.broadcast <- msg:
		return true
	default:
		return false
	}
}

// HasActiveConn reports whether the session has at least one active connection.
func (h *Hub) HasActiveConn(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions[sessionID]) > 0
}

// SendToSession delivers a message to all connections subscribed to a session.
// PriorityControl is a scheduling hint, but sequence-bearing events still use
// the ordered broadcast writer so allocation order matches client delivery
// order. afterDrain functions are called sequentially after the item is routed
// by Run.
func (h *Hub) SendToSession(ctx context.Context, env *events.Envelope, afterDrain ...func()) error {
	spanCtx, span := observability.Tracer().Start(ctx, "hub.send_to_session")
	defer span.End()
	span.SetAttributes(
		attribute.String(observability.KeySessionID, env.SessionID),
		attribute.String(observability.KeyEventType, string(env.Event.Type)),
		attribute.String(observability.KeyPriority, string(env.Priority)),
	)

	// Inject trace_id + span_id into AEP metadata so a downstream consumer can
	// correlate an event to the precise span that produced it.
	// Copy-on-write: if Metadata is shared, create a new map to avoid data races
	// when the same envelope is processed by multiple goroutines.
	if sc := trace.SpanContextFromContext(spanCtx); sc.IsValid() {
		traceID := sc.TraceID().String()
		spanID := sc.SpanID().String()
		if env.Metadata == nil {
			env.Metadata = map[string]any{
				observability.KeyTraceID: traceID,
				observability.KeySpanID:  spanID,
			}
		} else {
			copied := make(map[string]any, len(env.Metadata)+2)
			maps.Copy(copied, env.Metadata)
			copied[observability.KeyTraceID] = traceID
			copied[observability.KeySpanID] = spanID
			env.Metadata = copied
		}
	}

	// Assign sequence number before sending to broadcast queue or clients.
	// We skip assignment if seq is already set (eg. by Handler for direct replies).
	// When allocating, hold the per-session publish-order lock through enqueue
	// (and any terminal ack wait) so allocation order matches the order clients
	// observe. Without it, a forwarder goroutine that allocates early but sends
	// late interleaves with a handler-allocated event, delivering out-of-order
	// seqs (contract test C04-double-stop).
	if env.Seq == 0 {
		mu := h.SeqOrderLock(env.SessionID)
		mu.Lock()
		defer mu.Unlock()
		env.Seq = h.NextSeq(env.SessionID)
		if env.Seq == 0 {
			return fmt.Errorf("gateway: session released: %s", env.SessionID)
		}
	}
	// afterDrainCallback is called by Run after the item is routed; nil if not supplied.
	var afterDrainCallback func()
	if len(afterDrain) > 0 {
		afterDrainCallback = afterDrain[0]
	}

	// No Clone needed here: Bridge.forwardEvents already clones the envelope
	// before calling SendToSession, so this is a bridge-owned copy. The Hub.Run
	// goroutine reads it from the channel for routing without mutation.
	msg := &EnvelopeWithConn{Env: env, afterDrain: afterDrainCallback}
	if isTerminalPlatformEvent(env.Event.Type) {
		result := make(chan error, 1)
		msg.routeCtx = ctx
		msg.result = result
		if !h.sendBroadcast(msg) {
			return errors.New("gateway: broadcast channel closed")
		}
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-h.ctx.Done():
			return errors.New("gateway: broadcast channel closed")
		}
	}

	// Control events are guaranteed-delivery messages, not an out-of-band
	// write path. Sending them through the same queue prevents a higher-seq
	// InputAck/State from overtaking an earlier worker event already admitted to
	// Hub.Run (C04-double-stop).
	if env.Priority == events.PriorityControl {
		if h.sendBroadcast(msg) {
			return nil
		}
		return errors.New("gateway: broadcast channel closed")
	}

	if isDroppable(env.Event.Type) {
		// Non-blocking send — droppable events must never stall Hub.Run.
		// A full broadcast channel means a slow consumer is already being
		// protected at the per-conn writeCh layer; dropping here is the
		// intended backpressure semantics. The UI self-heals on done.
		if h.trySendBroadcast(msg) {
			return nil
		}
		observability.GatewayDeltasDropped().Add(context.Background(), 1)
		return nil
	}

	// Guaranteed delivery path.
	if h.sendBroadcast(msg) {
		return nil
	}
	return errors.New("gateway: broadcast channel closed")
}

func (h *Hub) sendControlToSession(ctx context.Context, env *events.Envelope) {
	conns := h.snapshotConns(env.SessionID)

	if len(conns) == 0 {
		observability.GatewayNoSubscribersDropped().Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", string(env.Event.Type))))
		h.log.Debug("gateway: control event dropped, no connections",
			"session_id", env.SessionID, "event_type", env.Event.Type)
		return
	}

	env = events.Clone(env)
	for _, conn := range conns {
		observability.GatewayMessages().Add(ctx, 1, metric.WithAttributes(attribute.String("direction", "outgoing"), attribute.String("event_type", string(env.Event.Type))))
		if err := conn.WriteCtx(ctx, env); err != nil {
			h.log.Warn("gateway: send to conn failed", "session_id", env.SessionID, "err", err)
		}
	}
}

// HandleHTTP serves WebSocket upgrade requests at the gateway endpoint.
// It authenticates the request, upgrades to WebSocket, and starts read/write pumps.
func (h *Hub) HandleHTTP(
	auth *security.Authenticator,
	handler *Handler,
	bridge *Bridge,
	cookieAuth *security.CookieAuth,
) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to authenticate at HTTP upgrade time.
		// If no API key is provided, defer auth to the init envelope (browser WS clients).
		var userID, botID string
		var pendingAuth bool

		key, found := auth.ExtractAPIKey(r)
		if found {
			uid, ok := auth.AuthenticateKey(r.Context(), key)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			userID = uid
			botID = security.BotIDFromRequest(r)
		} else if cookieAuth != nil {
			// No API key — try cookie auth before deferring to init envelope.
			// AuthenticateActiveCookie rejects disabled users (shared with the
			// REST API); a disabled/invalid cookie falls back to init envelope.
			if uid, ok := auth.AuthenticateActiveCookie(r); ok {
				userID = uid
				botID = security.BotIDFromRequest(r)
			} else {
				pendingAuth = true
			}
		} else {
			// No key at HTTP level — defer to init envelope auth (browser WS clients).
			pendingAuth = true
		}

		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			sessionID = aep.NewSessionID()
		}

		wc, err := h.upgrader.Upgrade(w, r, nil)
		if err != nil {
			h.log.Warn("gateway: upgrade failed", "err", err)
			return
		}

		c := newConn(h, wc, sessionID, bridge)
		c.SetAuditCollector(h.auditCollector)
		c.pendingAuth = pendingAuth
		if !pendingAuth {
			c.userID = userID
			c.botID = botID
		}
		h.RegisterConn(c)

		// Start read pump in background. WritePump is started by newConn.
		go c.ReadPump(handler, handler.sm, auth)

		idLog := userID
		if pendingAuth {
			idLog = "(pending)"
		}
		h.log.Info("gateway: WS connected", "session_id", sessionID, "user_id", idLog, "bot_id", c.botID)
	})
}

// Run starts the hub's run loop. It blocks until the context is cancelled.
// The broadcast channel is never closed — sendBroadcast uses ctx.Done() to
// detect shutdown, and this function drains remaining messages non-blockingly.
func (h *Hub) Run() {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("hub: panic in Run", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	// Start periodic cleanup for throttler
	throttleCleanup := time.NewTicker(10 * time.Minute)
	defer throttleCleanup.Stop()

	for {
		select {
		case <-h.ctx.Done():
			h.drainBroadcast()
			return
		case <-throttleCleanup.C:
			h.InitThrottle.Cleanup()
		case msg := <-h.broadcast:
			if msg == nil || msg.Env == nil {
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						h.log.Error("hub: panic in routeMessage", "session_id", msg.Env.SessionID, "panic", r, "stack", string(debug.Stack()))
					}
				}()
				_, span := observability.Tracer().Start(h.ctx, "hub.broadcast")
				span.SetAttributes(
					attribute.String("session_id", msg.Env.SessionID),
					attribute.String("event_type", string(msg.Env.Event.Type)),
					attribute.String("seq", fmt.Sprintf("%d", msg.Env.Seq)),
				)
				err := h.routeMessage(msg)
				span.End()
				if !msg.asyncTerminal() {
					msg.complete(err)
				}
				if msg.afterDrain != nil {
					msg.afterDrain()
				}
			}()
		}
	}
}

func (h *Hub) routeMessage(msg *EnvelopeWithConn) error {
	conns := h.snapshotConns(msg.Env.SessionID)
	terminalAsync := msg.asyncTerminal()
	var terminalWrites []terminalWriteResult
	var routeErrs []error
	// Terminal callers wait on a result channel. When no per-connection
	// aggregator was started, complete them on every exit path (no
	// subscribers, encode failure, or synchronous write errors); otherwise
	// the aggregator owns completion.
	defer func() {
		if terminalAsync && len(terminalWrites) == 0 {
			msg.complete(errors.Join(routeErrs...))
		}
	}()

	if len(conns) == 0 {
		observability.GatewayNoSubscribersDropped().Add(h.ctx, 1, metric.WithAttributes(attribute.String("event_type", string(msg.Env.Event.Type))))
		// Suppress debug log for sessions that never had any connection
		// registered (e.g. cron/internal sessions). These events are expected
		// to have no subscribers — logging every one is just noise.
		h.mu.RLock()
		hadConn := h.everHadConn[msg.Env.SessionID]
		h.mu.RUnlock()
		if hadConn {
			h.log.Debug("gateway: event dropped, no connections",
				"session_id", msg.Env.SessionID, "event_type", msg.Env.Event.Type)
		}
		return nil
	}

	if h.LogHandler != nil {
		level := "INFO"
		switch msg.Env.Event.Type {
		case events.Error:
			level = "ERROR"
		case events.State:
			level = "WARN"
		}
		h.LogHandler(level, fmt.Sprintf("event %s seq=%d", msg.Env.Event.Type, msg.Env.Seq), msg.Env.SessionID)
	}

	// Pre-encode once and distribute raw bytes to all connections.
	// This avoids N redundant JSON marshal operations (one per conn).
	data, err := aep.EncodeJSON(msg.Env)
	if err != nil {
		h.log.Error("gateway: encode message failed", "session_id", msg.Env.SessionID, "err", err)
		return err
	}

	for _, conn := range conns {
		var err error
		if conn.PreferEnvelope() {
			// Platform connections need the original envelope to preserve
			// json:"-" fields (e.g. OwnerID) that EncodeJSON omits.
			// Use WithoutCancel so cancelled h.ctx doesn't block during
			// shutdown drain, while preserving tracing propagation.
			routeCtx := context.WithoutCancel(h.ctx)
			if msg.routeCtx != nil {
				routeCtx = msg.routeCtx
			}
			if async, ok := conn.(platformWriteAsync); ok && terminalAsync {
				// Terminal writes must never block the single router: admit
				// only; a per-connection result channel is completed by the
				// write loop (or its budget guard) and aggregated off-router.
				perConn := make(chan error, 1)
				if err = async.EnqueueWrite(routeCtx, msg.Env, perConn); err == nil {
					terminalWrites = append(terminalWrites, terminalWriteResult{conn: conn, ch: perConn})
				}
			} else {
				err = conn.RouteWrite(routeCtx, msg.Env)
			}
		} else {
			err = conn.RouteWriteData(data, msg.Env.Event.Type)
		}
		if err == nil {
			continue
		}
		if isContentPresentedErr(err) {
			// Body delivered, decoration-only failure: healthy turn, keep the
			// writer registered — detaching would drop the next turn's events.
			h.log.Warn("gateway: terminal write decoration failed, body already presented",
				"session_id", msg.Env.SessionID, "err", err)
			continue
		}
		routeErrs = append(routeErrs, err)
		h.log.Warn("gateway: write failed", "session_id", msg.Env.SessionID, "err", err)
		h.detachAndCloseSessionWriter(msg.Env.SessionID, conn)
	}
	if len(terminalWrites) > 0 {
		go h.aggregateTerminalWrites(msg, terminalWrites, errors.Join(routeErrs...))
	}
	return errors.Join(routeErrs...)
}

type terminalWriteResult struct {
	conn SessionWriter
	ch   chan error
}

// contentPresentedError is implemented by platform conns whose terminal write
// errors can prove the message BODY already reached the user even though the
// write reported a failure (e.g. the Slack streaming close/decoration step
// failed after the reply text was delivered). Such failures are not delivery
// failures: the session writer must stay registered for the next turn, and the
// terminal caller must not see a spurious error.
type contentPresentedError interface {
	error
	BodyPresented() bool
}

// isContentPresentedErr reports whether err proves the message body was
// already presented to the user despite the write error.
func isContentPresentedErr(err error) bool {
	var cpe contentPresentedError
	return errors.As(err, &cpe) && cpe.BodyPresented()
}

// aggregateTerminalWrites collects the per-connection terminal write results
// off the router goroutine, detaches failed writers, and completes the caller
// with the joined error. Each result channel is guaranteed a value within the
// terminal budget (write loop or its guard), so this goroutine cannot leak.
func (h *Hub) aggregateTerminalWrites(msg *EnvelopeWithConn, writes []terminalWriteResult, baseErr error) {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	if baseErr != nil {
		errs = append(errs, baseErr)
	}
	for _, tw := range writes {
		wg.Add(1)
		go func(tw terminalWriteResult) {
			defer wg.Done()
			if err := <-tw.ch; err != nil {
				if isContentPresentedErr(err) {
					// Body delivered, decoration-only failure: do not count as a
					// failed terminal delivery and keep the writer registered.
					h.log.Warn("gateway: terminal platform write decoration failed, body already presented",
						"session_id", msg.Env.SessionID, "err", err)
					return
				}
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				h.log.Warn("gateway: terminal platform write failed",
					"session_id", msg.Env.SessionID, "err", err)
				h.detachAndCloseSessionWriter(msg.Env.SessionID, tw.conn)
			}
		}(tw)
	}
	wg.Wait()
	msg.complete(errors.Join(errs...))
}

// detachAndCloseSessionWriter removes a failed writer from routing before
// starting cleanup. Close may wait on a platform write that outlives its
// caller's terminal deadline, so it must never block the Hub's single router.
func (h *Hub) detachAndCloseSessionWriter(sessionID string, conn SessionWriter) {
	h.mu.Lock()
	h.removeSession(sessionID, conn)
	h.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.log.Error("gateway: panic closing failed session writer",
					"session_id", sessionID, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if err := conn.Close(); err != nil {
			h.log.Warn("gateway: failed session writer cleanup",
				"session_id", sessionID, "err", err)
		}
	}()
}

// drainBroadcast processes remaining messages in the broadcast channel.
// Non-blocking: returns when the channel is empty. Since sendBroadcast checks
// ctx.Done() before sending, no new messages arrive after context cancellation.
func (h *Hub) drainBroadcast() {
	for {
		select {
		case msg := <-h.broadcast:
			if msg != nil && msg.Env != nil {
				err := h.routeMessage(msg)
				if !msg.asyncTerminal() {
					msg.complete(err)
				}
				if msg.afterDrain != nil {
					msg.afterDrain()
				}
			}
		default:
			return
		}
	}
}

// NextSeq returns the next sequence number for a session from the central generator.
func (h *Hub) NextSeq(sessionID string) int64 {
	release, ok := h.BeginSeqOperation(sessionID)
	if !ok {
		return 0
	}
	defer release()
	return h.NextSeqHeld(sessionID)
}

// NextSeqHeld allocates while the caller already holds a sequence operation
// lease. It avoids recursively acquiring an RWMutex when a release is queued.
func (h *Hub) NextSeqHeld(sessionID string) int64 {
	return h.seqGen.Next(sessionID)
}

// NextSeqBeforeRelease is reserved for the synchronous deleted-state notice:
// the durable session has already been removed, but runtime release has not yet
// started. It participates in the barrier without consulting session existence.
func (h *Hub) NextSeqBeforeRelease(sessionID string) int64 {
	barrier := h.seqBarrier(sessionID)
	barrier.RLock()
	defer barrier.RUnlock()
	return h.seqGen.Next(sessionID)
}

// BeginSeqOperation pins a session's sequence generation until the returned
// release function is called. Durable producers acquire this before NextSeq
// and hold it through collector capture, so physical deletion cannot reset the
// counter while an old-generation event is still in flight.
func (h *Hub) BeginSeqOperation(sessionID string) (func(), bool) {
	barrier := h.seqBarrier(sessionID)
	barrier.RLock()
	if h.seqSessionExists != nil && !h.seqSessionExists(sessionID) {
		barrier.RUnlock()
		return func() {}, false
	}
	return barrier.RUnlock, true
}

// NextSeqPeek returns the current sequence number for a session without incrementing.
func (h *Hub) NextSeqPeek(sessionID string) int64 {
	return h.seqGen.Peek(sessionID)
}

// SeqOrderLock returns the per-session publish-order mutex. Producers that
// allocate a seq themselves (durable forwarding, which must pin the seq for
// eventstore capture) hold this lock across allocation → capture → enqueue so
// their seq order matches the enqueue order observed by clients.
func (h *Hub) SeqOrderLock(sessionID string) *sync.Mutex {
	v, _ := h.seqOrderMu.LoadOrStore(sessionID, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex) // 唯一写入者是本方法,断言必然成功
	return mu
}

// ForgetSeq releases sequence state after the session has been physically
// deleted and pending collector writes have been flushed.
func (h *Hub) ForgetSeq(sessionID string) {
	_ = h.ReleaseSeq(sessionID, nil)
	h.seqOrderMu.Delete(sessionID)
}

// ReleaseSeq waits for all durable sequence producers, runs drain, and only
// then removes the per-session counter. A drain failure retains the counter so
// a reconnect cannot reuse sequence numbers that may still be pending.
func (h *Hub) ReleaseSeq(sessionID string, drain func() error) error {
	barrier := h.seqBarrier(sessionID)
	barrier.Lock()
	defer barrier.Unlock()

	if drain != nil {
		if err := drain(); err != nil {
			return err
		}
	}
	h.seqGen.Remove(sessionID)
	return nil
}

func (h *Hub) seqBarrier(sessionID string) *sync.RWMutex {
	// FNV-1a without allocating on the sequence hot path.
	hash := uint32(2166136261)
	for i := 0; i < len(sessionID); i++ {
		hash ^= uint32(sessionID[i])
		hash *= 16777619
	}
	return &h.seqBarriers[hash%seqBarrierShards]
}

// SetSeqHydrator injects the persisted-seq reader used to hydrate SeqGen on
// reconnect (issue #879). Optional; when nil (eventstore disabled), SeqGen
// restarts from 1 as before. Called once during gateway startup after the
// eventstore is constructed.
func (h *Hub) SetSeqHydrator(sh SeqHydrator) {
	h.seqHydrator = sh
}

// SetSeqSessionExists injects the durable session existence check used to
// reject producers that arrive after physical deletion. Called once at startup.
func (h *Hub) SetSeqSessionExists(check func(sessionID string) bool) {
	h.seqSessionExists = check
}

// SetSeqFlusher injects the collector flush used to drain pending writes
// before reading LatestSeq during hydration. Called once at gateway startup
// after the collector is created; nil when eventstore is disabled.
func (h *Hub) SetSeqFlusher(sf SeqFlusher) {
	h.seqFlusher = sf
}

// EnsureSeqHydrated seeds the SeqGen counter for sessionID from persisted
// events on first use. A database error is fatal to the handshake: falling
// back to 1 could collide with durable history and make the collector reject
// the entire batch. MUST run before the first NextSeq for a session.
//
// Uses a write-lock fence (instead of BeginSeqOperation's RLock) to act as a
// full barrier: all concurrent seq producers and ReleaseSeq must complete
// before LatestSeq is read. An optional SeqFlusher drains the collector's
// async captureC first, preventing stale-LatestSeq races where an allocated
// seq is in-flight but not yet visible in the DB (issue #894).
func (h *Hub) EnsureSeqHydrated(sessionID string) error {
	barrier := h.seqBarrier(sessionID)
	barrier.Lock()
	defer barrier.Unlock()

	if h.seqSessionExists != nil && !h.seqSessionExists(sessionID) {
		return fmt.Errorf("gateway: session released: %s", sessionID)
	}

	if h.seqHydrator == nil {
		return nil
	}
	// Use the hydrated flag (not Initialized) so a racing producer that called
	// Next before us — implicitly creating the counter — cannot short-circuit
	// hydration. Without this, a session resumed after WS bounce skips the DB
	// read, restarts the counter near 0, and collides with persisted history
	// (recurring UNIQUE constraint failures; issue #879 regression surviving #900).
	if h.seqGen.IsHydrated(sessionID) {
		return nil
	}

	// priorPeek reveals a racing producer: >0 means Next already allocated seqs
	// before hydration ran. Init below raises the floor past them.
	priorPeek := h.seqGen.Peek(sessionID)

	// Flush the collector's async captureC before reading LatestSeq so that
	// events whose seq was already allocated but not yet committed to the DB
	// are visible before we seed the new counter.
	if h.seqFlusher != nil {
		if err := h.seqFlusher.FlushSession(sessionID); err != nil {
			return fmt.Errorf("gateway: flush before hydrate seq for session %s: %w", sessionID, err)
		}
	}

	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	latest, err := h.seqHydrator.LatestSeq(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("gateway: hydrate seq for session %s: %w", sessionID, err)
	}
	h.seqGen.Init(sessionID, latest)
	h.seqGen.MarkHydrated(sessionID)
	h.log.Info("gateway: seq hydrated from eventstore",
		"session_id", sessionID,
		"latest_seq", latest,
		"prior_peek", priorPeek,
	)
	return nil
}

// ConnectionsOpen returns the number of currently open WebSocket connections.
func (h *Hub) ConnectionsOpen() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// Shutdown gracefully shuts down all connections and stops the hub.
// It signals Run() to stop via context cancellation, waits for in-flight
// broadcast messages to drain, then closes all WebSocket connections.
// The ctx deadline controls the maximum wait time.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.cancel()

	// Wait briefly for Run() to drain remaining messages.
	// Run() handles drain in its ctx.Done() path. The broadcast channel
	// is never closed — it's GC'd with the Hub.
	drainDone := make(chan struct{})
	go func() {
		// Give Run() a moment to process its ctx.Done path.
		// This also handles the case where Run() was never started.
		h.drainBroadcast()
		close(drainDone)
	}()
	select {
	case <-drainDone:
	case <-ctx.Done():
		h.log.Warn("gateway: broadcast drain timed out")
	}

	// Close all connections.
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	// Collect platform connections from sessions map. These are not in h.conns
	// and must be closed here since Hub.Shutdown is the canonical shutdown point.
	seenPC := make(map[*pcEntry]bool)
	var pcConns []*pcEntry
	for _, conns := range h.sessions {
		for sw := range conns {
			if pce, ok := sw.(*pcEntry); ok && !seenPC[pce] {
				seenPC[pce] = true
				pcConns = append(pcConns, pce)
			}
		}
	}
	h.mu.RUnlock()

	var errs []error
	for _, c := range conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, pce := range pcConns {
		if err := pce.Close(); err != nil {
			errs = append(errs, fmt.Errorf("platform conn close: %w", err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
