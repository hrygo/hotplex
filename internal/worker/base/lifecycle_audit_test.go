package base

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/proc"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestAuditProcCompletionPreservesReplacement(t *testing.T) {
	t.Parallel()
	for _, replaceProc := range []bool{false, true} {
		name := "same_process"
		if replaceProc {
			name = "replacement_process"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			oldProc, replacement := &proc.Manager{}, &proc.Manager{}
			w := NewBaseWorker(nil, nil)
			w.Proc = oldProc
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var code int
			var err error
			go func() {
				defer close(finished)
				code, err = withProcResult(w, func(p *proc.Manager) (int, error) {
					close(entered)
					<-release
					return 42, nil
				}, -1, errors.New("missing process"))
			}()
			<-entered
			if replaceProc {
				w.Mu.Lock()
				w.Proc = replacement
				w.Mu.Unlock()
			}
			close(release)
			<-finished
			require.NoError(t, err)
			require.Equal(t, 42, code)
			if replaceProc {
				require.Same(t, replacement, w.Proc)
			} else {
				require.Nil(t, w.Proc)
			}
		})
	}
}

func TestAuditConnTrySendAfterClose(t *testing.T) {
	t.Parallel()
	c := NewConn(nil, nil, "user", "session")
	require.NoError(t, c.Close())
	require.NotPanics(t, func() {
		require.False(t, c.TrySend(&events.Envelope{Event: events.Event{Type: events.Done}}))
	})
	require.NoError(t, c.Close())
}

func TestAuditConnSessionIDConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewConn(nil, nil, "user", "initial")
	defer c.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			c.SetSessionID("replacement")
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			_ = c.SessionID()
		}
	}()
	close(start)
	wg.Wait()
}
