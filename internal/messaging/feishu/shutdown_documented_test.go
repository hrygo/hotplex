package feishu

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestD06AdapterShutdownHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := NewChatQueue(nil)
		a := &Adapter{chatQueue: q}
		started := make(chan struct{})
		require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }))
		<-started
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		finished := make(chan error, 1)
		go func() { finished <- a.Close(ctx) }()
		synctest.Wait()
		select {
		case err := <-finished:
			require.ErrorIs(t, err, context.Canceled)
		default:
			q.Abort("chat") // retire the original implementation before failing
			<-finished
			t.Fatal("Adapter.Close ignored its caller budget while draining queue")
		}
	})
}
func TestD06GracefulQueueCloseStillDrains(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := NewChatQueue(nil)
		finished := make(chan struct{}, 2)
		for i := 0; i < 2; i++ {
			require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error { time.Sleep(time.Second); finished <- struct{}{}; return nil }))
		}
		q.Close()
		require.Len(t, finished, 2)
		require.ErrorIs(t, q.Enqueue("chat", func(context.Context) error { return nil }), ErrChatQueueClosed)
	})
}
