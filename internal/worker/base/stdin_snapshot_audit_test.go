package base

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuditStdinSnapshotDoesNotWaitForPipeWriter(t *testing.T) {
	t.Parallel()
	c := NewConn(nil, nil, "user", "session")
	defer c.Close()
	c.WriteMu().Lock()
	got := make(chan struct{})
	go func() {
		c.StdinUnlocked()
		_ = c.SessionID()
		close(got)
	}()
	timely := false
	select {
	case <-got:
		timely = true
	case <-time.After(time.Second):
	}
	// Release before asserting so a defective getter does not leak its goroutine.
	c.WriteMu().Unlock()
	<-got
	require.True(t, timely, "metadata snapshot blocked before context-guarded I/O could start")
}

func TestAuditStdinSnapshotConcurrentCloseInput(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		reader, writer, err := os.Pipe()
		require.NoError(t, err)
		c := NewConn(nil, writer, "user", "session")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 256; j++ {
				c.StdinUnlocked()
			}
		}()
		go func() { defer wg.Done(); <-start; _ = c.CloseInput() }()
		close(start)
		wg.Wait()
		stdin, mu := c.StdinUnlocked()
		require.Nil(t, stdin)
		require.Same(t, c.WriteMu(), mu)
		require.NoError(t, c.Close())
		require.NoError(t, reader.Close())
	}
}
