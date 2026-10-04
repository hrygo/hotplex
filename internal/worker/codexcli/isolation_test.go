package codexcli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
)

// TestReportIsolation_NeverClaimsEnforced is the point of this reporter. The
// gateway hands a sandbox mode to the codex CLI and has no independent check
// that the CLI honoured it, so "enforced" would be the gateway describing its
// own request as the machine's behaviour.
func TestReportIsolation_NeverClaimsEnforced(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access", "who-knows"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w := &AppServerWorker{manager: &CodexAppServerManager{
				cfg: config.CodexCLIConfig{Sandbox: mode},
			}}
			got := w.ReportIsolation(context.Background(), worker.SessionInfo{})

			require.NotEqual(t, worker.IsolationEnforced, got.Filesystem,
				"the gateway cannot verify codex enforcement, so it must not claim it")
			require.Equal(t, worker.IsolationUnknown, got.Network,
				"the codex sandbox vocabulary says nothing about the network")
			require.Equal(t, worker.IsolationScopeProcess, got.Scope,
				"one app-server serves every codex_cli session")
		})
	}
}

func TestReportIsolation_MapsSandboxModesHonestly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode string
		want worker.IsolationState
	}{
		{mode: "read-only", want: worker.IsolationDeclared},
		{mode: "workspace-write", want: worker.IsolationDeclared},
		// The requested mode IS the absence of a filesystem boundary; calling
		// that "declared" would dress up no restriction as a declared one.
		{mode: "danger-full-access", want: worker.IsolationUnavailable},
		{mode: "", want: worker.IsolationUnknown},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			t.Parallel()
			w := &AppServerWorker{manager: &CodexAppServerManager{
				cfg: config.CodexCLIConfig{Sandbox: tc.mode},
			}}
			got := w.ReportIsolation(context.Background(), worker.SessionInfo{})
			require.Equal(t, tc.want, got.Filesystem)
		})
	}
}

// TestReportIsolation_SessionSandboxOverridesConfig checks the per-session
// value wins, since a per-bot override is what the app-server was launched
// with for that session.
func TestReportIsolation_SessionSandboxOverridesConfig(t *testing.T) {
	t.Parallel()
	w := &AppServerWorker{manager: &CodexAppServerManager{
		cfg: config.CodexCLIConfig{Sandbox: "danger-full-access"},
	}}
	got := w.ReportIsolation(context.Background(), worker.SessionInfo{Sandbox: "read-only"})
	require.Equal(t, worker.IsolationDeclared, got.Filesystem)
}
