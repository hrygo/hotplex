package messaging

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit1002OldTimeoutCannotDenyReplacement(t *testing.T) {
	t.Parallel()
	for _, finish := range []string{"complete", "claimed", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m := NewInteractionManager(slog.Default())
				var sent atomic.Int32
				old := &PendingInteraction{ID: "same", SessionID: "old-session", Type: events.PermissionRequest, Timeout: time.Second, SendResponse: func(map[string]any) { sent.Add(1) }}
				m.Register(old)
				synctest.Wait()
				oldCancel := old.cancelCh
				switch finish {
				case "complete":
					_, ok := m.Complete(old.ID)
					require.True(t, ok)
				case "claimed":
					_, ok := m.Claim(old.ID)
					require.True(t, ok)
					_, ok = m.CompleteClaimed(old.ID)
					require.True(t, ok)
				case "cancel":
					m.CancelAll(old.SessionID)
				}
				current := &PendingInteraction{ID: "same", SessionID: "new-session", Type: events.QuestionRequest, Timeout: time.Hour, SendResponse: func(map[string]any) { sent.Add(1) }}
				m.Register(current)
				synctest.Wait()
				<-time.After(2 * time.Second) // virtual clock, not a scheduling sleep
				synctest.Wait()
				got, pending := m.Get(current.ID)
				m.CancelAll(current.SessionID)
				require.True(t, pending, "the old deadline consumed a newer registration")
				require.Same(t, current, got)
				require.Zero(t, sent.Load())
				select {
				case <-oldCancel:
				default:
					t.Error("completion did not retire the old watcher")
				}
			})
		})
	}
}

func TestDeepAudit1002SameObjectCanBeRegisteredAfterCompletion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := NewInteractionManager(slog.Default())
		var sent atomic.Int32
		pi := &PendingInteraction{ID: "same", SessionID: "session", Type: events.PermissionRequest, Timeout: time.Second, SendResponse: func(map[string]any) { sent.Add(1) }}
		// Do not wait for the first watcher to start. Reusing the struct must
		// not redirect its old goroutine to the new registration's channel.
		m.Register(pi)
		_, ok := m.Claim(pi.ID)
		require.True(t, ok)
		_, ok = m.CompleteClaimed(pi.ID)
		require.True(t, ok)
		m.Register(pi)
		synctest.Wait()
		_, claimable := m.Claim(pi.ID)
		m.CancelAll(pi.SessionID)
		require.True(t, claimable, "resolving state leaked across registration lifetimes")
		require.Zero(t, sent.Load())
	})
}

func TestDeepAudit1002CurrentTimeoutStillDeniesOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := NewInteractionManager(slog.Default())
		var sent atomic.Int32
		pi := &PendingInteraction{ID: "current", SessionID: "session", Type: events.PermissionRequest, Timeout: time.Second, SendResponse: func(map[string]any) { sent.Add(1) }}
		m.Register(pi)
		synctest.Wait()
		<-time.After(2 * time.Second)
		synctest.Wait()
		require.Zero(t, m.Len())
		require.EqualValues(t, 1, sent.Load())
		m.CancelAll(pi.SessionID)
	})
}
