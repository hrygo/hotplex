package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuditChatQueuePanicPreservesAcceptedTail(t *testing.T) {
	t.Parallel()
	q := NewChatQueue(nil)
	defer q.Close()
	entered, release, tail := make(chan struct{}), make(chan struct{}), make(chan struct{})
	require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error {
		close(entered)
		<-release
		panic("injected task failure")
	}))
	<-entered
	require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error {
		close(tail)
		return nil
	}))
	close(release)
	select {
	case <-tail:
	case <-time.After(time.Second):
		t.Fatal("an accepted task was stranded behind a panicking task")
	}
}
