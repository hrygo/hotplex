package acp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
)

// #867: approve=false 是 declared Gate，approve=true 连 Gate 都没有。
func TestReportIsolationHonestGate(t *testing.T) {
	t.Parallel()
	w := &Worker{}
	w.autoApprove.Store(false)
	got := worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
	require.Equal(t, worker.IsolationDeclared, got.Filesystem)
	require.Equal(t, worker.IsolationUnknown, got.Network)

	w.autoApprove.Store(true)
	got = worker.ReportIsolation(context.Background(), w, worker.SessionInfo{})
	require.Equal(t, worker.IsolationUnknown, got.Filesystem)
}
