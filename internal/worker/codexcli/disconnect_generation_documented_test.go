package codexcli

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
)

func TestD07OldEOFDoesNotCloseNewTransport(t *testing.T) {
	t.Parallel()
	m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{})
	m.setStdin(&documentedWriter{})
	old := m.snapshotTransport()
	m.setStdin(&documentedWriter{})
	fresh := m.snapshotTransport()
	m.closeTransport(old)
	select {
	case <-old.done:
	default:
		t.Fatal("old generation was not released")
	}
	select {
	case <-fresh.done:
		t.Fatal("stale EOF closed the replacement")
	default:
	}
	m.closeTransport(fresh)
	require.NotPanics(t, func() { m.closeTransport(fresh) })
}
