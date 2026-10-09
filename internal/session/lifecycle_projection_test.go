package session

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestShouldNotifyTerminationCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy string
		reason string
		want   bool
	}{
		{name: "v2 idle release", policy: config.LifecyclePolicyV2, reason: "idle_timeout", want: false},
		{name: "v2 archive command", policy: config.LifecyclePolicyV2, reason: "gc", want: false},
		{name: "v2 cron completion", policy: config.LifecyclePolicyV2, reason: "cron_complete", want: false},
		{name: "v2 explicit terminate", policy: config.LifecyclePolicyV2, reason: "client_kill", want: true},
		{name: "legacy idle behavior", policy: config.LifecyclePolicyLegacy, reason: "idle_timeout", want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, shouldNotifyTerminationCleanup(test.policy, test.reason))
		})
	}
}

func TestUpdateArchiveProjection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	archiveAt := now
	tests := []struct {
		name string
		info SessionInfo
		want bool
	}{
		{
			name: "v2 deadline reached",
			info: SessionInfo{LifecyclePolicy: config.LifecyclePolicyV2, ArchiveAt: &archiveAt},
			want: true,
		},
		{
			name: "v2 deadline remains in future",
			info: SessionInfo{LifecyclePolicy: config.LifecyclePolicyV2, ArchiveAt: ptr(now.Add(time.Second))},
			want: false,
		},
		{
			name: "legacy deadline does not imply archive",
			info: SessionInfo{LifecyclePolicy: config.LifecyclePolicyLegacy, ArchiveAt: &archiveAt},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updateArchiveProjection(&test.info, now)
			require.Equal(t, test.want, test.info.Archived)
		})
	}
}

func TestManager_V2RuntimeTerminationSkipsLogicalCleanupCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	store.On("Upsert", mock.Anything, mock.AnythingOfType("*session.SessionInfo")).Return(nil)

	manager, err := NewManager(ctx, nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer manager.Close()

	now := time.Now()
	manager.mu.Lock()
	manager.sessions["v2-idle"] = &managedSession{
		info: SessionInfo{
			ID:              "v2-idle",
			UserID:          "u1",
			WorkerType:      worker.TypeClaudeCode,
			State:           events.StateIdle,
			LifecyclePolicy: config.LifecyclePolicyV2,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
	}
	manager.mu.Unlock()

	var cleanupCalls atomic.Int32
	manager.OnTerminate = func(string) { cleanupCalls.Add(1) }

	require.NoError(t, manager.TransitionWithReason(ctx, "v2-idle", events.StateTerminated, "idle_timeout"))
	require.Never(t, func() bool { return cleanupCalls.Load() > 0 }, 50*time.Millisecond, time.Millisecond)
}

func TestManager_ListSetsArchiveProjection(t *testing.T) {
	t.Parallel()

	store := new(mockStore)
	store.Test(t)
	store.On("Close").Return(nil)
	now := time.Now()
	store.On("List", mock.Anything, "u1", "webchat", "", 20, 0).Return([]*SessionInfo{
		{
			ID:              "archived",
			LifecyclePolicy: config.LifecyclePolicyV2,
			ArchiveAt:       ptr(now.Add(-time.Hour)),
		},
	}, nil)

	manager, err := NewManager(t.Context(), nil, config.Default(), nil, store)
	require.NoError(t, err)
	defer manager.Close()

	sessions, err := manager.List(t.Context(), "u1", "webchat", "", 20, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.True(t, sessions[0].Archived)
}
