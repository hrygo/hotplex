package codexcli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/proc"
)

type documentedHookHandler struct{ hook func(slog.Record) }

func (h documentedHookHandler) Enabled(context.Context, slog.Level) bool      { return true }
func (h documentedHookHandler) Handle(_ context.Context, r slog.Record) error { h.hook(r); return nil }
func (h documentedHookHandler) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h documentedHookHandler) WithGroup(string) slog.Handler                 { return h }

func TestD04IdleNotPublishedBeforeCleanup(t *testing.T) {
	t.Parallel()
	var m *CodexAppServerManager
	observed := stateStopped
	log := slog.New(documentedHookHandler{hook: func(r slog.Record) {
		if r.Message == "codex-app-server: process crashed" {
			observed = m.state
		}
	}})
	m = NewCodexAppServerManager(log, config.CodexCLIConfig{})
	m.proc = proc.New(proc.Opts{Logger: log})
	m.state = stateRunning
	m.refs = 1
	_ = m.Subscribe("old", "session")
	m.getOrCreateConverter("old")
	// An unstarted manager returns from Wait immediately; cleanup uses the same
	// production path, without a wall-clock race or external executable.
	m.monitorProcess()
	require.NotEqual(t, stateIdle, observed, "idle became visible before old-generation cleanup")
	require.Equal(t, stateIdle, m.state)
	require.Empty(t, m.subscribers)
	require.Empty(t, m.converters)
}

func TestD04NativeFixture(t *testing.T) {
	if os.Getenv("HOTPLEX_D04_NATIVE_FIXTURE") != "1" {
		return
	}
	// Native pipe fixture exits on stdin EOF, or on the manager's group kill.
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestD04AcquireCannotLeaseRetiringProcess(t *testing.T) {
	t.Parallel()
	var m *CodexAppServerManager
	var acquiredErr error
	observed := false
	log := slog.New(documentedHookHandler{hook: func(r slog.Record) {
		if r.Message == "codex-app-server: killing idle process immediately" {
			observed = true
			_, acquiredErr = m.Acquire(context.Background())
		}
	}})
	pm := proc.New(proc.Opts{Logger: log})
	binary, err := os.Executable()
	require.NoError(t, err)
	_, _, _, err = pm.Start(context.Background(), binary, []string{"-test.run=^TestD04NativeFixture$"}, append(os.Environ(), "HOTPLEX_D04_NATIVE_FIXTURE=1"), "")
	require.NoError(t, err)
	defer func() { _ = pm.Kill(); _, _ = pm.Wait() }()
	m = NewCodexAppServerManager(log, config.CodexCLIConfig{})
	m.proc = pm
	m.pgid = pm.PGID()
	m.state = stateRunning
	m.KillIfIdle() // the hook deterministically attempts Acquire after claiming kill
	require.True(t, observed)
	require.Error(t, acquiredErr, "Acquire leased the process after idle retirement was claimed")
	require.Zero(t, m.refs)
	m.monitorProcess()
	require.Equal(t, stateIdle, m.state)
}
