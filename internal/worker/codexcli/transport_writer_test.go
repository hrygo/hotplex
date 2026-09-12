package codexcli

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type deep989Discard struct{}

func (deep989Discard) Write(p []byte) (int, error) { return len(p), nil }
func (deep989Discard) Close() error                { return nil }

func TestDeepAudit989NilAndConcurrentTransportSnapshots(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	m.setTransportWriter(nil)
	require.ErrorIs(t, m.Notify(context.Background(), "probe", nil), io.ErrClosedPipe)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			m.setTransportWriter(deep989Discard{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = m.Notify(context.Background(), "probe", nil)
		}
	}()
	wg.Wait()
}
