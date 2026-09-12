package codexcli

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type documentedWriter struct {
	closed atomic.Bool
	writes atomic.Int64
}

func (w *documentedWriter) Close() error { w.closed.Store(true); return nil }
func (w *documentedWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	w.writes.Add(1)
	return len(p), nil
}

// Deadline is queried before dispatching the write helper. It supplies a
// deterministic signal while the test holds writeMu; no sleeps or race polling.
type documentedSignalContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *documentedSignalContext) Deadline() (time.Time, bool) {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Deadline()
}

func TestD03QueuedWriteCannotEnterReplacementTransport(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	old, next := &documentedWriter{}, &documentedWriter{}
	m.stdin = old
	m.writeMu.Lock()
	ctx := &documentedSignalContext{Context: context.Background(), entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- m.Notify(ctx, "probe", nil) }()
	<-ctx.entered
	// The fixture performs the same replacement as native process startup.
	// The channel signal orders this mutation after snapshot capture, and
	// releasing writeMu orders the old implementation's read after mutation.
	m.stdin = next
	require.NoError(t, old.Close())
	m.writeMu.Unlock()
	err := <-done
	require.Error(t, err, "old request silently entered the replacement process")
	require.Zero(t, next.writes.Load())
}
func TestD03AbsentTransportReturnsError(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	m.stdin = nil
	require.NotPanics(t, func() { require.Error(t, m.Notify(context.Background(), "probe", nil)) })
}
