package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// ErrNotConnected is returned when sending before connection.
var ErrNotConnected = errors.New("client: not connected")

// Protocol constants (matching gateway defaults).
const (
	DefaultPingInterval = 54 * time.Second
	SendChannelCap      = 100
	InitTimeout         = 30 * time.Second
)

// Client is the HotPlex Worker Gateway client.
// It implements the AEP v1 WebSocket protocol.
type Client struct {
	// config from options
	url             string
	workerType      string
	botID           string
	apiKey          string
	clientSessionID string
	title           string

	// heartbeat config
	pingInterval time.Duration

	// reconnection config
	autoReconnect bool
	metadata      map[string]any

	// runtime state
	mu        sync.Mutex
	conn      *websocket.Conn
	sessionID string
	state     SessionState
	seq       int64
	closed    bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sendCh    chan []byte
	listeners []*subscriber

	logger *slog.Logger
}

// subscriber pairs an event channel with a done signal. deliver() sends
// outside c.mu, so a subscriber that is removed concurrently would otherwise
// race a channel close — a send on a closed channel panics the process, and
// Unsubscribe is reachable from ordinary use (SendInputAsync defers it).
type subscriber struct {
	ch   chan Event
	done chan struct{}
}

// Event is an inbound event delivered via the Events() channel.
type Event struct {
	Type    string `json:"type"`
	Seq     int64  `json:"seq"`
	Session string `json:"session"`
	Data    any    `json:"data,omitempty"`
}

// AsInputAckData parses a durable input acknowledgement.
func (e Event) AsInputAckData() (InputAckData, bool) {
	return events.DecodeAs[InputAckData](e.Data)
}

// AsDoneData parses event data as DoneData.
func (e Event) AsDoneData() (DoneData, bool) { return events.DecodeAs[DoneData](e.Data) }

// AsErrorData parses event data as ErrorData.
func (e Event) AsErrorData() (ErrorData, bool) { return events.DecodeAs[ErrorData](e.Data) }

// AsToolCallData parses event data as ToolCallData.
func (e Event) AsToolCallData() (ToolCallData, bool) { return events.DecodeAs[ToolCallData](e.Data) }

// AsPermissionRequestData parses event data as PermissionRequestData.
func (e Event) AsPermissionRequestData() (PermissionRequestData, bool) {
	return events.DecodeAs[PermissionRequestData](e.Data)
}

// AsQuestionRequestData parses event data as QuestionRequestData.
func (e Event) AsQuestionRequestData() (QuestionRequestData, bool) {
	return events.DecodeAs[QuestionRequestData](e.Data)
}

// AsElicitationRequestData parses event data as ElicitationRequestData.
func (e Event) AsElicitationRequestData() (ElicitationRequestData, bool) {
	return events.DecodeAs[ElicitationRequestData](e.Data)
}

// AsMessageStartData parses event data as MessageStartData.
func (e Event) AsMessageStartData() (MessageStartData, bool) {
	return events.DecodeAs[MessageStartData](e.Data)
}

// AsMessageDeltaData parses event data as MessageDeltaData.
func (e Event) AsMessageDeltaData() (MessageDeltaData, bool) {
	return events.DecodeAs[MessageDeltaData](e.Data)
}

// AsMessageEndData parses event data as MessageEndData.
func (e Event) AsMessageEndData() (MessageEndData, bool) {
	return events.DecodeAs[MessageEndData](e.Data)
}

// AsStateData parses event data as StateData.
func (e Event) AsStateData() (StateData, bool) { return events.DecodeAs[StateData](e.Data) }

// AsReasoningData parses event data as ReasoningData.
func (e Event) AsReasoningData() (ReasoningData, bool) { return events.DecodeAs[ReasoningData](e.Data) }

// AsStepData parses event data as StepData.
func (e Event) AsStepData() (StepData, bool) { return events.DecodeAs[StepData](e.Data) }

// AsContextUsageData parses event data as ContextUsageData.
func (e Event) AsContextUsageData() (ContextUsageData, bool) {
	return events.DecodeAs[ContextUsageData](e.Data)
}

// AsSkillsListData parses event data as SkillsListData.
func (e Event) AsSkillsListData() (SkillsListData, bool) {
	return events.DecodeAs[SkillsListData](e.Data)
}

