package codexcli

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/proc"
)

func TestD04OldMonitorCannotRetireReplacement(t *testing.T) {
	t.Parallel()
	m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{})
	old, replacement := proc.New(proc.Opts{Logger: slog.Default()}), proc.New(proc.Opts{Logger: slog.Default()})
	m.proc = replacement
	m.state = stateRunning
	sub := m.subscribe("fresh", "session")
	m.monitorBoundProcess(old)
	require.Same(t, replacement, m.proc)
	require.Equal(t, stateRunning, m.state)
	require.Same(t, sub, m.subscribers["fresh"])
	sub.close()
}
