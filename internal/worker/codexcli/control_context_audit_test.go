package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
)

// audit987Sink exercises the actual manager encoder and pending response path.
// It never starts a process or makes an external API call.
type audit987Sink struct {
	mu      sync.Mutex
	manager *CodexAppServerManager
	frames  []JSONRPCRequest
	reply   bool
}

func (s *audit987Sink) Close() error { return nil }
func (s *audit987Sink) Write(p []byte) (int, error) {
	var frame JSONRPCRequest
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	reply := s.reply
	s.mu.Unlock()
	if reply && frame.ID != 0 {
		if pending, ok := s.manager.pending.Load(frame.ID); ok {
			pending.(chan *JSONRPCResponse) <- &JSONRPCResponse{Result: json.RawMessage(`{"ok":true}`)}
		}
	}
	return len(p), nil
}
func (s *audit987Sink) snapshot() []JSONRPCRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]JSONRPCRequest(nil), s.frames...)
}
func audit987Worker() (*AppServerWorker, *audit987Sink) {
	m := auditRPCManager()
	sink := &audit987Sink{manager: m, reply: true}
	m.stdin = sink
	return &AppServerWorker{
		BaseWorker: base.NewBaseWorker(nil, nil), manager: m,
		threadID: "audit-thread", turnID: "audit-turn",
		commands: NewServerCommander(m, "audit-thread"),
	}, sink
}

type audit987Control struct {
	name     string
	method   string
	params   string
	response bool
	invoke   func(context.Context, *AppServerWorker) error
}

func audit987Controls() []audit987Control {
	return []audit987Control{
		{"steer", "turn/steer", `{"threadId":"audit-thread","expectedTurnId":"audit-turn","input":[{"type":"text","text":"supplement"}]}`, true,
			func(ctx context.Context, w *AppServerWorker) error { return w.InjectMidTurn(ctx, "supplement", nil) }},
		{"compact", "thread/compact/start", `{"threadId":"audit-thread"}`, true,
			func(ctx context.Context, w *AppServerWorker) error { return w.Compact(ctx, nil) }},
		{"rewind", "thread/rollback", `{"threadId":"audit-thread","numTurns":2}`, true,
			func(ctx context.Context, w *AppServerWorker) error { return w.Rewind(ctx, "2") }},
		{"commander_compact", "thread/compact/start", `{"threadId":"audit-thread"}`, true,
			func(ctx context.Context, w *AppServerWorker) error { return w.commands.Compact(ctx, nil) }},
		{"mcp_status", "mcpServerStatus/list", `{}`, true,
			func(ctx context.Context, w *AppServerWorker) error {
				_, err := w.SendControlRequest(ctx, "mcp_status", nil)
				return err
			}},
		{"mcp_refresh", "config/mcpServer/reload", ``, false,
			func(ctx context.Context, w *AppServerWorker) error {
				_, err := w.SendControlRequest(ctx, "mcp_refresh", nil)
				return err
			}},
		{"mcp_oauth", "mcpServer/oauth/login", `{"name":"audit-server"}`, true,
			func(ctx context.Context, w *AppServerWorker) error {
				_, err := w.SendControlRequest(ctx, "mcp_oauth", map[string]any{"server_name": "audit-server"})
				return err
			}},
	}
}

func TestAudit987ControlsPreCanceled(t *testing.T) {
	t.Parallel()
	for _, tc := range audit987Controls() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, sink := audit987Worker()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := tc.invoke(ctx, w)
			require.Empty(t, sink.snapshot(), "canceled control must not write protocol bytes")
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestAudit987ControlsCancelResponseWait(t *testing.T) {
	t.Parallel()
	for _, tc := range audit987Controls() {
		if !tc.response {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				w, sink := audit987Worker()
				sink.reply = false
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- tc.invoke(ctx, w) }()
				synctest.Wait()
				cancel()
				synctest.Wait()
				var err error
				select {
				case err = <-result:
				default:
					// Retire the baseline call before reporting its failure, without sleeps.
					w.manager.pending.Range(func(_, value any) bool {
						value.(chan *JSONRPCResponse) <- &JSONRPCResponse{Result: json.RawMessage(`{}`)}
						return true
					})
					synctest.Wait()
					<-result
					t.Fatal("control ignored cancellation while awaiting RPC response")
				}
				require.ErrorIs(t, err, context.Canceled)
				pending := 0
				w.manager.pending.Range(func(_, _ any) bool { pending++; return true })
				require.Zero(t, pending)
				require.True(t, w.manager.IsRunning())
			})
		})
	}
}

func TestAudit987ControlsPreserveProtocol(t *testing.T) {
	t.Parallel()
	for _, tc := range audit987Controls() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, sink := audit987Worker()
			require.NoError(t, tc.invoke(context.Background(), w))
			frames := sink.snapshot()
			require.Len(t, frames, 1)
			require.Equal(t, tc.method, frames[0].Method)
			if tc.params == "" {
				require.Empty(t, frames[0].Params)
			} else {
				require.JSONEq(t, tc.params, string(frames[0].Params))
			}
		})
	}
}

func TestAudit987TurnPreCanceledNotUnavailable(t *testing.T) {
	t.Parallel()
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, sink := audit987Worker()
			ctx, cancel := context.WithCancel(context.Background())
			cause := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
				cause = context.DeadlineExceeded
			}
			cancel()
			err := w.startTurn(ctx, []TurnInputItem{{Type: "text", Text: "probe"}})
			require.ErrorIs(t, err, cause)
			require.Empty(t, sink.snapshot())
			var workerErr *worker.WorkerError
			if errors.As(err, &workerErr) {
				require.NotEqual(t, worker.ErrKindUnavailable, workerErr.Kind)
			}
		})
	}
}

func TestAudit987CanceledQueuedWriteNeverExecutes(t *testing.T) {
	t.Parallel()
	w, sink := audit987Worker()
	w.manager.writeMu.Lock()
	// An explicit deadline exercises cancellation while waiting for writeMu.
	// Mutex waits are not durably blocked operations in testing/synctest.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := w.startTurn(ctx, []TurnInputItem{{Type: "text", Text: "probe"}})
	w.manager.writeMu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var workerErr *worker.WorkerError
	if errors.As(err, &workerErr) {
		require.NotEqual(t, worker.ErrKindUnavailable, workerErr.Kind)
	}
	require.Never(t, func() bool { return len(sink.snapshot()) != 0 }, 50*time.Millisecond, time.Millisecond,
		"abandoned queued write was executed after cancellation")
}

func TestAudit987InFlightWriteRemainsUnavailable(t *testing.T) {
	t.Parallel()
	w, _ := audit987Worker()
	writer := newBlockingFrameWriter()
	defer writer.Release()
	w.manager.stdin = writer
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- w.startTurn(ctx, []TurnInputItem{{Type: "text", Text: "probe"}}) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("frame encoder did not start")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		var workerErr *worker.WorkerError
		require.ErrorAs(t, err, &workerErr)
		require.Equal(t, worker.ErrKindUnavailable, workerErr.Kind,
			"a genuinely blocked pipe write must retain its recovery classification")
	case <-time.After(time.Second):
		t.Fatal("in-flight cancellation exceeded its budget")
	}
}