// AsMCPStatusData parses event data as MCPStatusData.
func (e Event) AsMCPStatusData() (MCPStatusData, bool) { return events.DecodeAs[MCPStatusData](e.Data) }

// AsWorkerCommandData parses event data as WorkerCommandData.
func (e Event) AsWorkerCommandData() (WorkerCommandData, bool) {
	return events.DecodeAs[WorkerCommandData](e.Data)
}

// AsToolUpdateData parses event data as ToolUpdateData.
func (e Event) AsToolUpdateData() (ToolUpdateData, bool) {
	return events.DecodeAs[ToolUpdateData](e.Data)
}

// AsPlanData parses event data as PlanData.
func (e Event) AsPlanData() (PlanData, bool) { return events.DecodeAs[PlanData](e.Data) }

// AsModeUpdateData parses event data as ModeUpdateData.
func (e Event) AsModeUpdateData() (ModeUpdateData, bool) {
	return events.DecodeAs[ModeUpdateData](e.Data)
}

// AsToolResultData parses event data as ToolResultData.
func (e Event) AsToolResultData() (ToolResultData, bool) {
	return events.DecodeAs[ToolResultData](e.Data)
}

// AsInitAckData parses event data as InitAckData.
func (e Event) AsInitAckData() (InitAckData, bool) { return events.DecodeAs[InitAckData](e.Data) }

