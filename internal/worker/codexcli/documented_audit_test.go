package codexcli

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// documentedSink drives the real encoder, dispatcher and Worker lifecycle;
// only the external Codex process is replaced by an in-memory protocol peer.
type documentedSink struct {
	mu      sync.Mutex
	m       *CodexAppServerManager
	reply   bool
	failure bool
	result  json.RawMessage
	frames  []JSONRPCRequest
}

func (s *documentedSink) Close() error { return nil }
func (s *documentedSink) Write(p []byte) (int, error) {
	var frame JSONRPCRequest
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	reply, failure, result := s.reply, s.failure, append(json.RawMessage(nil), s.result...)
	s.mu.Unlock()
	if reply && frame.ID != 0 {
		resp := &JSONRPCResponse{ID: frame.ID, Result: result}
		if failure {
			resp.Error = &JSONRPCError{Code: -32000, Message: "injected rejection"}
		}
		s.m.dispatchResponse(resp)
	}
	return len(p), nil
}
func (s *documentedSink) setResult(reply, failure bool, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = reply
	s.failure = failure
	s.result = json.RawMessage(result)
}
func (s *documentedSink) snapshot() []JSONRPCRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]JSONRPCRequest(nil), s.frames...)
}
func documentedWorker(t *testing.T) (*AppServerWorker, *documentedSink) {
	t.Helper()
	m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{CallTimeout: time.Hour, IdleDrainPeriod: time.Hour})
	s := &documentedSink{m: m}
	s.setResult(true, false, `{"thread":{"id":"documented-thread"},"turn":{"id":"documented-turn"}}`)
	m.stdin = s
	m.state = stateRunning
	m.refs = 1 // a separate session owns one reference
	w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}
	require.NoError(t, w.Start(context.Background(), worker.SessionInfo{SessionID: "documented-session", UserID: "user"}))
	frames := s.snapshot()
	require.NotEmpty(t, frames, "fixture must traverse the real protocol encoder")
	require.Equal(t, "thread/start", frames[0].Method)
	return w, s
}

func TestD01ManagerThenConnectionClose(t *testing.T) {
	t.Parallel()
	w, _ := documentedWorker(t)
	c := w.conn
	w.manager.Shutdown(context.Background())
	require.NotPanics(t, func() { require.NoError(t, c.Close()) })
}
func TestD01SendAfterClose(t *testing.T) {
	t.Parallel()
	w, _ := documentedWorker(t)
	c := w.conn
	w.manager.Unsubscribe(w.threadID)
	require.NoError(t, c.Close())
	require.NotPanics(t, func() { require.False(t, c.TrySend(&events.Envelope{})) })
}
func TestD01ConcurrentSendShutdown(t *testing.T) {
	t.Parallel()
	// Test failure is reported without allowing the old implementation's panic
	// to abort unrelated red regressions in this package.
	for i := 0; i < 20; i++ {
		w, _ := documentedWorker(t)
		c := w.conn
		start := make(chan struct{})
		panics := make(chan any, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			<-start
			for j := 0; j < 300; j++ {
				c.TrySend(&events.Envelope{})
			}
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			<-start
			w.manager.Shutdown(context.Background())
			_ = c.Close()
		}()
		close(start)
		wg.Wait()
		close(panics)
		for r := range panics {
			t.Errorf("send/close panic: %v", r)
		}
	}
}
