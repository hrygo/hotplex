package gateway

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

type deep995Conn struct{ send func(context.Context) error }

func (c *deep995Conn) WriteCtx(ctx context.Context, _ *events.Envelope) error { return c.send(ctx) }
func (c *deep995Conn) Close() error                                           { return nil }
func deep995Writer(pc *deep995Conn) *pcEntry {
	return newPCEntry(context.Background(), pc, pcEntryConfig{WriteBuffer: 8, DropThreshold: 7, CoalesceIntvl: time.Millisecond, CoalesceSize: 200, TerminalTimeout: time.Second}, slog.Default())
}
func deep995Done() *events.Envelope { return &events.Envelope{Event: events.Event{Type: events.Done}} }

func TestDeepAudit995SuccessfulReceiptNeverBecomesTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := deep995Writer(&deep995Conn{send: func(context.Context) error { return nil }})
		defer e.Close()
		result := make(chan error, 1)
		require.NoError(t, e.EnqueueWrite(context.Background(), deep995Done(), result))
		require.NoError(t, <-result)
		// Advance the bubble's virtual clock beyond the receipt budget.
		<-time.After(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("completed receipt produced a second outcome: %v", err)
		default:
		}
	})
}

func TestDeepAudit995TerminalWithoutReceiptKeepsLiveContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		observed := make(chan error, 1)
		e := deep995Writer(&deep995Conn{send: func(ctx context.Context) error { observed <- ctx.Err(); return nil }})
		defer e.Close()
		require.NoError(t, e.EnqueueWrite(context.Background(), deep995Done(), nil))
		require.NoError(t, <-observed, "not requesting a receipt must not cancel the platform write")
	})
}

func TestDeepAudit995LateWriteCannotOverwriteTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		e := deep995Writer(&deep995Conn{send: func(context.Context) error { close(entered); <-release; return nil }})
		defer e.Close()
		defer releaseOnce.Do(func() { close(release) })
		result := make(chan error, 1)
		require.NoError(t, e.EnqueueWrite(context.Background(), deep995Done(), result))
		<-entered
		require.ErrorIs(t, <-result, context.DeadlineExceeded)
		releaseOnce.Do(func() { close(release) })
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("late platform completion produced a second outcome: %v", err)
		default:
		}
	})
}
