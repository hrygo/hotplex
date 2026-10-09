package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// ─── mockStore implements Store for testing ───────────────────────────────────

type mockStore struct {
	mock.Mock
}

func (m *mockStore) Upsert(ctx context.Context, info *SessionInfo) error {
	args := m.Called(ctx, info)
	if args.Error(0) == nil {
		// Copy fields back to info so callers see updated state
		if ms, ok := args.Get(0).(*SessionInfo); ok {
			*info = *ms
		}
	}
	return args.Error(0)
}

func (m *mockStore) UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error {
	args := m.Called(ctx, id, workerSessionID)
	return args.Error(0)
}

func (m *mockStore) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	args := m.Called(ctx, id, ceiling)
	return args.String(0), args.Error(1)
}

func (m *mockStore) UpdateSpecSnapshot(ctx context.Context, id string, snapshot *agentspec.EffectiveAgentSpecSnapshot) error {
	args := m.Called(ctx, id, snapshot)
	return args.Error(0)
}

func (m *mockStore) Get(ctx context.Context, id string) (*SessionInfo, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*SessionInfo), args.Error(1)
}

func (m *mockStore) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*SessionInfo, error) {
	args := m.Called(ctx, userID, platform, workspaceID, limit, offset)
	return args.Get(0).([]*SessionInfo), args.Error(1)
}

func (m *mockStore) GetExpiredMaxLifetime(ctx context.Context, now time.Time) ([]string, error) {
	args := m.Called(ctx, now)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) GetExpiredIdle(ctx context.Context, now time.Time) ([]string, error) {
	args := m.Called(ctx, now)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) DeleteTerminated(ctx context.Context, cronCutoff, defaultCutoff time.Time) ([]*SessionInfo, error) {
	args := m.Called(ctx, cronCutoff, defaultCutoff)
	if len(args) == 1 {
		return nil, args.Error(0)
	}
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*SessionInfo), args.Error(1)
}

func (m *mockStore) DeletePhysical(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *mockStore) GetSessionsByState(ctx context.Context, state events.SessionState) ([]string, error) {
	args := m.Called(ctx, state)
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockStore) Close() error {
	args := m.Called()
	return args.Error(0)
}

// ─── test helpers ──────────────────────────────────────────────────────────────

// ─── state transition tests ───────────────────────────────────────────────────

func TestStateTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    events.SessionState
		to      events.SessionState
		wantErr bool
	}{
		// CREATED transitions
		{"CREATED → RUNNING", events.StateCreated, events.StateRunning, false},
		{"CREATED → TERMINATED", events.StateCreated, events.StateTerminated, false},
		{"CREATED → IDLE invalid", events.StateCreated, events.StateIdle, true},
		{"CREATED → DELETED invalid", events.StateCreated, events.StateDeleted, true},

		// RUNNING transitions
		{"RUNNING → IDLE", events.StateRunning, events.StateIdle, false},
		{"RUNNING → TERMINATED", events.StateRunning, events.StateTerminated, false},
		{"RUNNING → DELETED", events.StateRunning, events.StateDeleted, false},
		{"RUNNING → CREATED invalid", events.StateRunning, events.StateCreated, true},

		// IDLE transitions
		{"IDLE → RUNNING", events.StateIdle, events.StateRunning, false},
		{"IDLE → TERMINATED", events.StateIdle, events.StateTerminated, false},
		{"IDLE → DELETED", events.StateIdle, events.StateDeleted, false},
		{"IDLE → CREATED invalid", events.StateIdle, events.StateCreated, true},

		// TERMINATED transitions
		{"TERMINATED → RUNNING (resume)", events.StateTerminated, events.StateRunning, false},
		{"TERMINATED → DELETED", events.StateTerminated, events.StateDeleted, false},
		{"TERMINATED → IDLE invalid", events.StateTerminated, events.StateIdle, true},
		{"TERMINATED → CREATED invalid", events.StateTerminated, events.StateCreated, true},

		// DELETED is terminal
		{"DELETED → RUNNING invalid", events.StateDeleted, events.StateRunning, true},
		{"DELETED → IDLE invalid", events.StateDeleted, events.StateIdle, true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok := events.IsValidTransition(tt.from, tt.to)
			if tt.wantErr {
				require.False(t, ok)
			} else {
				require.True(t, ok)
			}
		})
	}
}

func TestManager_Create(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).
		Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	info, err := m.Create(ctx, "sess_new", "user1", worker.TypeClaudeCode, nil, "", "test-session")
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "sess_new", info.ID)
	require.Equal(t, "user1", info.UserID)
	require.Equal(t, worker.TypeClaudeCode, info.WorkerType)
	require.Equal(t, events.StateCreated, info.State)
	require.NotNil(t, info.ExpiresAt)
	require.Equal(t, config.LifecyclePolicyV2, info.LifecyclePolicy)
	require.NotEmpty(t, info.LifecyclePolicyRevision)
	require.Nil(t, info.LastInputAt, "creating a session without input must not fabricate last_input_at")
	require.Equal(t, info.CreatedAt.Add(7*24*time.Hour), *info.ArchiveAt)
	require.Equal(t, info.CreatedAt.Add(180*24*time.Hour), *info.ConversationExpiresAt)
	require.Equal(t, info.ConversationExpiresAt, info.HistoryExpiresAt)
}

func TestManager_RecordInputAccepted_AdvancesAndPersistsLifecycleDeadlines(t *testing.T) {
	t.Parallel()

	store, cfg := helperDB(t)
	cfg.Session.GCScanInterval = time.Hour
	m, err := NewManager(t.Context(), nil, cfg, nil, store)
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	info, err := m.Create(t.Context(), "sess_input_lifecycle", "user1", worker.TypeClaudeCode, nil, "", "test-session")
	require.NoError(t, err)

	acceptedAt := info.CreatedAt.Add(24 * time.Hour)
	require.NoError(t, m.RecordInputAccepted(t.Context(), info.ID, acceptedAt))

	got, err := store.Get(t.Context(), info.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastInputAt)
	require.NotNil(t, got.ArchiveAt)
	require.NotNil(t, got.ConversationExpiresAt)
	require.NotNil(t, got.HistoryExpiresAt)
	require.True(t, got.LastInputAt.Equal(acceptedAt))
	require.True(t, got.ArchiveAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.ArchiveAfter)))
	require.True(t, got.ConversationExpiresAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.RetentionAfterLastInput)))
	require.True(t, got.HistoryExpiresAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.RetentionAfterLastInput)))
	require.Nil(t, got.LastContentExpiresAt, "session activity must not invent per-record content expiry")

	// Out-of-order acceptance must not shorten any persisted lifecycle deadline.
	require.NoError(t, m.RecordInputAccepted(t.Context(), info.ID, acceptedAt.Add(-48*time.Hour)))
	got, err = store.Get(t.Context(), info.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastInputAt)
	require.NotNil(t, got.ArchiveAt)
	require.NotNil(t, got.ConversationExpiresAt)
	require.NotNil(t, got.HistoryExpiresAt)
	require.True(t, got.LastInputAt.Equal(acceptedAt))
	require.True(t, got.ArchiveAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.ArchiveAfter)))
	require.True(t, got.ConversationExpiresAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.RetentionAfterLastInput)))
	require.True(t, got.HistoryExpiresAt.Equal(acceptedAt.Add(cfg.Lifecycle.Conversation.RetentionAfterLastInput)))
}

