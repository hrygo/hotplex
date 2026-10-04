package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDefault_ExecutionQueue pins the queue defaults. The bounds are part of
// the operator contract (an operator has to be able to predict when the queue
// refuses), so they are asserted rather than left implicit.
func TestDefault_ExecutionQueue(t *testing.T) {
	t.Parallel()

	q := Default().Execution.Queue
	require.False(t, q.Enabled,
		"the durable queue must stay off by default: dispatch wiring is what makes it safe")
	require.Equal(t, 20, q.PerSession)
	require.Equal(t, 1000, q.Global)
	require.Equal(t, 64<<10, q.MaxPayloadBytes)
	require.Equal(t, 24*time.Hour, q.TTL)
	require.Equal(t, time.Minute, q.SweepInterval)
	require.Equal(t, 100, q.SweepBatch)
}

// TestLoad_ExecutionQueue_AbsentBlockStaysDisabled proves an existing config
// file with no execution section does not accidentally enable the queue, and
// does not acquire a zero-valued limit set that would refuse every input.
func TestLoad_ExecutionQueue_AbsentBlockStaysDisabled(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("gateway:\n  addr: :8888\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.False(t, cfg.Execution.Queue.Enabled)
	require.Equal(t, 20, cfg.Execution.Queue.PerSession,
		"defaults must survive an absent block; zero limits would refuse everything")
	require.Equal(t, 24*time.Hour, cfg.Execution.Queue.TTL)
}

func TestLoad_ExecutionQueue_Overrides(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`execution:
  queue:
    enabled: true
    per_session: 5
    global: 50
    max_payload_bytes: 4096
    ttl: 2h
    sweep_interval: 30s
    sweep_batch: 25
`), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	q := cfg.Execution.Queue
	require.True(t, q.Enabled)
	require.Equal(t, 5, q.PerSession)
	require.Equal(t, 50, q.Global)
	require.Equal(t, 4096, q.MaxPayloadBytes)
	require.Equal(t, 2*time.Hour, q.TTL)
	require.Equal(t, 30*time.Second, q.SweepInterval)
	require.Equal(t, 25, q.SweepBatch)
}

// TestLoad_ExecutionQueue_EnvOverride proves the documented
// HOTPLEX_EXECUTION_QUEUE_* variables are actually bound. viper's Unmarshal
// only sees environment keys that were explicitly bound, so an unbound key is
// silently ignored — a documented variable that does nothing is worse than an
// undocumented one.
//
// Not parallel: t.Setenv mutates process environment.
func TestLoad_ExecutionQueue_EnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("gateway:\n  addr: :8888\n"), 0o644))

	t.Setenv("HOTPLEX_EXECUTION_QUEUE_ENABLED", "true")
	t.Setenv("HOTPLEX_EXECUTION_QUEUE_PER_SESSION", "7")
	t.Setenv("HOTPLEX_EXECUTION_QUEUE_TTL", "45m")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.True(t, cfg.Execution.Queue.Enabled)
	require.Equal(t, 7, cfg.Execution.Queue.PerSession)
	require.Equal(t, 45*time.Minute, cfg.Execution.Queue.TTL)
	// Untouched keys must keep their defaults rather than collapsing to zero.
	require.Equal(t, 1000, cfg.Execution.Queue.Global)
}
