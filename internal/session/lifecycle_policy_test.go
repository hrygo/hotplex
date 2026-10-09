package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
)

func TestNewInitialLifecycleState_V2(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	cfg := config.Default().Lifecycle
	state, err := newInitialLifecycleState(now, cfg)
	require.NoError(t, err)
	require.Equal(t, config.LifecyclePolicyV2, state.policy)
	require.NotEmpty(t, state.revision)
	require.Equal(t, now.Add(7*24*time.Hour), *state.archiveAt)
	require.Equal(t, now.Add(180*24*time.Hour), *state.conversationExpiresAt)
	require.Equal(t, state.conversationExpiresAt, state.historyExpiresAt)
}

func TestNewInitialLifecycleState_LegacyHasNoV2Deadlines(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Lifecycle
	cfg.Policy = config.LifecyclePolicyLegacy
	state, err := newInitialLifecycleState(time.Now(), cfg)
	require.NoError(t, err)
	require.Equal(t, config.LifecyclePolicyLegacy, state.policy)
	require.Empty(t, state.revision)
	require.Nil(t, state.archiveAt)
	require.Nil(t, state.conversationExpiresAt)
	require.Nil(t, state.historyExpiresAt)
}

func TestLifecyclePolicyRevisionChangesWithPolicyValues(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Lifecycle
	first := lifecyclePolicyRevision(cfg)
	require.Equal(t, first, lifecyclePolicyRevision(cfg))

	cfg.Conversation.RetentionAfterLastInput += 24 * time.Hour
	require.NotEqual(t, first, lifecyclePolicyRevision(cfg))
}