func TestManager_RecordInputAccepted_PreservesAssignedPolicyAndRenewsV2(t *testing.T) {
	t.Parallel()

	store, cfg := helperDB(t)
	cfg.Session.GCScanInterval = time.Hour
	m, err := NewManager(t.Context(), nil, cfg, nil, store)
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		id       string
		policy   string
		revision string
	}{
		{name: "legacy", id: "sess_legacy", policy: config.LifecyclePolicyLegacy},
		{name: "different revision", id: "sess_revision", policy: config.LifecyclePolicyV2, revision: "older-policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := &SessionInfo{
				ID:                      tc.id,
				UserID:                  "user1",
				WorkerType:              worker.TypeClaudeCode,
				State:                   events.StateCreated,
				CreatedAt:               now,
				UpdatedAt:               now,
				LifecyclePolicy:         tc.policy,
				LifecyclePolicyRevision: tc.revision,
				LastInputAt:             ptr(now),
				ArchiveAt:               ptr(now.Add(24 * time.Hour)),
				ConversationExpiresAt:   ptr(now.Add(48 * time.Hour)),
				HistoryExpiresAt:        ptr(now.Add(48 * time.Hour)),
			}
			require.NoError(t, store.Upsert(t.Context(), before))

			require.NoError(t, m.RecordInputAccepted(t.Context(), tc.id, now.Add(72*time.Hour)))

			after, err := store.Get(t.Context(), tc.id)
			require.NoError(t, err)
			require.Equal(t, before.LifecyclePolicyRevision, after.LifecyclePolicyRevision)
			if tc.policy == config.LifecyclePolicyV2 {
				acceptedAt := now.Add(72 * time.Hour)
				require.Equal(t, ptr(acceptedAt), after.LastInputAt)
				require.Equal(t, ptr(acceptedAt.Add(cfg.Lifecycle.Conversation.ArchiveAfter)), after.ArchiveAt)
				require.Equal(t, ptr(acceptedAt.Add(cfg.Lifecycle.Conversation.RetentionAfterLastInput)), after.ConversationExpiresAt)
				require.Equal(t, after.ConversationExpiresAt, after.HistoryExpiresAt)
				return
			}
			require.Equal(t, before.LastInputAt, after.LastInputAt)
			require.Equal(t, before.ArchiveAt, after.ArchiveAt)
			require.Equal(t, before.ConversationExpiresAt, after.ConversationExpiresAt)
			require.Equal(t, before.HistoryExpiresAt, after.HistoryExpiresAt)
		})
	}
}

func TestManager_Get(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Not in memory, falls back to store
	now := time.Now()
	expected := &SessionInfo{
		ID:         "sess_existing",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	store.On("Get", ctx, "sess_existing").Return(expected, nil)

	info, err := m.Get(context.Background(), "sess_existing")
	require.NoError(t, err)
	require.Equal(t, "sess_existing", info.ID)
	require.Equal(t, events.StateRunning, info.State)

	// After Get, session should be in memory map
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Get", ctx, "sess_existing").Return(expected, nil).Maybe()

	// In-memory hit
	info2, err := m.Get(context.Background(), "sess_existing")
	require.NoError(t, err)
	require.Equal(t, "sess_existing", info2.ID)
}

func TestManager_Get_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_missing").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	_, err = m.Get(context.Background(), "sess_missing")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_Transition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed a session in memory
	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_trans",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.mu.Lock()
	m.sessions["sess_trans"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.Transition(ctx, "sess_trans", events.StateRunning)
	require.NoError(t, err)

	info, _ := m.Get(context.Background(), "sess_trans")
	require.Equal(t, events.StateRunning, info.State)
}

func TestManager_Transition_Invalid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed a CREATED session
	seed := &SessionInfo{
		ID:         "sess_invalid",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_invalid"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Cannot go CREATED → IDLE directly
	err = m.Transition(ctx, "sess_invalid", events.StateIdle)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidTransition))
}

func TestManager_Transition_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	err = m.Transition(ctx, "sess_ghost", events.StateRunning)
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_TransitionWithInput_Atomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_atomic",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_atomic"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	// TransitionWithInput should succeed atomically
	err = m.TransitionWithInput(ctx, "sess_atomic", events.StateIdle, "user input", nil)
	require.NoError(t, err)
}

func TestManager_TransitionWithInput_InvalidTransition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_atomic_inv",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_atomic_inv"] = &managedSession{info: *seed}
	m.mu.Unlock()

	err = m.TransitionWithInput(ctx, "sess_atomic_inv", events.StateIdle, "input", nil)
	require.Error(t, err)
}

func TestSessionBusy_RejectWhenNotActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed a TERMINATED session
	seed := &SessionInfo{
		ID:         "sess_busy",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateTerminated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_busy"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Attempt TransitionWithInput on TERMINATED → IDLE is invalid (TERMINATED → IDLE not allowed)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.TransitionWithInput(ctx, "sess_busy", events.StateIdle, "input", nil)
	require.Error(t, err)
}

func TestManager_Delete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_del",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateTerminated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_del"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.Delete(ctx, "sess_del")
	require.NoError(t, err)

	// Session should be removed from in-memory map
	m.mu.RLock()
	_, ok := m.sessions["sess_del"]
	m.mu.RUnlock()
	require.False(t, ok)
}

func TestManager_DeleteTerminatesAttachedWorker(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	w := newMockWorker(worker.TypeOpenCodeSrv, 0)
	w.On("Terminate", mock.Anything).Return(nil).Once()
	seed := &SessionInfo{
		ID:         "sess_delete_worker",
		UserID:     "user1",
		WorkerType: worker.TypeOpenCodeSrv,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions[seed.ID] = &managedSession{info: *seed, worker: w}
	m.mu.Unlock()
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	require.NoError(t, m.Delete(ctx, seed.ID))
	w.AssertExpectations(t)
}

func TestManager_DeletePhysical(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	t.Run("removes session from memory and database", func(t *testing.T) {
		t.Parallel()

		seed := &SessionInfo{
			ID:         "sess_phys",
			UserID:     "user1",
			WorkerType: worker.TypeClaudeCode,
			State:      events.StateRunning,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		m.mu.Lock()
		m.sessions["sess_phys"] = &managedSession{info: *seed}
		m.mu.Unlock()

		store.On("DeletePhysical", ctx, "sess_phys").Return(nil)

		err := m.DeletePhysical(ctx, "sess_phys")
		require.NoError(t, err)

		m.mu.RLock()
		_, ok := m.sessions["sess_phys"]
		m.mu.RUnlock()
		require.False(t, ok)
	})

	t.Run("no-op when session not in memory", func(t *testing.T) {
		t.Parallel()

		store.On("DeletePhysical", ctx, "nonexistent").Return(nil)

		err := m.DeletePhysical(ctx, "nonexistent")
		require.NoError(t, err)
	})

	t.Run("returns database error", func(t *testing.T) {
		t.Parallel()

		store.On("DeletePhysical", ctx, "db_fail").Return(errors.New("db error"))

		err := m.DeletePhysical(ctx, "db_fail")
		require.Error(t, err)
	})
}

func TestManager_DeletePhysicalNotifiesRuntimeCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("DeletePhysical", ctx, "sess_cleanup_seq").Return(nil)
	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	var notified atomic.Bool
	m.OnRuntimeRelease = func(_ context.Context, id string) {
		require.Equal(t, "sess_cleanup_seq", id)
		notified.Store(true)
	}
	require.NoError(t, m.DeletePhysical(ctx, "sess_cleanup_seq"))
	require.True(t, notified.Load())
}

func TestManager_DeletePhysicalKeepsDeletingTombstone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	deleteStarted := make(chan struct{})
	allowDelete := make(chan struct{})
	store.On("DeletePhysical", ctx, "sess_delete_window").Run(func(mock.Arguments) {
		close(deleteStarted)
		<-allowDelete
	}).Return(nil)

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.DeletePhysical(ctx, "sess_delete_window") }()
	<-deleteStarted
	require.False(t, m.IsSeqActive(ctx, "sess_delete_window"))
	close(allowDelete)
	require.NoError(t, <-deleteDone)
}

func TestManager_DeleteNotifiesRuntimeCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := SessionInfo{
		ID: "sess_logical_delete", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	m.mu.Lock()
	m.sessions[seed.ID] = &managedSession{info: seed}
	m.mu.Unlock()

	var notified atomic.Bool
	var stateNotified atomic.Bool
	m.StateNotifier = func(_ context.Context, id string, state events.SessionState, _ string) {
		require.Equal(t, seed.ID, id)
		require.Equal(t, events.StateDeleted, state)
		stateNotified.Store(true)
	}
	m.OnRuntimeRelease = func(_ context.Context, id string) {
		require.Equal(t, seed.ID, id)
		require.True(t, stateNotified.Load(), "deleted state must be captured before runtime release")
		notified.Store(true)
	}
	require.NoError(t, m.Delete(ctx, seed.ID))
	require.True(t, notified.Load())
}

func TestManager_IsSeqActiveRejectsDeletingSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	m.mu.Lock()
	m.sessions["s1"] = &managedSession{
		info:     SessionInfo{ID: "s1", State: events.StateRunning},
		deleting: true,
	}
	m.mu.Unlock()

	require.False(t, m.IsSeqActive(ctx, "s1"))
}

