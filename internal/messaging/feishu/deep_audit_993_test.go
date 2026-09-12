package feishu

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit993AdapterShutdownHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := NewChatQueue(nil)
		a := &Adapter{chatQueue: q}
		entered := make(chan struct{})
		require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }))
		<-entered
		var tail atomic.Int32
		require.NoError(t, q.Enqueue("chat", func(context.Context) error { tail.Add(1); return nil }))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		err := a.Close(ctx)
		elapsed := time.Since(start)
		synctest.Wait()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.LessOrEqual(t, elapsed, 2*time.Second)
		require.Zero(t, tail.Load(), "expired shutdown must not begin queued side effects")
		require.ErrorIs(t, q.Enqueue("chat", func(context.Context) error { return nil }), ErrChatQueueClosed)
		q.Close()
	})
}

func TestDeepAudit993UncooperativeTaskDoesNotHoldAdapterClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := NewChatQueue(nil)
		a := &Adapter{chatQueue: q}
		entered, release := make(chan struct{}), make(chan struct{})
		require.NoError(t, q.Enqueue("chat", func(context.Context) error { close(entered); <-release; return nil }))
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- a.Close(ctx) }()
		<-time.After(2 * time.Second) // deterministic virtual time
		synctest.Wait()
		timely := false
		var err error
		select {
		case err = <-result:
			timely = true
		default:
		}
		// Always release the external task before reporting a baseline failure.
		close(release)
		if !timely {
			err = <-result
		}
		q.Close()
		require.True(t, timely, "Adapter.Close ignored its budget")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestDeepAudit993LegacyCloseStillDrainsAcceptedTasks(t *testing.T) {
	t.Parallel()
	q := NewChatQueue(nil)
	var completed atomic.Int32
	for i := 0; i < 16; i++ {
		require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error {
			require.NoError(t, ctx.Err())
			completed.Add(1)
			return nil
		}))
	}
	q.Close()
	q.Close()
	require.EqualValues(t, 16, completed.Load())
}
