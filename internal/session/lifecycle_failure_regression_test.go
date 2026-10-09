package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/pkg/events"
)

type refusedLifecycleRetirementStore struct {
	*SQLiteStore
	err   error
	calls int
}

func (s *refusedLifecycleRetirementStore) RetireExpiredLifecycleSession(context.Context, string, time.Time) (*SessionInfo, error) {
	s.calls++
	return nil, s.err
}

func TestManager_UnsuccessfulLifecycleRetirementPreservesSessionAvailability(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		for _, fails := range []bool{false, true} {
			name := "cold_refused"
			if cached {
				name = "cached_refused"
			}
			if fails {
				name += "_store_error"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				store, cfg := helperDB(t)
				cfg.Session.GCScanInterval = time.Hour
				manager, err := NewManager(t.Context(), nil, cfg, nil, store)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, manager.Close()) })
				now := time.Now()
				if cached {
					_, err = manager.Create(t.Context(), name, "user-1", "claude_code", nil, "", "")
					require.NoError(t, err)
				} else {
					require.NoError(t, store.Upsert(t.Context(), expiredLifecycleSession(name, now)))
				}
				before, err := store.Get(t.Context(), name)
				require.NoError(t, err)
				retirement := &refusedLifecycleRetirementStore{SQLiteStore: store}
				if fails {
					retirement.err = errors.New("retirement transaction unavailable")
				}

				manager.retireExpiredLifecycleSession(t.Context(), retirement, name, now)

				require.Equal(t, 1, retirement.calls)
				manager.mu.RLock()
				ms, remainsCached := manager.sessions[name]
				manager.mu.RUnlock()
				require.Equal(t, cached, remainsCached, "a failed cold retirement must remove its temporary deletion fence")
				if cached {
					ms.mu.Lock()
					deleting := ms.deleting
					ms.mu.Unlock()
					require.False(t, deleting, "a failed cached retirement must release its deletion fence")
				}
				visible, err := manager.Get(t.Context(), name)
				require.NoError(t, err, "retirement refusal must leave the session usable")
				require.Equal(t, before.State, visible.State)
				after, err := store.Get(t.Context(), name)
				require.NoError(t, err)
				require.Equal(t, before.HistoryExpiresAt, after.HistoryExpiresAt)
				require.Nil(t, after.DeletedAt)
				pending, err := store.HasPendingCleanup(t.Context(), name)
				require.NoError(t, err)
				require.False(t, pending, "a refused retirement must not queue remote deletion")
			})
		}
	}
}

type failedLifecycleRenewalStore struct {
	*SQLiteStore
	getErr     error
	advanceErr error
}

func (s *failedLifecycleRenewalStore) Get(ctx context.Context, id string) (*SessionInfo, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.SQLiteStore.Get(ctx, id)
}

func (s *failedLifecycleRenewalStore) AdvanceLifecycleDeadlines(
	ctx context.Context, id, revision string,
	lastInputAt, archiveAt, conversationExpiresAt, historyExpiresAt, updatedAt time.Time,
) error {
	if s.advanceErr != nil {
		return s.advanceErr
	}
	return s.SQLiteStore.AdvanceLifecycleDeadlines(
		ctx, id, revision, lastInputAt, archiveAt, conversationExpiresAt, historyExpiresAt, updatedAt,
	)
}

func TestManager_InputRenewalFailurePreservesDurableDeadlines(t *testing.T) {
	t.Parallel()
	errUnavailable := errors.New("lifecycle persistence unavailable")
	for _, name := range []string{"refresh_error", "write_error", "missing_capability", "deleted_durable"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base, cfg := helperDB(t)
			cfg.Session.GCScanInterval = time.Hour
			var store Store = base
			switch name {
			case "refresh_error":
				store = &failedLifecycleRenewalStore{SQLiteStore: base, getErr: errUnavailable}
			case "write_error":
				store = &failedLifecycleRenewalStore{SQLiteStore: base, advanceErr: errUnavailable}
			case "missing_capability":
				store = struct{ Store }{Store: base}
			}
			manager, err := NewManager(t.Context(), nil, cfg, nil, store)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.Close()) })
			created, err := manager.Create(t.Context(), name, "user-1", "claude_code", nil, "", "")
			require.NoError(t, err)
			before, err := base.Get(t.Context(), name)
			require.NoError(t, err)
			if name == "deleted_durable" {
				require.NoError(t, base.MarkDeletedWithCleanup(t.Context(), before))
			}

			err = manager.RecordInputAccepted(t.Context(), name, created.CreatedAt.Add(24*time.Hour))

			require.Error(t, err, "an unpersisted renewal must not be acknowledged as successful")
			switch name {
			case "refresh_error", "write_error":
				require.ErrorIs(t, err, errUnavailable)
			case "missing_capability":
				require.ErrorContains(t, err, "does not support lifecycle deadline updates")
			case "deleted_durable":
				require.ErrorIs(t, err, ErrSessionNotFound)
			}
			after, getErr := base.Get(t.Context(), name)
			require.NoError(t, getErr)
			require.Equal(t, before.LastInputAt, after.LastInputAt)
			require.Equal(t, before.ConversationExpiresAt, after.ConversationExpiresAt)
			require.Equal(t, before.HistoryExpiresAt, after.HistoryExpiresAt)
			if name == "deleted_durable" {
				require.Equal(t, events.StateDeleted, after.State)
				require.NotNil(t, after.DeletedAt, "renewal must not revive a deleted durable session")
			}
		})
	}
}

func TestManager_LegacyInputAcceptanceDoesNotAssignV2Deadlines(t *testing.T) {
	t.Parallel()
	store, cfg := helperDB(t)
	cfg.Session.GCScanInterval = time.Hour
	cfg.Lifecycle.Policy = config.LifecyclePolicyLegacy
	manager, err := NewManager(t.Context(), nil, cfg, nil, store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	created, err := manager.Create(t.Context(), "legacy-renewal", "user-1", "claude_code", nil, "", "")
	require.NoError(t, err)

	require.NoError(t, manager.RecordInputAccepted(t.Context(), created.ID, created.CreatedAt.Add(24*time.Hour)))

	stored, err := store.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, config.LifecyclePolicyLegacy, stored.LifecyclePolicy)
	require.Nil(t, stored.LastInputAt)
	require.Nil(t, stored.ConversationExpiresAt)
	require.Nil(t, stored.HistoryExpiresAt)
}