func TestManager_DeleteNotifiesWorkerSessionCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:              "sess_cleanup",
		UserID:          "user1",
		WorkerType:      worker.TypeOpenCodeSrv,
		WorkerSessionID: "ocs-session-1",
		State:           events.StateTerminated,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	m.mu.Lock()
	m.sessions[seed.ID] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	cleaned := make(chan SessionInfo, 1)
	m.OnDelete = func(_ context.Context, info SessionInfo) { cleaned <- info }

	require.NoError(t, m.Delete(ctx, seed.ID))
	require.Eventually(t, func() bool { return len(cleaned) == 1 }, time.Second, 10*time.Millisecond)
	info := <-cleaned
	require.Equal(t, worker.TypeOpenCodeSrv, info.WorkerType)
	require.Equal(t, "ocs-session-1", info.WorkerSessionID)
}

func TestManager_DeleteNotInMemoryNotifiesWorkerSessionCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Get", ctx, "sess_stored_cleanup").Return(&SessionInfo{
		ID:              "sess_stored_cleanup",
		WorkerType:      worker.TypeOpenCodeSrv,
		WorkerSessionID: "ocs-stored",
		State:           events.StateTerminated,
		CreatedAt:       time.Now().Add(-time.Hour),
		UpdatedAt:       time.Now().Add(-time.Hour),
	}, nil).Once()
	store.On("Upsert", ctx, mock.MatchedBy(func(info *SessionInfo) bool {
		return info.ID == "sess_stored_cleanup" &&
			info.State == events.StateDeleted &&
			info.DeletedAt != nil
	})).Return(nil).Once()

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()
	cleaned := make(chan SessionInfo, 1)
	m.OnDelete = func(_ context.Context, info SessionInfo) { cleaned <- info }

	require.NoError(t, m.Delete(ctx, "sess_stored_cleanup"))
	require.Eventually(t, func() bool { return len(cleaned) == 1 }, time.Second, 10*time.Millisecond)
	info := <-cleaned
	require.Equal(t, "ocs-stored", info.WorkerSessionID)
}

func TestManager_GC_NotifiesWorkerSessionCleanupForRetentionDeletion(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).
		Return([]*SessionInfo{{ID: "sess_expired", WorkerType: worker.TypeOpenCodeSrv, WorkerSessionID: "ocs-expired"}}, nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()
	cleaned := make(chan SessionInfo, 1)
	m.OnDelete = func(_ context.Context, info SessionInfo) { cleaned <- info }

	m.gc(ctx)
	require.Eventually(t, func() bool { return len(cleaned) == 1 }, time.Second, 10*time.Millisecond)
	info := <-cleaned
	require.Equal(t, "ocs-expired", info.WorkerSessionID)
}

func TestManager_ValidateOwnership(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_owner",
		UserID:     "user_owner",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_owner"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Owner matches
	err = m.ValidateOwnership(ctx, "sess_owner", "user_owner", "")
	require.NoError(t, err)

	// Owner mismatch
	err = m.ValidateOwnership(ctx, "sess_owner", "wrong_user", "")
	require.True(t, errors.Is(err, ErrOwnershipMismatch))

	// Admin bypass
	err = m.ValidateOwnership(ctx, "sess_owner", "wrong_user", "admin_user")
	require.NoError(t, err)
}

func TestManager_ValidateOwnership_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_missing").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	err = m.ValidateOwnership(ctx, "sess_missing", "user1", "")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_Lock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_lock",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_lock"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Lock and immediately unlock
	unlock, err := m.Lock("sess_lock")
	require.NoError(t, err)
	require.NotNil(t, unlock)
	unlock()
}

func TestManager_Lock_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost_lock").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	_, err = m.Lock("sess_ghost_lock")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_List(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	expected := []*SessionInfo{
		{ID: "sess_1", UserID: "user1", WorkerType: worker.TypeClaudeCode, State: events.StateRunning},
		{ID: "sess_2", UserID: "user2", WorkerType: worker.TypeClaudeCode, State: events.StateIdle},
	}
	store.On("List", ctx, "", "", "", 50, 0).Return(expected, nil)

	list, err := m.List(ctx, "", "", "", 50, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestManager_ListActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed sessions
	for _, id := range []string{"sess_a", "sess_b"} {
		m.mu.Lock()
		m.sessions[id] = &managedSession{info: SessionInfo{
			ID:         id,
			UserID:     "user1",
			WorkerType: worker.TypeClaudeCode,
			State:      events.StateRunning,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}}
		m.mu.Unlock()
	}

	active := m.ListActive()
	require.Len(t, active, 2)
}

func TestManager_Stats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	total, max, users := m.Stats()
	require.Equal(t, 0, total)
	require.Equal(t, cfg.Pool.MaxSize, max)
	require.Equal(t, 0, users)
}

func TestSessionInfo_IsActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state  events.SessionState
		active bool
	}{
		{events.StateCreated, true},
		{events.StateRunning, true},
		{events.StateIdle, true},
		{events.StateTerminated, false},
		{events.StateDeleted, false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.active, tt.state.IsActive())
		})
	}
}

func TestSessionInfo_IsTerminal(t *testing.T) {
	t.Parallel()

	require.True(t, events.StateDeleted.IsTerminal())
	require.False(t, events.StateTerminated.IsTerminal())
	require.False(t, events.StateRunning.IsTerminal())
}

// ─── mockWorker implements worker.Worker for testing ──────────────────────────

type mockWorker struct {
	mock.Mock
	workerType  worker.WorkerType
	maxTurns    int
	lastIO      time.Time
	health      worker.WorkerHealth
	sessionConn *mockSessionConn
}

type mockSessionConn struct {
	mock.Mock
}

func (m *mockSessionConn) Send(ctx context.Context, msg *events.Envelope) error {
	args := m.Called(ctx, msg)
	return args.Error(0)
}

func (m *mockSessionConn) Recv() <-chan *events.Envelope {
	args := m.Called()
	if args.Get(0) == nil {
		ch := make(chan *events.Envelope)
		close(ch)
		return ch
	}
	return args.Get(0).(<-chan *events.Envelope)
}

func (m *mockSessionConn) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockSessionConn) UserID() string    { return "user1" }
func (m *mockSessionConn) SessionID() string { return "mock_sess" }

func newMockWorker(t worker.WorkerType, maxTurns int) *mockWorker {
	return &mockWorker{
		workerType: t,
		maxTurns:   maxTurns,
		health: worker.WorkerHealth{
			Type:      t,
			SessionID: "mock_sess",
			PID:       12345,
			Running:   true,
			Healthy:   true,
			Uptime:    "1m0s",
		},
		sessionConn: &mockSessionConn{},
	}
}

func (w *mockWorker) Type() worker.WorkerType   { return w.workerType }
func (w *mockWorker) SupportsResume() bool      { return false }
func (w *mockWorker) CanResumeTerminated() bool { return false }
func (w *mockWorker) SupportsStreaming() bool   { return true }
func (w *mockWorker) SupportsTools() bool       { return true }
func (w *mockWorker) EnvBlocklist() []string    { return nil }
func (w *mockWorker) SessionStoreDir() string   { return "" }
func (w *mockWorker) MaxTurns() int             { return w.maxTurns }
func (w *mockWorker) Modalities() []string      { return []string{"text", "code"} }
func (w *mockWorker) Start(ctx context.Context, session worker.SessionInfo) error {
	args := w.Called(ctx, session)
	return args.Error(0)
}
func (w *mockWorker) Input(ctx context.Context, content string, metadata map[string]any) error {
	args := w.Called(ctx, content, metadata)
	return args.Error(0)
}
func (w *mockWorker) Resume(ctx context.Context, session worker.SessionInfo) error {
	args := w.Called(ctx, session)
	return args.Error(0)
}
func (w *mockWorker) Terminate(ctx context.Context) error {
	args := w.Called(ctx)
	return args.Error(0)
}
func (w *mockWorker) Kill() error {
	return nil
}
func (w *mockWorker) Wait() (int, error) {
	return 0, nil
}
func (w *mockWorker) Conn() worker.SessionConn { return w.sessionConn }
func (w *mockWorker) Health() worker.WorkerHealth {
	return w.health
}
func (w *mockWorker) LastIO() time.Time { return w.lastIO }
func (w *mockWorker) ResetContext(ctx context.Context) (worker.ResetResult, error) {
	args := w.Called(ctx)
	return args.Get(0).(worker.ResetResult), args.Error(1)
}
func (w *mockWorker) StopCurrentTurn(_ context.Context) error {
	return nil
}
func (w *mockWorker) IsStopped() bool {
	return false
}

// ─── AttachWorker tests ───────────────────────────────────────────────────────

func TestManager_AttachWorker_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_attach",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_attach"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_attach", w)
	require.NoError(t, err)

	// Verify pool slot acquired
	total, _, users := m.Stats()
	require.Equal(t, 1, total)
	require.Equal(t, 1, users)
}

