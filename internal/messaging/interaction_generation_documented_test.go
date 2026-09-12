package messaging

import (
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestD10OldTimeoutCannotResolveReplacement(t *testing.T) {
	t.Parallel()
	for _, claimed := range []bool{false, true} {
		name := "complete"
		if claimed {
			name = "complete_claimed"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewInteractionManager(slog.Default())
				denied := make(chan struct{}, 2)
				old := &PendingInteraction{ID: "reused", SessionID: "old", Type: events.PermissionRequest, Timeout: time.Second, SendResponse: func(map[string]any) { denied <- struct{}{} }}
				m.Register(old)
				synctest.Wait()
				if claimed {
					_, ok := m.Claim(old.ID)
					require.True(t, ok)
					_, ok = m.CompleteClaimed(old.ID)
					require.True(t, ok)
				} else {
					_, ok := m.Complete(old.ID)
					require.True(t, ok)
				}
				fresh := &PendingInteraction{ID: "reused", SessionID: "new", Type: events.QuestionRequest, Timeout: time.Hour, SendResponse: func(map[string]any) { denied <- struct{}{} }}
				m.Register(fresh)
				defer m.CancelAll("new")
				time.Sleep(2 * time.Second) // synctest virtual clock, not polling
				synctest.Wait()
				got, ok := m.Get("reused")
				require.True(t, ok, "old TTL retired a newer request")
				require.Same(t, fresh, got)
				require.Empty(t, denied)
			})
		})
	}
}
func TestD10OrdinaryTimeoutStillResolvesOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := NewInteractionManager(slog.Default())
		called := make(chan map[string]any, 2)
		m.Register(&PendingInteraction{ID: "live", Type: events.PermissionRequest, Timeout: time.Second, SendResponse: func(v map[string]any) { called <- v }})
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.Zero(t, m.Len())
		require.Len(t, called, 1)
	})
}
