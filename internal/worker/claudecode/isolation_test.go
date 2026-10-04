package claudecode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
)

// #867: read-only/workspace 如实 declared，network 永 unknown，不虚报 enforced。
func TestReportIsolationHonestTiers(t *testing.T) {
	t.Parallel()
	w := New()
	for _, mode := range []string{worker.PermissionModeReadOnly, worker.PermissionModeWorkspace} {
		operatorPermissionMode.Store(mode)
		got := worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
		require.Equal(t, worker.IsolationDeclared, got.Filesystem, "mode %s", mode)
		require.Equal(t, worker.IsolationUnknown, got.Network)
		require.NotContains(t, string(worker.IsolationEnforced), string(got.Filesystem)+string(got.Network))
	}
	operatorPermissionMode.Store(worker.PermissionModeBypass)
	got := worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
	require.Equal(t, worker.IsolationUnknown, got.Filesystem)
	require.Equal(t, worker.IsolationUnknown, got.Network)
	operatorPermissionMode.Store("")
}