func TestManager_AttachWorker_PoolExhausted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	// Global pool size = 1
	cfg.Pool.MaxSize = 1
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_exhaust",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_exhaust"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)

	// First session exhausts the global pool
	err = m.AttachWorker(context.Background(), "sess_exhaust", w)
	require.NoError(t, err)

	// Second session (different user) fails due to global limit
	seed2 := &SessionInfo{
		ID:         "sess_exhaust2",
		UserID:     "user2",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_exhaust2"] = &managedSession{info: *seed2}
	m.mu.Unlock()
	w2 := newMockWorker(worker.TypeClaudeCode, 0)
	w2.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_exhaust2", w2)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPoolExhausted))
}

func TestManager_AttachWorker_UserQuotaExceeded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	// Per-user limit = 1
	cfg.Pool.MaxIdlePerUser = 1
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_quota",
		UserID:     "user_quota",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_quota"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_quota", w)
	require.NoError(t, err)

	// Second session for same user → quota exceeded
	seed2 := &SessionInfo{
		ID:         "sess_quota2",
		UserID:     "user_quota",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_quota2"] = &managedSession{info: *seed2}
	m.mu.Unlock()
	w2 := newMockWorker(worker.TypeClaudeCode, 0)
	w2.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_quota2", w2)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUserQuotaExceeded))
}

func TestManager_AttachWorker_MemoryExceeded_Rollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	// 1 GB per user, 512 MB per worker → max 2
	cfg.Pool.MaxMemoryPerUser = 512 * 1024 * 1024
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_mem",
		UserID:     "user_mem",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_mem"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_mem", w)
	require.NoError(t, err)

	// Detach first worker, then reattach succeeds (memory freed)
	m.DetachWorker("sess_mem")

	// After detach, pool is clean
	total, _, _ := m.Stats()
	require.Equal(t, 0, total)

	// Second worker on same session after detach — succeeds since memory is freed
	w2 := newMockWorker(worker.TypeClaudeCode, 0)
	w2.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_mem", w2)
	require.NoError(t, err)
}

func TestManager_AttachWorker_AlreadyAttached(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_double",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_double"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_double", w)
	require.NoError(t, err)

	// Second attach on same session → ErrWorkerAttached (no quota acquired)
	w2 := newMockWorker(worker.TypeClaudeCode, 0)
	w2.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_double", w2)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrWorkerAttached))

	// Pool quota not leaked
	total, _, _ := m.Stats()
	require.Equal(t, 1, total)
}

func TestManager_AttachWorker_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_missing").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_missing", w)
	require.Error(t, err)
}

// TestManager_AttachWorker_SetWorkspaceID_Race: concurrent SetWorkspaceID (writes
// ms.info.WorkspaceID) and AttachWorker (reads ms.info.WorkspaceID) must not
// trigger the race detector. Review P1: AttachWorker must read ms.info fields
// under ms.mu — matching Get's lock discipline — instead of under m.mu.RLock
// alone. Run with -race.
func TestManager_AttachWorker_SetWorkspaceID_Race(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil).Maybe()
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	m.mu.Lock()
	m.sessions["sess_race"] = &managedSession{info: SessionInfo{
		ID:          "sess_race",
		UserID:      "u1",
		WorkerType:  worker.TypeClaudeCode,
		State:       events.StateCreated,
		WorkspaceID: "wsA",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)

	// Hammer AttachWorker and SetWorkspaceID concurrently. Before the P1 fix,
	// AttachWorker read ms.info.WorkspaceID under m.mu.RLock only (not ms.mu),
	// racing the SetWorkspaceID write under ms.mu — -race reports it.
	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = m.AttachWorker(ctx, "sess_race", w)
		}()
		go func(i int) {
			defer wg.Done()
			workspaceID := "wsA"
			if i%2 == 1 {
				workspaceID = "wsB"
			}
			_ = m.SetWorkspaceID(ctx, "sess_race", workspaceID)
		}(i)
	}
	wg.Wait()
}

func TestManager_SetWorkspaceID_RollsBackIdentityOnPersistFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).
		Return(errors.New("persist failed")).Once()

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := SessionInfo{
		ID:         "sess-workspace-rollback",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateCreated,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	bindIdentity(&seed)
	originalIdentity := *seed.Identity
	m.mu.Lock()
	m.sessions[seed.ID] = &managedSession{info: seed}
	m.mu.Unlock()

	err = m.SetWorkspaceID(ctx, seed.ID, "ws-new")
	require.Error(t, err)

	got, getErr := m.Get(ctx, seed.ID)
	require.NoError(t, getErr)
	require.Empty(t, got.WorkspaceID)
	require.NotNil(t, got.Identity)
	require.Equal(t, originalIdentity, *got.Identity,
		"failed workspace persistence must roll back the derived identity as well")
}

// TestManager_AttachWorker_QuotaNoDriftAfterWorkspaceChange: when SetWorkspaceID
// flips ms.info.WorkspaceID between Acquire and Detach, the per-workspace quota
// slot acquired at attach time must be released against the SAME workspace it
// was acquired on (review P2 — quota drift). DetachWorker must use the
// attach-time workspaceID, not the live ms.info.WorkspaceID, or wsA leaks and a
// fresh wsA attach is rejected.
func TestManager_AttachWorker_QuotaNoDriftAfterWorkspaceChange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	cfg.Pool.MaxPerWorkspace = 1 // one slot per workspace
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil).Maybe()
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := func(id, workspaceID string) {
		m.mu.Lock()
		m.sessions[id] = &managedSession{info: SessionInfo{
			ID:          id,
			UserID:      "u1",
			WorkerType:  worker.TypeClaudeCode,
			State:       events.StateCreated,
			WorkspaceID: workspaceID,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}}
		m.mu.Unlock()
	}

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)

	// s1 attaches on wsA, consuming wsA's only per-workspace slot.
	seed("s1", "wsA")
	require.NoError(t, m.AttachWorker(ctx, "s1", w))

	// Flip s1's workspace to wsB AFTER attach. A buggy detach that reads live
	// ms.info.WorkspaceID would release wsB (never acquired) and strand wsA.
	require.NoError(t, m.SetWorkspaceID(ctx, "s1", "wsB"))
	m.DetachWorker("s1")

	// wsA's slot must have been released, so a fresh wsA session can attach.
	seed("s2", "wsA")
	require.NoError(t, m.AttachWorker(ctx, "s2", w),
		"wsA quota leaked: DetachWorker released the wrong workspace (P2 drift)")
}

// ─── DetachWorker tests ───────────────────────────────────────────────────────

func TestManager_DetachWorker_WithWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_detach",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_detach"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_detach", w)
	require.NoError(t, err)

	m.DetachWorker("sess_detach")

	// Pool slot released
	total, _, _ := m.Stats()
	require.Equal(t, 0, total)
	// No worker attached
	require.Nil(t, m.GetWorker("sess_detach"))
}

func TestManager_DetachWorker_NoWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_no_worker",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateIdle,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_no_worker"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Detaching with no worker attached should be safe (no panic)
	m.DetachWorker("sess_no_worker")

	total, _, _ := m.Stats()
	require.Equal(t, 0, total)
}

func TestManager_DetachWorker_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost_detach").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Should be safe
	m.DetachWorker("sess_ghost_detach")
}

// ─── GetWorker tests ──────────────────────────────────────────────────────────

func TestManager_GetWorker_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost_worker").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	require.Nil(t, m.GetWorker("sess_ghost_worker"))
}

// ─── DebugSnapshot tests ──────────────────────────────────────────────────────

func TestManager_DebugSnapshot_WithWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_debug",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	ms := &managedSession{info: *seed, TurnCount: 5}
	m.sessions["sess_debug"] = ms
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	_ = m.AttachWorker(context.Background(), "sess_debug", w)

	snap, ok := m.DebugSnapshot("sess_debug")
	require.True(t, ok)
	require.Equal(t, 5, snap.TurnCount)
	require.True(t, snap.HasWorker)
}

func TestManager_DebugSnapshot_NoWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_no_worker_debug",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateIdle,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_no_worker_debug"] = &managedSession{info: *seed, TurnCount: 3}
	m.mu.Unlock()

	snap, ok := m.DebugSnapshot("sess_no_worker_debug")
	require.True(t, ok)
	require.Equal(t, 3, snap.TurnCount)
	require.False(t, snap.HasWorker)
}

func TestManager_DebugSnapshot_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost_debug").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	_, ok := m.DebugSnapshot("sess_ghost_debug")
	require.False(t, ok)
}

