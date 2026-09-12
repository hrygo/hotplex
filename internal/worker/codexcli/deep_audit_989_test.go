package codexcli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deep989Context struct {
	context.Context
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (c *deep989Context) Deadline() (time.Time, bool) {
	c.once.Do(func() { close(c.entered); <-c.proceed })
	return time.Time{}, false
}

func TestDeepAudit989QueuedFrameCannotMoveToReplacementTransport(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	oldWriter, newWriter := &auditRPCSink{}, &auditRPCSink{}
	m.stdin = oldWriter
	ctx := &deep989Context{Context: context.Background(), entered: make(chan struct{}), proceed: make(chan struct{})}
	result := make(chan error, 1)
	m.writeMu.Lock()
	go func() { result <- m.Notify(ctx, "old-generation/probe", map[string]string{"request": "old"}) }()
	<-ctx.entered
	// The call has entered writeFrame but cannot encode. This transfer is
	// channel-ordered, so the test itself never races on the source fixture.
	m.stdin = newWriter
	close(ctx.proceed)
	m.writeMu.Unlock()
	require.NoError(t, <-result)
	require.Empty(t, newWriter.String(), "an old call migrated to a new process stdin")
	require.Contains(t, oldWriter.String(), "old-generation/probe")
}
