package codexcli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/proc"
)

func TestDeepAudit991StaleExitCannotClearReplacement(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	old, current := &proc.Manager{}, &proc.Manager{}
	m.proc = current
	m.refs = 1
	channel := m.Subscribe("new", "new-session")
	mapper := m.getOrCreateConverter("new")
	m.finishOwnedProcess(old, 42)
	require.Same(t, current, m.proc)
	require.Equal(t, stateRunning, m.state)
	require.Equal(t, channel, m.subscribers["new"])
	require.Same(t, mapper, m.getConverter("new"))
	m.Unsubscribe("new")
}

func TestDeepAudit991IdleClaimRejectsNewAcquireAndWrongGeneration(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	owned := &proc.Manager{}
	m.proc = owned
	m.pgid = 123456
	m.refs = 0
	// These tests exercise the claim only, never signal a real process group.
	m.mu.Lock()
	_, claimed := m.claimIdleProcessLocked(&proc.Manager{})
	require.False(t, claimed)
	pgid, claimed := m.claimIdleProcessLocked(owned)
	m.mu.Unlock()
	require.True(t, claimed)
	require.Equal(t, 123456, pgid)
	_, err := m.Acquire(context.Background())
	require.Error(t, err)
	require.Zero(t, m.refs)
	require.Equal(t, stateRetiring, m.state)
	m.finishOwnedProcess(owned, 0)
	require.Equal(t, stateIdle, m.state)
}
