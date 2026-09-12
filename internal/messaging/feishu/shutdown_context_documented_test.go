package feishu

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestD06ExpiredDrainCancelsAndDiscardsTail(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := NewChatQueue(nil)
		entered := make(chan struct{})
		require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }))
		<-entered
		called := make(chan struct{}, 1)
		require.NoError(t, q.Enqueue("chat", func(context.Context) error { called <- struct{}{}; return nil }))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, q.CloseContext(ctx), context.DeadlineExceeded)
		q.Close() // observe cooperative worker convergence
		require.EqualValues(t, 1, q.DiscardedTasks())
		require.Empty(t, called)
		require.NoError(t, q.CloseContext(context.Background()))
	})
}