// New creates a new client with the given options.
func New(ctx context.Context, opts ...Option) (*Client, error) {
	c := &Client{
		pingInterval: DefaultPingInterval,
		sendCh:       make(chan []byte, SendChannelCap),
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.url == "" {
		return nil, errors.New("client: URL is required")
	}
	if c.workerType == "" {
		return nil, errors.New("client: workerType is required")
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	return c, nil
}

// Connect establishes a new session with the gateway.
func (c *Client) Connect(ctx context.Context) (*InitAckData, error) {
	sessionID := c.clientSessionID
	if sessionID == "" {
		sessionID = aep.NewSessionID()
	}

	if !c.autoReconnect {
		return c.doConnect(ctx, sessionID, false)
	}

	// Reconnection loop
	var (
		ack     *InitAckData
		err     error
		attempt int
	)
	for {
		ack, err = c.doConnect(ctx, sessionID, attempt > 0)
		if err == nil {
			return ack, nil
		}

		attempt++
		c.logger.Warn("client: connect failed, retrying", "attempt", attempt, "err", err)

		backoff := backoffDuration(attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, errors.New("client: closed")
		case <-time.After(backoff):
			// retry
		}
	}
}

// Resume attaches to an existing session.
func (c *Client) Resume(ctx context.Context, sessionID string) (*InitAckData, error) {
	return c.doConnect(ctx, sessionID, true)
}

func (c *Client) doConnect(ctx context.Context, sessionID string, isResume bool) (*InitAckData, error) {
	hdr := http.Header{}
	if c.botID != "" {
		hdr.Set("X-Bot-ID", c.botID)
	}
	if c.apiKey != "" {
		hdr.Set("X-API-Key", c.apiKey)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.url, hdr)
	if err != nil {
		return nil, fmt.Errorf("client: dial: %w", err)
	}
	c.conn = conn
	c.sessionID = sessionID

	// Build and send init envelope.
	initData := map[string]any{
		"version":     events.Version,
		"worker_type": c.workerType,
		"client_caps": map[string]any{
			"supports_delta":     true,
			"supports_tool_call": true,
			"supported_kinds": []string{
				"error", "state", "done", "message", "message.start",
				"message.delta", "message.end", "tool_call", "tool_result",
				"reasoning", "step", "raw", "permission_request",
				"control", "ping", "pong", "question_request",
				"question_response", "elicitation_request", "elicitation_response",
			},
		},
	}
	if c.metadata != nil {
		initData["config"] = map[string]any{"metadata": c.metadata}
	}
	if c.botID != "" {
		initData["auth"] = map[string]any{"bot_id": c.botID}
	}
	if c.clientSessionID != "" || isResume {
		initData["session_id"] = sessionID
	}
	if c.title != "" {
		initData["title"] = c.title
	}

	env := events.NewEnvelope(aep.NewID(), sessionID, 1, events.Init, initData)
	env.Priority = PriorityControl
	frame, err := aep.EncodeJSON(env)
	if err != nil {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		return nil, fmt.Errorf("client: send init: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("client: send init: %w", err)
	}

	// Read init_ack. Use raw JSON decode to avoid strict Validate()
	// (init_ack from gateway may not satisfy all envelope requirements).
	//
	// The gateway may send other events (e.g. state) before init_ack arrives
	// due to a race between the ReadPump goroutine (sending init_ack) and the
	// Run goroutine (routing state events). Skip non-init_ack messages until
	// the handshake response is received.
	var ackEnv events.Envelope
	var preInitEvents []Event
	for {
		_, r, err := conn.NextReader()
		if err != nil {
			if c.conn != nil {
				_ = c.conn.Close()
			}
			return nil, fmt.Errorf("client: read init ack: %w", err)
		}
		raw, err := io.ReadAll(r)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("client: read init ack body: %w", err)
		}

		if err := json.Unmarshal(raw, &ackEnv); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("client: decode init ack: %w", err)
		}
		if ackEnv.Event.Type == "init_ack" {
			break
		}
		// Buffer pre-init_ack events for delivery after the handshake.
		// The gateway may route state events before init_ack due to a race
		// between the Bridge (starting worker) and Conn (sending init_ack).
		preInitEvents = append(preInitEvents, Event{
			Type:    string(ackEnv.Event.Type),
			Seq:     ackEnv.Seq,
			Session: ackEnv.SessionID,
			Data:    ackEnv.Event.Data,
		})
	}

	ack := parseInitAck(&ackEnv)

	// If the gateway rejected init with an error, propagate it to the caller.
	if ack.Code != "" || ack.Error != "" {
		_ = conn.Close()
		return nil, fmt.Errorf("client: init rejected: %s: %s", ack.Code, ack.Error)
	}

	c.mu.Lock()
	c.conn = conn
	c.sessionID = ack.SessionID
	c.state = ack.State
	c.seq = 1
	c.mu.Unlock()

	c.wg.Add(3)
	go c.recvPump()
	go c.sendPump()
	go c.pingPump()

	// Deliver any events buffered during the handshake.
	for _, evt := range preInitEvents {
		c.deliver(evt)
	}

	return ack, nil
}

// Events returns a receive-only channel of inbound events.
// Each call to Events() returns a new channel that receives all subsequent events.
// To stop receiving events and free resources, call Unsubscribe(ch). Call it
// once, not per event: a channel that is never drained stops the receive pump.
func (c *Client) Events() <-chan Event {
	ch := make(chan Event, SendChannelCap)
	c.mu.Lock()
	c.listeners = append(c.listeners, &subscriber{ch: ch, done: make(chan struct{})})
	c.mu.Unlock()
	return ch
}

// Unsubscribe stops delivering events to the given channel. The channel is not
// closed: deliver() sends outside the client lock, so closing it here would
// race an in-flight send and panic. Close() still closes every outstanding
// channel once the receive pump has stopped, so a `for range` over a channel
// still terminates on client shutdown.
func (c *Client) Unsubscribe(ch <-chan Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.listeners {
		if (<-chan Event)(s.ch) == ch {
			c.listeners = append(c.listeners[:i], c.listeners[i+1:]...)
			close(s.done)
			return
		}
	}
}

// SessionID returns the current session ID.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// State returns the current session state.
func (c *Client) State() SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// SendInput sends a user input message.
func (c *Client) SendInput(ctx context.Context, content string, metadata ...map[string]any) error {
	return c.SendInputWithID(ctx, aep.NewID(), content, metadata...)
}

// SendInputWithID sends input with a caller-stable client message ID. Reusing
// the same ID with the same payload after a reconnect is idempotent; reusing it
// with different data is rejected by the gateway.
func (c *Client) SendInputWithID(ctx context.Context, clientMessageID, content string, metadata ...map[string]any) error {
	if clientMessageID == "" {
		return errors.New("client: client message ID is required")
	}
	data := map[string]any{"content": content}
	if len(metadata) > 0 && metadata[0] != nil {
		data["metadata"] = metadata[0]
	}
	return c.sendWithID(ctx, clientMessageID, events.Input, data, PriorityData)
}

// SendInputAsync sends input and blocks until the resulting task is complete or context is canceled.
func (c *Client) SendInputAsync(ctx context.Context, content string, metadata ...map[string]any) (*DoneData, error) {
	eventsCh := c.Events()
	defer c.Unsubscribe(eventsCh)

	// Create a temporary channel to wait for the specific completion of this input.
	// Since the gateway is sequential per-session, the next 'done' or 'error' event
	// will correspond to this input.
	doneCh := make(chan struct {
		data *DoneData
		err  error
	}, 1)

	// Subscribe to events.
	// Note: In a production client, we'd want a more robust way to match
	// done events to inputs (e.g. via parent IDs), but AEP v1 is currently
	// sequential within a session.
	go func() {
		for evt := range eventsCh {
			switch evt.Type {
			case EventDone:
				if d, ok := evt.AsDoneData(); ok {
					doneCh <- struct {
						data *DoneData
						err  error
					}{data: &d}
					return
				}
			case EventError:
				if d, ok := evt.AsErrorData(); ok {
					doneCh <- struct {
						data *DoneData
						err  error
					}{err: fmt.Errorf("gateway error: %s: %s", d.Code, d.Message)}
					return
				}
			}
		}
	}()

	if err := c.SendInput(ctx, content, metadata...); err != nil {
		return nil, err
	}

	select {
	case result := <-doneCh:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, ErrNotConnected
	}
}

// SendPermissionResponse approves or denies a tool permission.
func (c *Client) SendPermissionResponse(ctx context.Context, id string, approved bool, reason string) error {
	data := map[string]any{"id": id, "allowed": approved}
	if reason != "" {
		data["reason"] = reason
	}
	return c.send(ctx, events.PermissionResponse, data, PriorityControl)
}

// SendQuestionResponse sends answers to a question request.
func (c *Client) SendQuestionResponse(ctx context.Context, id string, answers map[string]string) error {
	return c.send(ctx, events.QuestionResponse, QuestionResponseData{
		ID:      id,
		Answers: answers,
	}, PriorityControl)
}

// SendElicitationResponse sends a response to an elicit request.
func (c *Client) SendElicitationResponse(ctx context.Context, id, action string, content map[string]any) error {
	return c.send(ctx, events.ElicitationResponse, ElicitationResponseData{
		ID:      id,
		Action:  action,
		Content: content,
	}, PriorityControl)
}

// SendControl sends a control action ("terminate" | "delete").
func (c *Client) SendControl(ctx context.Context, action string) error {
	return c.send(ctx, events.Control, &events.ControlData{
		Action: events.ControlAction(action),
	}, PriorityControl)
}

// SendReset sends a reset control action to clear session context.
// The session will restart with a fresh worker, preserving the session ID.
func (c *Client) SendReset(ctx context.Context, reason string) error {
	return c.sendControlWithReason(ctx, events.ControlActionReset, reason)
}

// SendGC sends a gc control action to archive the session.
// The worker is terminated but session history is preserved for resume.
func (c *Client) SendGC(ctx context.Context, reason string) error {
	return c.sendControlWithReason(ctx, events.ControlActionGC, reason)
}

func (c *Client) sendControlWithReason(ctx context.Context, action events.ControlAction, reason string) error {
	data := &events.ControlData{Action: action}
	if reason != "" {
		data.Reason = reason
	}
	return c.send(ctx, events.Control, data, PriorityControl)
}

// Close gracefully shuts down the client.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	c.mu.Unlock()

	// Cancel context to unblock sendPump (select on ctx.Done) and pingPump.
	c.cancel()
	// Close the WebSocket connection to unblock recvPump (NextReader).
	// This must happen before wg.Wait() to avoid deadlock.
	if conn != nil {
		_ = conn.Close()
	}
	// Close sendCh to unblock sendPump (range c.sendCh).
	// Safe because c.closed=true prevents new writes to sendCh.
	close(c.sendCh)
	c.wg.Wait()

	c.mu.Lock()
	for _, s := range c.listeners {
		close(s.ch)
	}
	c.listeners = nil
	c.mu.Unlock()
	return nil
}

