package codexcli

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/proc"
)

type deep991ExitObserver struct {
	manager  *CodexAppServerManager
	observed chan managerState
	once     sync.Once
}

func (h *deep991ExitObserver) Enabled(context.Context, slog.Level) bool { return true }
func (h *deep991ExitObserver) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *deep991ExitObserver) WithGroup(string) slog.Handler            { return h }
func (h *deep991ExitObserver) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "codex-app-server: process exited" || r.Message == "codex-app-server: process crashed" {
		// Both original and repaired monitor log this boundary while holding
		// manager.mu. Observe its invariant without introducing a test race.
		h.once.Do(func() { h.observed <- h.manager.state })
	}
	return nil
}

func TestDeepAudit991ExitCannotAdvertiseIdleBeforeOwnedCleanup(t *testing.T) {
	t.Parallel()
	m := auditRPCManager()
	m.proc = &proc.Manager{}
	m.refs = 0
	observer := &deep991ExitObserver{manager: m, observed: make(chan managerState, 1)}
	m.log = slog.New(observer)
	m.Subscribe("old", "old-session")
	m.getOrCreateConverter("old")
	// Block old converter cleanup. A process state of idle at this boundary
	// would permit the next Acquire before stale resources were retired.
	m.convMu.Lock()
	finished := make(chan struct{})
	go func() { m.monitorProcess(); close(finished) }()
	state := <-observer.observed
	m.convMu.Unlock()
	<-finished
	require.NotEqual(t, stateIdle, state, "old generation exposed idle before cleanup")
	require.Equal(t, stateIdle, m.state)
	require.Empty(t, m.subscribers)
	require.Empty(t, m.converters)
}