// ─── releaseWorkerQuota tests ─────────────────────────────────────────────────

func TestManager_ReleaseWorkerQuota(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_quota_rel",
		UserID:     "user_quota_rel",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	ms := &managedSession{info: *seed}
	m.sessions["sess_quota_rel"] = ms
	m.mu.Unlock()

	// Attach and release
	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	_ = m.AttachWorker(context.Background(), "sess_quota_rel", w)
	total, _, _ := m.Stats()
	require.Equal(t, 1, total)

	m.releaseWorkerQuota(context.Background(), ms)
	total, _, _ = m.Stats()
	require.Equal(t, 0, total)
}

func TestManager_TransitionTerminated_NilsWorker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed a RUNNING session with a mock worker.
	seed := &SessionInfo{
		ID:         "sess_worker_nil",
		UserID:     "user_worker_nil",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	ms := &managedSession{info: *seed}
	m.mu.Lock()
	m.sessions["sess_worker_nil"] = ms
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	_ = m.AttachWorker(context.Background(), "sess_worker_nil", w)

	total, _, _ := m.Stats()
	require.Equal(t, 1, total)

	// Transition to TERMINATED — should nil the worker pointer.
	err = m.TransitionWithReason(ctx, "sess_worker_nil", events.StateTerminated, "zombie")
	require.NoError(t, err)

	// Worker pointer must be nil to prevent double release by DetachWorker.
	ms.mu.RLock()
	workerPtr := ms.worker
	ms.mu.RUnlock()
	require.Nil(t, workerPtr, "worker pointer should be nil after transition to TERMINATED")

	// DetachWorker should be a no-op (no pool underflow).
	m.DetachWorker("sess_worker_nil")
	total, _, _ = m.Stats()
	require.Equal(t, 0, total, "pool should be at 0, not negative")
}

// ─── WorkerHealthStatuses tests ───────────────────────────────────────────────

func TestManager_WorkerHealthStatuses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_health",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_health"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	_ = m.AttachWorker(context.Background(), "sess_health", w)

	statuses := m.WorkerHealthStatuses()
	require.Len(t, statuses, 1)
	require.Equal(t, worker.TypeClaudeCode, statuses[0].Type)
}

func TestManager_WorkerHealthStatuses_NoWorkers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	statuses := m.WorkerHealthStatuses()
	require.Len(t, statuses, 0)
}

// ─── GC tests ─────────────────────────────────────────────────────────────────

func TestManager_GC_ZombieDetection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	// Simulate zombie: worker lastIO was 10 min ago (beyond timeout)
	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_zombie",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now.Add(-20 * time.Minute),
		UpdatedAt:  now.Add(-20 * time.Minute),
	}
	m.mu.Lock()
	ms := &managedSession{info: *seed}
	m.sessions["sess_zombie"] = ms
	m.mu.Unlock()
	m.addToRunningIndex("sess_zombie")

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	w.lastIO = now.Add(-31 * time.Minute) // zombie: no IO beyond 30m default execution_timeout
	_ = m.AttachWorker(context.Background(), "sess_zombie", w)

	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(nil)

	m.gc(ctx)

	// Session should be terminated
	m.mu.RLock()
	state := m.sessions["sess_zombie"].info.State
	m.mu.RUnlock()
	require.Equal(t, events.StateTerminated, state)
}

func TestManager_GC_NoZombieWhenRecentIO(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_healthy",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.mu.Lock()
	m.sessions["sess_healthy"] = &managedSession{info: *seed}
	m.mu.Unlock()
	m.addToRunningIndex("sess_healthy")

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.lastIO = now // very recent IO
	_ = m.AttachWorker(context.Background(), "sess_healthy", w)

	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(nil)

	m.gc(ctx)

	// Session still RUNNING
	m.mu.RLock()
	state := m.sessions["sess_healthy"].info.State
	m.mu.RUnlock()
	require.Equal(t, events.StateRunning, state)
}

func TestManager_GC_ExpiredMaxLifetime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	now := time.Now()
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string{"sess_maxlife"}, nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).
		Return(nil)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	seed := &SessionInfo{
		ID:         "sess_maxlife",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateIdle,
		CreatedAt:  now.Add(-48 * time.Hour),
		UpdatedAt:  now.Add(-48 * time.Hour),
	}
	m.mu.Lock()
	m.sessions["sess_maxlife"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	m.gc(ctx)

	m.mu.RLock()
	state := m.sessions["sess_maxlife"].info.State
	m.mu.RUnlock()
	require.Equal(t, events.StateTerminated, state)
}

func TestManager_GC_ExpiredIdleTimeout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	now := time.Now()
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string{"sess_idle_exp"}, nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).
		Return(nil)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	seed := &SessionInfo{
		ID:         "sess_idle_exp",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateIdle,
		CreatedAt:  now,
		UpdatedAt:  now.Add(-35 * time.Minute),
	}
	m.mu.Lock()
	m.sessions["sess_idle_exp"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	m.gc(ctx)

	m.mu.RLock()
	state := m.sessions["sess_idle_exp"].info.State
	m.mu.RUnlock()
	require.Equal(t, events.StateTerminated, state)
}

func TestManager_GC_NoRetentionCleanup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	m.gc(ctx)
}

func TestManager_GC_TerminatedSessionPreserved(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	// Seed a TERMINATED session within TTL (recent UpdatedAt) — should survive GC eviction.
	now := time.Now()
	ms := &managedSession{
		info: SessionInfo{
			ID:         "sess_retention_preserved",
			UserID:     "user1",
			WorkerType: worker.TypeClaudeCode,
			State:      events.StateTerminated,
			CreatedAt:  now.Add(-cfg.Session.RetentionPeriod - time.Hour),
			UpdatedAt:  now, // within 24h TTL — will not be evicted
		},
	}
	m.mu.Lock()
	m.sessions["sess_retention_preserved"] = ms
	m.mu.Unlock()

	// Before GC: session exists in memory.
	_, ok := m.sessions["sess_retention_preserved"]
	require.True(t, ok, "session should exist in memory before GC")

	m.gc(ctx)

	// After GC: session STILL in memory because it's within TTL.
	// TERMINATED records are "resume decision flags" and are only evicted after 24h.
	m.mu.RLock()
	_, ok = m.sessions["sess_retention_preserved"]
	m.mu.RUnlock()
	require.True(t, ok, "TERMINATED session should remain in memory after GC (within TTL)")
}

func TestManager_GC_TerminatedSession_DBError_NoImpact(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	// DeleteTerminated error is logged but does not propagate.
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), errors.New("db error"))
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), errors.New("db error"))
	store.On("Close").Return(nil)

	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(errors.New("db error"))
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	// Seed a TERMINATED session within TTL.
	now := time.Now()
	ms := &managedSession{
		info: SessionInfo{
			ID:         "sess_retention_noop",
			UserID:     "user1",
			WorkerType: worker.TypeClaudeCode,
			State:      events.StateTerminated,
			CreatedAt:  now.Add(-cfg.Session.RetentionPeriod - time.Hour),
			UpdatedAt:  now, // within 24h TTL
		},
	}
	m.mu.Lock()
	m.sessions["sess_retention_noop"] = ms
	m.mu.Unlock()

	// gc should not panic and should not touch TERMINATED sessions.
	m.gc(ctx)

	m.mu.RLock()
	_, ok := m.sessions["sess_retention_noop"]
	m.mu.RUnlock()
	require.True(t, ok, "TERMINATED session should remain after GC even with store errors")
}

func TestManager_GC_NoPanicOnStoreErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), errors.New("db error"))
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).
		Return([]string(nil), errors.New("db error"))
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(errors.New("db error"))
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	// gc should not panic even on store errors
	m.gc(ctx)
}

// ─── ClearContext tests ──────────────────────────────────────────────────────

func TestManager_ClearContext_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_clear",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
		Context:    map[string]any{"key1": "value1", "key2": 42},
	}
	m.mu.Lock()
	m.sessions["sess_clear"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.ClearContext(ctx, "sess_clear")
	require.NoError(t, err)

	// Verify Context is now empty in memory
	info, _ := m.Get(context.Background(), "sess_clear")
	require.NotNil(t, info)
	require.Empty(t, info.Context)
}