// ─── Private ─────────────────────────────────────────────────────────────────

func (c *Client) send(ctx context.Context, kind events.Kind, data any, priority Priority) error {
	return c.sendWithID(ctx, aep.NewID(), kind, data, priority)
}

func (c *Client) sendWithID(ctx context.Context, eventID string, kind events.Kind, data any, priority Priority) error {
	c.mu.Lock()
	closed := c.closed
	sessionID := c.sessionID
	c.seq++
	seq := c.seq
	conn := c.conn
	c.mu.Unlock()

	if conn == nil || closed {
		return ErrNotConnected
	}

	env := events.NewEnvelope(eventID, sessionID, seq, kind, data)
	env.Priority = priority
	frame, err := aep.EncodeJSON(env)
	if err != nil {
		return err
	}
	select {
	case c.sendCh <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrNotConnected
	}
}

func (c *Client) recvPump() {
	defer c.wg.Done()
	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}

		_, r, err := conn.NextReader()
		if err != nil {
			if isClosedWS(err) {
				return
			}
			c.deliver(Event{Type: EventError, Data: map[string]any{"code": "read_error", "message": err.Error()}})
			return
		}

		raw, err := io.ReadAll(r)
		if err != nil {
			c.deliver(Event{Type: EventError, Data: map[string]any{"code": "read_error", "message": err.Error()}})
			return
		}

		env, err := aep.DecodeLine(raw)
		if err != nil {
			c.deliver(Event{Type: EventError, Data: map[string]any{"code": "decode_error", "message": err.Error()}})
			return
		}

		// Update local state on state events.
		if env.Event.Type == events.State {
			if d, ok := events.DecodeAs[StateData](env.Event.Data); ok {
				c.mu.Lock()
				c.state = d.State
				c.mu.Unlock()
			}
		}

		c.deliver(Event{
			Type:    string(env.Event.Type),
			Seq:     env.Seq,
			Session: env.SessionID,
			Data:    env.Event.Data,
		})

		// `done` terminates a TURN, not the AEP session. A session that
		// received a supplement/injected input runs a second turn and the
		// gateway emits a second `done`; returning here made the client blind
		// to every event after the first turn, so a multi-turn session looked
		// hung to any caller waiting on the second turn's result. Proven live
		// against a real Worker: frames seq 12 and 13 were on the wire and
		// never surfaced. Only a closed connection ends this pump.
		//
		// `error` still ends it. That is unverified end-to-end — a retryable
		// turn error also leaves the session alive (autoRetry), so it likely
		// belongs here too, but no live run has shown it yet and changing it
		// blind would trade one unproven behaviour for another.
		if env.Event.Type == events.Error {
			return
		}
	}
}

