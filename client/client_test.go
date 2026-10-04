package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// TestClientSurfacesEventsAfterTheFirstDone pins the AEP session/turn
// distinction on the client side. `done` terminates a TURN: a session that
// received a supplement/injected input runs a second turn and the gateway
// emits a second `done`. A client that stops reading at the first `done` is
// blind to the rest of the session, which is what a live run against a real
// Worker showed — frames seq 12 and 13 were on the wire and never surfaced,
// so the session looked hung to anything waiting on the second turn.
func TestClientSurfacesEventsAfterTheFirstDone(t *testing.T) {
	t.Parallel()

	const sessionID = "two-turn-session"

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	// Frames are written from the handler goroutine; failures are reported back
	// over a channel because require must not be called off the test goroutine.
	var (
		errMu    sync.Mutex
		writeErr []error
	)
	record := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		writeErr = append(writeErr, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Consume the client's init, then answer with the handshake ack.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		write := func(seq int64, kind events.Kind, data any) {
			frame, err := aep.EncodeJSON(events.NewEnvelope(aep.NewID(), sessionID, seq, kind, data))
			if err != nil {
				record(err)
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				record(err)
			}
		}
		write(1, "init_ack", InitAckData{SessionID: sessionID, State: StateRunning})

		// Turn 1 ends here.
		write(2, events.Done, DoneData{Success: true})
		// Turn 2 — dispatched by the supplement that arrived mid-turn — runs on
		// the same connection and ends with its own done.
		write(3, events.MessageDelta, MessageDeltaData{Content: "second turn"})
		write(4, events.Done, DoneData{Success: true})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx, URL("ws"+srv.URL[len("http"):]), WorkerType("claude_code"), APIKey("test-key"))
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	ack, err := c.Connect(ctx)
	require.NoError(t, err)
	require.Equal(t, sessionID, ack.SessionID)

	// One subscription for the whole session, drained continuously.
	eventCh := c.Events()
	defer c.Unsubscribe(eventCh)

	var types []string
	require.Eventually(t, func() bool {
		// Drain the three post-handshake frames. The fixture closes the
		// connection afterwards, which legitimately produces a read_error
		// event; it is not part of what this test is about.
		for len(types) < 3 {
			select {
			case ev := <-eventCh:
				if ev.Type != EventError {
					types = append(types, ev.Type)
				}
			case <-time.After(2 * time.Second):
				return false
			}
		}
		return true
	}, 8*time.Second, 10*time.Millisecond,
		"the client must keep surfacing events after the first done; got %v", types)

	require.Equal(t, []string{
		EventDone, EventMessageDelta, EventDone,
	}, types, "turn 2 must be observable after turn 1's done")

	errMu.Lock()
	defer errMu.Unlock()
	for _, err := range writeErr {
		require.NoError(t, err)
	}
}

func TestEventAsHelpers(t *testing.T) {
	t.Parallel()

	t.Run("AsDoneData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventDone,
			Data: map[string]any{
				"success": true,
				"stats": map[string]any{
					"model": "claude-3-opus",
				},
			},
		}
		d, ok := evt.AsDoneData()
		require.True(t, ok)
		require.True(t, d.Success)
		require.Equal(t, "claude-3-opus", d.Stats["model"])
	})

	t.Run("AsErrorData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventError,
			Data: map[string]any{
				"code":    "SESSION_NOT_FOUND",
				"message": "not found",
			},
		}
		d, ok := evt.AsErrorData()
		require.True(t, ok)
		require.Equal(t, ErrorCode("SESSION_NOT_FOUND"), d.Code)
		require.Equal(t, "not found", d.Message)
	})

	t.Run("AsToolCallData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventToolCall,
			Data: map[string]any{
				"id":   "tc_123",
				"name": "bash",
				"input": map[string]any{
					"command": "ls",
				},
			},
		}
		d, ok := evt.AsToolCallData()
		require.True(t, ok)
		require.Equal(t, "tc_123", d.ID)
		require.Equal(t, "bash", d.Name)
		require.Equal(t, "ls", d.Input["command"])
	})

	t.Run("AsMessageDeltaData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventMessageDelta,
			Data: map[string]any{
				"message_id": "msg_1",
				"content":    "hello",
			},
		}
		d, ok := evt.AsMessageDeltaData()
		require.True(t, ok)
		require.Equal(t, "msg_1", d.MessageID)
		require.Equal(t, "hello", d.Content)
	})

	t.Run("AsStateData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventState,
			Data: map[string]any{
				"state": "running",
			},
		}
		d, ok := evt.AsStateData()
		require.True(t, ok)
		require.Equal(t, SessionState("running"), d.State)
	})

	t.Run("AsReasoningData", func(t *testing.T) {
		t.Parallel()
		evt := Event{
			Type: EventReasoning,
			Data: map[string]any{
				"id":      "r_1",
				"content": "thinking...",
				"model":   "opus",
			},
		}
		d, ok := evt.AsReasoningData()
		require.True(t, ok)
		require.Equal(t, "r_1", d.ID)
		require.Equal(t, "thinking...", d.Content)
		require.Equal(t, "opus", d.Model)
	})

	t.Run("nil_data_returns_false", func(t *testing.T) {
		t.Parallel()
		evt := Event{Type: EventDone, Data: nil}
		_, ok := evt.AsDoneData()
		require.False(t, ok)
	})
}

func TestBackoffDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped at max
		{10, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			t.Parallel()
			got := backoffDuration(tt.attempt)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsClosedWS(t *testing.T) {
	t.Parallel()

	t.Run("normal closure", func(t *testing.T) {
		t.Parallel()
		err := &websocket.CloseError{Code: websocket.CloseNormalClosure}
		require.True(t, isClosedWS(err))
	})

	t.Run("going away", func(t *testing.T) {
		t.Parallel()
		err := &websocket.CloseError{Code: websocket.CloseGoingAway}
		require.True(t, isClosedWS(err))
	})

	t.Run("other error", func(t *testing.T) {
		t.Parallel()
		require.False(t, isClosedWS(errors.New("random error")))
	})
}

func TestParseInitAck(t *testing.T) {
	t.Parallel()

	t.Run("full data", func(t *testing.T) {
		t.Parallel()
		env := &events.Envelope{
			SessionID: "sess_abc",
			Event: events.Event{
				Type: "init_ack",
				Data: map[string]any{
					"session_id": "sess_xyz",
					"state":      "running",
					"server_caps": map[string]any{
						"protocol_version":   "aep/v1",
						"worker_type":        "claude_code",
						"supports_resume":    true,
						"supports_delta":     true,
						"supports_tool_call": false,
						"supports_ping":      true,
						"max_frame_size":     float64(1048576),
						"max_turns":          float64(100),
						"tools":              []any{"Read", "Bash"},
					},
				},
			},
		}
		ack := parseInitAck(env)
		require.Equal(t, "sess_xyz", ack.SessionID)
		require.Equal(t, StateRunning, ack.State)
		require.Equal(t, "aep/v1", ack.ServerCaps.ProtocolVersion)
		require.Equal(t, "claude_code", ack.ServerCaps.WorkerType)
		require.True(t, ack.ServerCaps.SupportsResume)
		require.True(t, ack.ServerCaps.SupportsDelta)
		require.False(t, ack.ServerCaps.SupportsTool)
		require.True(t, ack.ServerCaps.SupportsPing)
		require.Equal(t, 1048576, ack.ServerCaps.MaxFrameSize)
		require.Equal(t, 100, ack.ServerCaps.MaxTurns)
		require.Equal(t, []string{"Read", "Bash"}, ack.ServerCaps.Tools)
	})

	t.Run("minimal data", func(t *testing.T) {
		t.Parallel()
		env := &events.Envelope{
			SessionID: "sess_abc",
			Event: events.Event{
				Type: "init_ack",
				Data: map[string]any{},
			},
		}
		ack := parseInitAck(env)
		require.Equal(t, "sess_abc", ack.SessionID)
		require.Equal(t, StateCreated, ack.State) // defaults to created
	})

	t.Run("nil data", func(t *testing.T) {
		t.Parallel()
		env := &events.Envelope{
			SessionID: "sess_abc",
			Event:     events.Event{Type: "init_ack"},
		}
		ack := parseInitAck(env)
		require.Equal(t, "sess_abc", ack.SessionID)
		require.Equal(t, StateCreated, ack.State)
	})
}

func TestSendInputBeforeConnect(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{ctx: ctx, cancel: func() {}, closed: false}
	err := c.SendInput(context.Background(), "test")
	require.ErrorIs(t, err, ErrNotConnected)
}

func TestSendInputWithIDRejectsEmptyID(t *testing.T) {
	t.Parallel()
	c := &Client{}
	err := c.SendInputWithID(context.Background(), "", "test")
	require.EqualError(t, err, "client: client message ID is required")
}

func TestEvent_AsInputAckData(t *testing.T) {
	t.Parallel()
	event := Event{Data: map[string]any{
		"client_message_id": "evt-1",
		"execution_id":      "exec-1",
		"status":            "delivered",
	}}
	ack, ok := event.AsInputAckData()
	require.True(t, ok)
	require.Equal(t, "evt-1", ack.ClientMessageID)
	require.Equal(t, "exec-1", ack.ExecutionID)
	require.Equal(t, ExecutionStatusDelivered, ack.Status)
}