func TestManager_ClearContext_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_missing_clear").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	err = m.ClearContext(ctx, "sess_missing_clear")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_ClearContext_NilManager(t *testing.T) {
	t.Parallel()

	m := (*Manager)(nil)
	err := m.ClearContext(context.Background(), "any")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_ClearContext_UpdatesTimestamp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_clear_ts",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now.Add(-1 * time.Hour),
		UpdatedAt:  now.Add(-1 * time.Hour),
		Context:    map[string]any{"old": "data"},
	}
	m.mu.Lock()
	m.sessions["sess_clear_ts"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.ClearContext(ctx, "sess_clear_ts")
	require.NoError(t, err)

	// Verify UpdatedAt was updated by checking in-memory state
	m.mu.RLock()
	updatedMs := m.sessions["sess_clear_ts"]
	m.mu.RUnlock()
	require.NotNil(t, updatedMs)
	// UpdatedAt should be after the original time
	require.True(t, updatedMs.info.UpdatedAt.After(now.Add(-5*time.Second)))
}

// ─── RepairRunningSessions tests ──────────────────────────────────────────────

func TestManager_RepairRunningSessions_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetSessionsByState", ctx, events.StateRunning).
		Return([]string{"sess_r1", "sess_r2", "sess_r3"}, nil)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed sessions in memory in RUNNING state.
	for _, id := range []string{"sess_r1", "sess_r2", "sess_r3"} {
		m.mu.Lock()
		m.sessions[id] = &managedSession{info: SessionInfo{
			ID:         id,
			UserID:     "user1",
			WorkerType: worker.TypeClaudeCode,
			State:      events.StateRunning,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}}
		m.mu.Unlock()
	}

	repaired, err := m.RepairRunningSessions(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, repaired)

	// All sessions should now be TERMINATED.
	for _, id := range []string{"sess_r1", "sess_r2", "sess_r3"} {
		info, _ := m.Get(context.Background(), id)
		require.Equal(t, events.StateTerminated, info.State)
	}
}

func TestManager_RepairRunningSessions_Empty(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetSessionsByState", ctx, events.StateRunning).
		Return([]string(nil), nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	repaired, err := m.RepairRunningSessions(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, repaired)
}

func TestManager_RepairRunningSessions_StoreError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetSessionsByState", ctx, events.StateRunning).
		Return([]string(nil), errors.New("db error"))
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	_, err = m.RepairRunningSessions(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "repair running sessions")
}

func TestManager_RepairRunningSessions_PartialFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("GetSessionsByState", ctx, events.StateRunning).
		Return([]string{"sess_ok", "sess_fail"}, nil)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// sess_ok is in memory and can transition.
	m.mu.Lock()
	m.sessions["sess_ok"] = &managedSession{info: SessionInfo{
		ID:         "sess_ok",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}}
	// sess_fail is DELETED (terminal) — transition will fail.
	m.sessions["sess_fail"] = &managedSession{info: SessionInfo{
		ID:         "sess_fail",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateDeleted,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}}
	m.mu.Unlock()

	repaired, err := m.RepairRunningSessions(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, repaired, "only sess_ok should be repaired")
}

// ─── DetachWorkerIf CAS tests ─────────────────────────────────────────────────

func TestManager_DetachWorkerIf_CAS_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_cas_ok",
		UserID:     "user_cas",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_cas_ok"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_cas_ok", w)
	require.NoError(t, err)

	// Detach with the correct expected worker → success.
	detached := m.DetachWorkerIf("sess_cas_ok", w)
	require.True(t, detached)

	// Worker gone, pool released.
	require.Nil(t, m.GetWorker("sess_cas_ok"))
	total, _, _ := m.Stats()
	require.Equal(t, 0, total)
}

func TestManager_DetachWorkerIf_CAS_Mismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	seed := &SessionInfo{
		ID:         "sess_cas_mismatch",
		UserID:     "user_cas2",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	m.mu.Lock()
	m.sessions["sess_cas_mismatch"] = &managedSession{info: *seed}
	m.mu.Unlock()

	w := newMockWorker(worker.TypeClaudeCode, 0)
	w.On("Terminate", mock.Anything).Return(nil)
	err = m.AttachWorker(context.Background(), "sess_cas_mismatch", w)
	require.NoError(t, err)

	// Try to detach with a different worker instance → CAS failure.
	otherWorker := newMockWorker(worker.TypeClaudeCode, 0)
	detached := m.DetachWorkerIf("sess_cas_mismatch", otherWorker)
	require.False(t, detached)

	// Original worker still attached, pool not released.
	require.NotNil(t, m.GetWorker("sess_cas_mismatch"))
	total, _, _ := m.Stats()
	require.Equal(t, 1, total)
}

func TestManager_DetachWorkerIf_NilManager(t *testing.T) {
	t.Parallel()

	m := (*Manager)(nil)
	detached := m.DetachWorkerIf("any", nil)
	require.False(t, detached)
}

func TestManager_DetachWorkerIf_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "sess_ghost_cas").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	detached := m.DetachWorkerIf("sess_ghost_cas", nil)
	require.False(t, detached)
}

func TestManager_ClearContext_PreservesOtherFields(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_clear_preserved",
		UserID:     "user_preserve",
		OwnerID:    "owner_preserve",
		BotID:      "bot_preserve",
		WorkerType: worker.TypeOpenCodeSrv,
		State:      events.StateRunning,
		CreatedAt:  now.Add(-30 * time.Minute),
		UpdatedAt:  now.Add(-30 * time.Minute),
		Context:    map[string]any{"some": "context"},
	}
	m.mu.Lock()
	m.sessions["sess_clear_preserved"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	err = m.ClearContext(ctx, "sess_clear_preserved")
	require.NoError(t, err)

	// Verify other fields preserved in-memory
	m.mu.RLock()
	ms := m.sessions["sess_clear_preserved"]
	m.mu.RUnlock()
	require.NotNil(t, ms)
	require.Equal(t, "user_preserve", ms.info.UserID)
	require.Equal(t, "owner_preserve", ms.info.OwnerID)
	require.Equal(t, "bot_preserve", ms.info.BotID)
	require.Equal(t, worker.TypeOpenCodeSrv, ms.info.WorkerType)
	require.Equal(t, events.StateRunning, ms.info.State)
	require.Empty(t, ms.info.Context)
}

// ─── UpdateWorkerSessionID tests ─────────────────────────────────────────────

func TestManager_UpdateWorkerSessionID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_wsid",
		UserID:     "user1",
		WorkerType: worker.TypeOpenCodeSrv,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.mu.Lock()
	m.sessions["sess_wsid"] = &managedSession{info: *seed}
	m.mu.Unlock()

	store.On("UpdateWorkerSessionIDSQL", mock.Anything, "sess_wsid", "ocs_internal_123").Return(nil)

	err = m.UpdateWorkerSessionID(ctx, "sess_wsid", "ocs_internal_123")
	require.NoError(t, err)

	// Verify in-memory state
	info, _ := m.Get(context.Background(), "sess_wsid")
	require.Equal(t, "ocs_internal_123", info.WorkerSessionID)
}

func TestManager_UpdateWorkerSessionID_SameValue_Idempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:              "sess_wsid_same",
		UserID:          "user1",
		WorkerType:      worker.TypeOpenCodeSrv,
		State:           events.StateRunning,
		CreatedAt:       now,
		UpdatedAt:       now,
		WorkerSessionID: "existing_id",
	}
	m.mu.Lock()
	m.sessions["sess_wsid_same"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Same value — fast-path skip: Upsert should not be called by
	// UpdateWorkerSessionID when in-memory matches and force=false.
	beforeCount := len(store.Calls)
	store.On("Upsert", mock.Anything, mock.Anything).Maybe().Return(nil)
	err = m.UpdateWorkerSessionID(ctx, "sess_wsid_same", "existing_id")
	require.NoError(t, err)
	// No new Upsert calls from UpdateWorkerSessionID (fast-path skip).
	for _, call := range store.Calls[beforeCount:] {
		require.NotEqual(t, "Upsert", call.Method,
			"UpdateWorkerSessionID with same value should not trigger Upsert (fast-path)")
	}
}

func TestManager_UpdateWorkerSessionID_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Get", ctx, "ghost").Return(nil, ErrSessionNotFound)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	err = m.UpdateWorkerSessionID(ctx, "ghost", "any")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestManager_UpdateWorkerSessionID_NilManager(t *testing.T) {
	t.Parallel()

	m := (*Manager)(nil)
	err := m.UpdateWorkerSessionID(context.Background(), "any", "id")
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

// ─── Permission ceiling tests ────────────────────────────────────────────────