func (c *Client) sendPump() {
	defer c.wg.Done()
	for frame := range c.sendCh {
		c.mu.Lock()
		conn := c.conn
		closed := c.closed
		c.mu.Unlock()
		if conn == nil || closed {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			c.logger.Debug("send pump: write failed", "err", err)
			return
		}
	}
}

func (c *Client) pingPump() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			deadline := time.Now().Add(10 * time.Second)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				return
			}
		}
	}
}

func (c *Client) deliver(evt Event) {
	c.mu.Lock()
	subscribers := make([]*subscriber, len(c.listeners))
	copy(subscribers, c.listeners)
	c.mu.Unlock()

	for _, s := range subscribers {
		// Skip anyone who unsubscribed while this event was in flight.
		select {
		case <-s.done:
			continue
		default:
		}
		// Critical events (done/error/state) must never be dropped — block until
		// delivered or the client is shutting down.
		if evt.Type == EventDone || evt.Type == EventError || evt.Type == EventState {
			select {
			case s.ch <- evt:
			case <-s.done:
			case <-c.ctx.Done():
			}
			continue
		}
		// Non-critical events (delta, raw, etc.) are silently dropped under backpressure.
		select {
		case s.ch <- evt:
		case <-s.done:
		default:
			// log skip if needed
		}
	}
}

func parseInitAck(env *events.Envelope) *InitAckData {
	ack, _ := events.DecodeAs[InitAckData](env.Event.Data)
	if ack.SessionID == "" {
		ack.SessionID = env.SessionID
	}
	if ack.State == "" {
		ack.State = StateCreated
	}
	return &ack
}

func isClosedWS(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	)
}

func backoffDuration(attempt int) time.Duration {
	const base = 1 * time.Second
	const max = 30 * time.Second
	d := base * (1 << uint(attempt-1))
	if d > max {
		return max
	}
	return d
}
