package messaging

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuditDedupExpiresBeforeSweep(t *testing.T) {
	t.Parallel()
	d := NewDedup(2, time.Hour)
	old, accepted := d.TryRecordWithHandle("message")
	require.True(t, accepted)
	d.mu.Lock()
	entry := d.entries["message"]
	entry.recordedAt = time.Now().Add(-2 * time.Hour)
	d.entries["message"] = entry
	d.mu.Unlock()
	fresh, accepted := d.TryRecordWithHandle("message")
	require.True(t, accepted, "TTL expiry must not depend on the cleanup scheduler")
	require.NotNil(t, fresh)
	d.Rollback(old)
	require.Equal(t, 1, d.Len(), "an old failure must not erase a fresh retry")
	require.False(t, d.TryRecord("message"))
	d.Rollback(fresh)
	require.Zero(t, d.Len())
}

func TestAuditDedupCloseBeforeStart(t *testing.T) {
	t.Parallel()
	d := NewDedup(2, time.Hour)
	d.Close()
	d.StartCleanup()
	d.Close()
	select {
	case <-d.done:
	default:
		// Close the baseline's orphaned loop before reporting its failure.
		close(d.done)
		t.Fatal("StartCleanup resurrected a closed deduplicator")
	}
}

func TestAuditDedupRepeatedStartKeepsSignal(t *testing.T) {
	t.Parallel()
	d := NewDedup(2, time.Hour)
	d.StartCleanup()
	original := d.done
	d.StartCleanup()
	sameSignal := original == d.done
	d.Close()
	if !sameSignal {
		// A baseline loop may already have captured the old signal.
		close(original)
	}
	require.True(t, sameSignal, "repeated StartCleanup replaced the shutdown signal")
}

func TestAuditDedupConcurrentLifecycle(t *testing.T) {
	t.Parallel()
	d := NewDedup(2, time.Hour)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d.StartCleanup()
		}()
	}
	close(start)
	wg.Wait()
	d.Close()
}