func TestManager_SetPermissionCeilingIfEmpty(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_ceiling"] = &managedSession{info: SessionInfo{
		ID:         "sess_ceiling",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}}
	m.mu.Unlock()

	// The store remains authoritative when another instance captured a stricter
	// value first. The caller alias is normalized before persistence.
	store.On("SetPermissionCeilingIfEmpty", ctx, "sess_ceiling", worker.PermissionModeWorkspace).
		Return(worker.PermissionModeReadOnly, nil)

	stored, err := m.SetPermissionCeilingIfEmpty(ctx, "sess_ceiling", "default")
	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeReadOnly, stored)

	info, err := m.Get(ctx, "sess_ceiling")
	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeReadOnly, info.PermissionCeiling)
}

func TestManager_SetPermissionCeilingIfEmpty_RefreshesSpecSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("SetPermissionCeilingIfEmpty", ctx, "sess_snapshot_ceiling", worker.PermissionModeWorkspace).
		Return(worker.PermissionModeWorkspace, nil)
	store.On("UpdateSpecSnapshot", ctx, "sess_snapshot_ceiling", mock.AnythingOfType("*agentspec.EffectiveAgentSpecSnapshot")).Return(nil).Once()

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	snap := agentspec.SnapshotFromSpec(agentspec.AgentSpec{
		Worker: agentspec.WorkerSpec{Type: string(worker.TypeClaudeCode)},
		Policy: agentspec.PolicySpec{AllowedTools: []string{"Read"}},
	})
	originalHash := snap.Hash
	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_snapshot_ceiling"] = &managedSession{info: SessionInfo{
		ID:           "sess_snapshot_ceiling",
		UserID:       "user1",
		WorkerType:   worker.TypeClaudeCode,
		AllowedTools: []string{"Read"},
		SpecSnapshot: &snap,
		State:        events.StateRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
	}}
	m.mu.Unlock()

	stored, err := m.SetPermissionCeilingIfEmpty(ctx, "sess_snapshot_ceiling", worker.PermissionModeWorkspace)
	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeWorkspace, stored)

	info, err := m.Get(ctx, "sess_snapshot_ceiling")
	require.NoError(t, err)
	require.NotNil(t, info.SpecSnapshot)
	require.Equal(t, worker.PermissionModeWorkspace, info.SpecSnapshot.PermissionMode)
	require.NotEqual(t, originalHash, info.SpecSnapshot.Hash,
		"the fingerprint must change when the effective permission ceiling is captured")
}

func TestManager_SetPermissionCeilingIfEmpty_RetryPersistsRefreshedSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("SetPermissionCeilingIfEmpty", ctx, "sess_snapshot_retry", worker.PermissionModeWorkspace).
		Return(worker.PermissionModeWorkspace, nil).Twice()
	store.On("UpdateSpecSnapshot", ctx, "sess_snapshot_retry", mock.AnythingOfType("*agentspec.EffectiveAgentSpecSnapshot")).
		Return(errors.New("persist failed")).Once()
	store.On("UpdateSpecSnapshot", ctx, "sess_snapshot_retry", mock.MatchedBy(func(snapshot *agentspec.EffectiveAgentSpecSnapshot) bool {
		return snapshot != nil && snapshot.PermissionMode == worker.PermissionModeWorkspace
	})).Return(nil).Once()

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	snap := agentspec.SnapshotFromSpec(agentspec.AgentSpec{
		Worker: agentspec.WorkerSpec{Type: string(worker.TypeClaudeCode)},
		Policy: agentspec.PolicySpec{AllowedTools: []string{"Read"}},
	})
	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_snapshot_retry"] = &managedSession{info: SessionInfo{
		ID:           "sess_snapshot_retry",
		UserID:       "user1",
		WorkerType:   worker.TypeClaudeCode,
		AllowedTools: []string{"Read"},
		SpecSnapshot: &snap,
		State:        events.StateRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
	}}
	m.mu.Unlock()

	_, err = m.SetPermissionCeilingIfEmpty(ctx, "sess_snapshot_retry", worker.PermissionModeWorkspace)
	require.ErrorContains(t, err, "persist permission snapshot")

	stored, err := m.SetPermissionCeilingIfEmpty(ctx, "sess_snapshot_retry", worker.PermissionModeWorkspace)
	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeWorkspace, stored)
}

func TestManager_SetPermissionCeilingIfEmpty_RejectsInvalidAndMissingSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Get", ctx, "missing").Return(nil, ErrSessionNotFound)

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	_, err = m.SetPermissionCeilingIfEmpty(ctx, "missing", "not-a-mode")
	require.ErrorIs(t, err, worker.ErrInvalidPermissionMode)

	_, err = m.SetPermissionCeilingIfEmpty(ctx, "missing", worker.PermissionModeReadOnly)
	require.ErrorIs(t, err, ErrSessionNotFound)

	var nilManager *Manager
	_, err = nilManager.SetPermissionCeilingIfEmpty(ctx, "missing", worker.PermissionModeReadOnly)
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestManager_SetPermissionCeilingIfEmpty_StoreError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_ceiling_error"] = &managedSession{info: SessionInfo{
		ID:         "sess_ceiling_error",
		UserID:     "user1",
		WorkerType: worker.TypeClaudeCode,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}}
	m.mu.Unlock()

	storeErr := errors.New("store unavailable")
	store.On(
		"SetPermissionCeilingIfEmpty",
		ctx,
		"sess_ceiling_error",
		worker.PermissionModeWorkspace,
	).Return("", storeErr)

	_, err = m.SetPermissionCeilingIfEmpty(ctx, "sess_ceiling_error", worker.PermissionModeWorkspace)
	require.ErrorIs(t, err, storeErr)
}

// ─── ResetGCInterval tests ───────────────────────────────────────────────────

func TestManager_ResetGCInterval(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Should not panic with valid interval
	m.ResetGCInterval(30 * time.Second)

	// Zero/negative interval should be ignored
	m.ResetGCInterval(0)
	m.ResetGCInterval(-1 * time.Second)
}

// ─── Pool() accessor tests ───────────────────────────────────────────────────

func TestManager_Pool(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)

	store.On("Close").Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	pool := m.Pool()
	require.NotNil(t, pool)
	total, max, _ := pool.Stats()
	require.Equal(t, 0, total)
	require.Equal(t, cfg.Pool.MaxSize, max)
}

// ─── RunningIndex tests ──────────────────────────────────────────────────────

func TestRunningIndex_TransitionMaintained(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed CREATED → transition to RUNNING → index should contain it.
	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_ri"] = &managedSession{info: SessionInfo{
		ID: "sess_ri", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateCreated, CreatedAt: now, UpdatedAt: now,
	}}
	m.mu.Unlock()

	require.Empty(t, m.getRunningSessionIDs(), "CREATED session should not be in runningIndex")

	err = m.Transition(ctx, "sess_ri", events.StateRunning)
	require.NoError(t, err)
	require.Contains(t, m.getRunningSessionIDs(), "sess_ri", "RUNNING transition should add to runningIndex")

	// RUNNING → TERMINATED → removed from index.
	err = m.TransitionWithReason(ctx, "sess_ri", events.StateTerminated, "test")
	require.NoError(t, err)
	require.NotContains(t, m.getRunningSessionIDs(), "sess_ri", "TERMINATED transition should remove from runningIndex")
}

func TestRunningIndex_DeleteCleanup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_ri_del"] = &managedSession{info: SessionInfo{
		ID: "sess_ri_del", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, CreatedAt: now, UpdatedAt: now,
	}}
	m.mu.Unlock()
	m.addToRunningIndex("sess_ri_del")

	require.Contains(t, m.getRunningSessionIDs(), "sess_ri_del")

	err = m.Delete(ctx, "sess_ri_del")
	require.NoError(t, err)
	require.NotContains(t, m.getRunningSessionIDs(), "sess_ri_del", "Delete should remove from runningIndex")
}

func TestRunningIndex_DeletePhysicalCleanup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("DeletePhysical", ctx, "sess_ri_phys").Return(nil)
	store.On("Close").Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	m.mu.Lock()
	m.sessions["sess_ri_phys"] = &managedSession{info: SessionInfo{
		ID: "sess_ri_phys", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, CreatedAt: now, UpdatedAt: now,
	}}
	m.mu.Unlock()
	m.addToRunningIndex("sess_ri_phys")

	require.Contains(t, m.getRunningSessionIDs(), "sess_ri_phys")

	err = m.DeletePhysical(ctx, "sess_ri_phys")
	require.NoError(t, err)
	require.NotContains(t, m.getRunningSessionIDs(), "sess_ri_phys", "DeletePhysical should remove from runningIndex")
}

