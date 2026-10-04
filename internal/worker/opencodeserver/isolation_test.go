package opencodeserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
)

// #867: ceiling 缺失或宽松时 unknown，只 restrictive tier 才 declared。
func TestReportIsolationHonestTiers(t *testing.T) {
	t.Parallel()
	w := New()
	got := worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
	require.Equal(t, worker.IsolationUnknown, got.Filesystem)
	require.Equal(t, worker.IsolationUnknown, got.Network)

	require.NoError(t, w.permissionCeiling.Capture(worker.PermissionModeReadOnly))
	got = worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
	require.Equal(t, worker.IsolationDeclared, got.Filesystem)
	require.Equal(t, worker.IsolationUnknown, got.Network)
}