func TestGC_EvictsOldTerminatedSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := new(mockStore)
	store.Test(t)
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Return([]string(nil), nil)
	store.On("Close").Return(nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)

	// Seed a TERMINATED session with UpdatedAt beyond 24h TTL.
	oldTime := time.Now().Add(-25 * time.Hour)
	m.mu.Lock()
	m.sessions["sess_old_term"] = &managedSession{info: SessionInfo{
		ID: "sess_old_term", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, CreatedAt: oldTime, UpdatedAt: oldTime,
	}}
	m.mu.Unlock()

	// Seed a recent TERMINATED session within TTL.
	recentTime := time.Now()
	m.mu.Lock()
	m.sessions["sess_recent_term"] = &managedSession{info: SessionInfo{
		ID: "sess_recent_term", UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, CreatedAt: recentTime, UpdatedAt: recentTime,
	}}
	m.mu.Unlock()

	m.gc(ctx)

	// Old TERMINATED session evicted from memory.
	m.mu.RLock()
	_, oldOk := m.sessions["sess_old_term"]
	_, recentOk := m.sessions["sess_recent_term"]
	m.mu.RUnlock()
	require.False(t, oldOk, "TERMINATED session older than TTL should be evicted from memory")
	require.True(t, recentOk, "TERMINATED session within TTL should remain in memory")
}

// ─── transitionState WorkerSessionID preservation tests ─────────────────────────

// raceStore wraps mockStore to inject a concurrent UpdateWorkerSessionID call
// during the Upsert window (when ms.mu is released), reproducing the race that
// causes WorkerSessionID loss (#709).
type raceStore struct {
	mockStore
	onUpsert   func()                      // called inside Upsert while ms.mu is released
	lastUpsert atomic.Pointer[SessionInfo] // captures the last Upserted info
}

func (r *raceStore) Upsert(ctx context.Context, info *SessionInfo) error {
	r.lastUpsert.Store(info)
	if r.onUpsert != nil {
		r.onUpsert()
	}
	return r.mockStore.Upsert(ctx, info)
}

func TestTransition_PreservesWorkerSessionID_OnConcurrentUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()
	store := &raceStore{}
	store.Test(t)

	store.On("Close").Return(nil)
	store.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Maybe().Return([]string(nil), nil)
	store.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Maybe().Return([]string(nil), nil)
	store.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Maybe().Return(nil)
	m, err := NewManager(ctx, nil, cfg, nil, store)
	require.NoError(t, err)
	defer m.Close()

	// Seed a RUNNING session with empty WorkerSessionID (simulates post-Start
	// state before persistWorkerSessionID fires).
	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_race_wsid",
		UserID:     "user1",
		WorkerType: worker.TypeACP,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.mu.Lock()
	m.sessions["sess_race_wsid"] = &managedSession{info: *seed}
	m.mu.Unlock()

	// Set up the race: during Transition's Upsert (ms.mu released), a
	// concurrent goroutine calls UpdateWorkerSessionID.
	var callCount atomic.Int32
	started := make(chan struct{})
	done := make(chan struct{})

	store.onUpsert = func() {
		if callCount.Add(1) == 1 {
			// First Upsert call (from Transition). Trigger concurrent update.
			close(started)
			<-done // Wait for concurrent update to finish.
		}
		// Subsequent Upsert calls (from UpdateWorkerSessionID) pass through.
	}

	go func() {
		<-started // Wait for Transition's Upsert to start.
		_ = m.UpdateWorkerSessionID(ctx, "sess_race_wsid", "hermes_session_abc")
		close(done)
	}()

	// The mockStore Upsert needs to be set up for both calls.
	store.On("Upsert", ctx, mock.AnythingOfType("*session.SessionInfo")).Return(nil)
	store.On("UpdateWorkerSessionIDSQL", mock.Anything, "sess_race_wsid", "hermes_session_abc").Return(nil)

	// Transition RUNNING → IDLE. The candidate snapshot has empty WorkerSessionID,
	// but the concurrent UpdateWorkerSessionID sets it to "hermes_session_abc".
	err = m.Transition(ctx, "sess_race_wsid", events.StateIdle)
	require.NoError(t, err)

	// Verify WorkerSessionID is preserved in-memory.
	info, _ := m.Get(context.Background(), "sess_race_wsid")
	require.Equal(t, events.StateIdle, info.State, "state should transition to IDLE")
	require.Equal(t, "hermes_session_abc", info.WorkerSessionID,
		"WorkerSessionID must be preserved when set concurrently during transition")

	// Verify the concurrent UpdateWorkerSessionID used targeted SQL (not Upsert).
	store.AssertCalled(t, "UpdateWorkerSessionIDSQL", mock.Anything, "sess_race_wsid", "hermes_session_abc")
}

func TestTransition_GuardRePersistError_InMemoryConsistent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.Default()

	gs := &guardErrStore{
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	gs.Test(t)
	gs.On("Close").Return(nil)
	gs.On("GetExpiredMaxLifetime", mock.Anything, mock.AnythingOfType("time.Time")).Maybe().Return([]string(nil), nil)
	gs.On("GetExpiredIdle", mock.Anything, mock.AnythingOfType("time.Time")).Maybe().Return([]string(nil), nil)
	gs.On("DeleteTerminated", mock.Anything, mock.AnythingOfType("time.Time"), mock.AnythingOfType("time.Time")).Maybe().Return(nil)
	gs.mockStore.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Maybe().Return(nil)

	m, err := NewManager(ctx, nil, cfg, nil, gs)
	require.NoError(t, err)
	defer m.Close()

	now := time.Now()
	seed := &SessionInfo{
		ID:         "sess_guard_err",
		UserID:     "user1",
		WorkerType: worker.TypeACP,
		State:      events.StateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.mu.Lock()
	m.sessions["sess_guard_err"] = &managedSession{info: *seed}
	m.mu.Unlock()

	go func() {
		<-gs.started
		_ = m.UpdateWorkerSessionID(ctx, "sess_guard_err", "hermes_session_xyz")
		// After concurrent update completes, the next Upsert is the guard re-persist.
		gs.failNextSQL.Store(true)
		close(gs.done)
	}()

	err = m.Transition(ctx, "sess_guard_err", events.StateIdle)
	require.NoError(t, err)

	// In-memory state must have WorkerSessionID even when guard re-persist
	// fails, because candidate already captured it before ms.info = candidate.
	info, _ := m.Get(context.Background(), "sess_guard_err")
	require.Equal(t, events.StateIdle, info.State)
	require.Equal(t, "", info.WorkerSessionID,
		"in-memory WorkerSessionID must be rolled back to empty when guard re-persist fails (safety-net will repair)")

	// Verify UpdateWorkerSessionIDSQL was called: first by the concurrent
	// UpdateWorkerSessionID (succeeded), then by the guard (failed).
	// lastSQLWSID captures the last call's value (the guard's attempt).
	sqlWSID := gs.lastSQLWSID.Load()
	require.NotNil(t, sqlWSID, "guard should have attempted UpdateWorkerSessionIDSQL")
	require.Equal(t, "hermes_session_xyz", *sqlWSID,
		"guard SQL UPDATE must target the preserved WorkerSessionID")
}

// guardErrStore shadows Upsert to inject a concurrent UpdateWorkerSessionID
// during the main transition Upsert, then fails the guard re-persist
// (UpdateWorkerSessionIDSQL) using a semantic flag.
type guardErrStore struct {
	mockStore
	failNextSQL    atomic.Bool // set by goroutine: next UpdateWorkerSessionIDSQL should fail
	sawFirst       atomic.Bool // set after the first Upsert (transition main) begins
	started        chan struct{}
	done           chan struct{}
	lastSuccessful atomic.Pointer[SessionInfo]
	lastSQLWSID    atomic.Pointer[string] // captures the wsid from UpdateWorkerSessionIDSQL
}

func (g *guardErrStore) Upsert(ctx context.Context, info *SessionInfo) error {
	if !g.sawFirst.Load() {
		// Main transition Upsert -- synchronize and always succeed.
		g.sawFirst.Store(true)
		close(g.started)
		<-g.done
		return nil
	}
	// Subsequent Upsert calls (from UpdateWorkerSessionID).
	infoCopy := *info
	g.lastSuccessful.Store(&infoCopy)
	return nil
}

func (g *guardErrStore) UpdateWorkerSessionIDSQL(ctx context.Context, id, workerSessionID string) error {
	wsid := workerSessionID
	g.lastSQLWSID.Store(&wsid)
	if g.failNextSQL.CompareAndSwap(true, false) {
		return fmt.Errorf("simulated guard persist failure")
	}
	return nil
}
