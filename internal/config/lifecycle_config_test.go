package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLifecycleConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg := Default()

	require.Equal(t, "v2", cfg.Lifecycle.Policy)
	require.Equal(t, 7*24*time.Hour, cfg.Lifecycle.Conversation.ArchiveAfter)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Conversation.RetentionAfterLastInput)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Content.Retention)
	require.Equal(t, 30*time.Minute, cfg.Worker.TurnTimeout)
	require.Equal(t, 7*24*time.Hour, cfg.Lifecycle.EffectPayload.RetentionAfterSettlement)
	require.Equal(t, 90*24*time.Hour, cfg.Lifecycle.Facts.RetentionAfterSettlement)
	require.False(t, cfg.Lifecycle.Audit.CaptureContent)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Audit.FactsRetention)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Audit.ContentRetention)
	require.Equal(t, 24*time.Hour, cfg.Lifecycle.Media.Retention)
	require.Equal(t, 48*time.Hour, cfg.Lifecycle.Trace.Retention)
	require.Equal(t, 100, cfg.Lifecycle.GC.BatchSize)
	require.Equal(t, time.Minute, cfg.Lifecycle.GC.Interval)
	require.Equal(t, time.Hour, cfg.Lifecycle.GC.MaxLag)
	require.Equal(t, 15*time.Minute, cfg.Execution.Queue.InteractiveTTL)

	require.Empty(t, cfg.Validate())
}

func TestEffectiveTurnTimeout(t *testing.T) {
	t.Parallel()

	cfg := Default()
	require.Equal(t, 30*time.Minute, EffectiveTurnTimeout(cfg))

	cfg.Worker.TurnTimeout = 42 * time.Minute
	require.Equal(t, 42*time.Minute, EffectiveTurnTimeout(cfg))

	cfg.Worker.TurnTimeout = 0
	cfg.Lifecycle.Policy = LifecyclePolicyV2
	require.Equal(t, 30*time.Minute, EffectiveTurnTimeout(cfg))

	cfg.Lifecycle.Policy = LifecyclePolicyLegacy
	require.Zero(t, EffectiveTurnTimeout(cfg))
}

func TestLifecycleConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "unknown policy",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Policy = "future"
			},
			wantErr: "lifecycle.policy",
		},
		{
			name: "zero archive duration",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Conversation.ArchiveAfter = 0
			},
			wantErr: "lifecycle.conversation.archive_after",
		},
		{
			name: "archive after session expiration",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Conversation.ArchiveAfter = 181 * 24 * time.Hour
			},
			wantErr: "lifecycle.conversation.archive_after",
		},
		{
			name: "zero conversation retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Conversation.RetentionAfterLastInput = 0
			},
			wantErr: "lifecycle.conversation.retention_after_last_input",
		},
		{
			name: "zero message retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Content.Retention = 0
			},
			wantErr: "lifecycle.content.retention",
		},
		{
			name: "zero effect payload retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.EffectPayload.RetentionAfterSettlement = 0
			},
			wantErr: "lifecycle.effect_payload.retention_after_settlement",
		},
		{
			name: "zero execution fact retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Facts.RetentionAfterSettlement = 0
			},
			wantErr: "lifecycle.facts.retention_after_settlement",
		},
		{
			name: "audit payload capture requires separate storage",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Audit.CaptureContent = true
			},
			wantErr: "lifecycle.audit.capture_content is unsupported",
		},
		{
			name: "audit payload exceeds chat body retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Audit.CaptureContent = true
				cfg.Lifecycle.Audit.ContentRetention = 181 * 24 * time.Hour
			},
			wantErr: "lifecycle.audit.content_retention",
		},
		{
			name: "zero media retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Media.Retention = 0
			},
			wantErr: "lifecycle.media.retention",
		},
		{
			name: "zero trace retention",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.Trace.Retention = 0
			},
			wantErr: "lifecycle.trace.retention",
		},
		{
			name: "zero gc batch",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.GC.BatchSize = 0
			},
			wantErr: "lifecycle.gc.batch_size",
		},
		{
			name: "zero gc interval",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.GC.Interval = 0
			},
			wantErr: "lifecycle.gc.interval",
		},
		{
			name: "zero gc lag target",
			mutate: func(cfg *Config) {
				cfg.Lifecycle.GC.MaxLag = 0
			},
			wantErr: "lifecycle.gc.max_lag",
		},
		{
			name: "zero interactive queue ttl",
			mutate: func(cfg *Config) {
				cfg.Execution.Queue.InteractiveTTL = 0
			},
			wantErr: "execution.queue.interactive_ttl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			tt.mutate(cfg)

			errs := strings.Join(cfg.Validate(), "\n")
			require.Contains(t, errs, tt.wantErr)
		})
	}
}

func TestLoad_LifecycleEnvironmentOverrides(t *testing.T) {
	t.Setenv("HOTPLEX_LIFECYCLE_CONVERSATION_RETENTION_AFTER_LAST_INPUT", "8760h")
	t.Setenv("HOTPLEX_LIFECYCLE_CONTENT_RETENTION", "8760h")
	t.Setenv("HOTPLEX_EXECUTION_QUEUE_INTERACTIVE_TTL", "45m")
	t.Setenv("HOTPLEX_WORKER_TURN_TIMEOUT", "42m")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 365*24*time.Hour, cfg.Lifecycle.Conversation.RetentionAfterLastInput)
	require.Equal(t, 365*24*time.Hour, cfg.Lifecycle.Content.Retention)
	require.Equal(t, 45*time.Minute, cfg.Execution.Queue.InteractiveTTL)
	require.Equal(t, 42*time.Minute, cfg.Worker.TurnTimeout)
	require.Empty(t, cfg.Validate())
}

func TestLoad_LifecycleEmptyEnvironmentOverrideIsRejected(t *testing.T) {
	t.Setenv("HOTPLEX_LIFECYCLE_CONVERSATION_RETENTION_AFTER_LAST_INPUT", "")

	_, err := Load("")
	require.ErrorContains(t, err, "HOTPLEX_LIFECYCLE_CONVERSATION_RETENTION_AFTER_LAST_INPUT")
	require.ErrorContains(t, err, "explicitly empty")
}

func TestLoad_LifecycleYAMLOverrides(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configData := []byte("lifecycle:\n  policy: legacy\n  conversation:\n    archive_after: 72h\n    retention_after_last_input: 8760h\n  content:\n    retention: 8760h\nexecution:\n  queue:\n    interactive_ttl: 45m\n")
	require.NoError(t, os.WriteFile(configPath, configData, 0o600))

	cfg, err := Load(configPath)
	require.NoError(t, err)
	require.Equal(t, "legacy", cfg.Lifecycle.Policy)
	require.Equal(t, 72*time.Hour, cfg.Lifecycle.Conversation.ArchiveAfter)
	require.Equal(t, 365*24*time.Hour, cfg.Lifecycle.Conversation.RetentionAfterLastInput)
	require.Equal(t, 365*24*time.Hour, cfg.Lifecycle.Content.Retention)
	require.Equal(t, 45*time.Minute, cfg.Execution.Queue.InteractiveTTL)
	require.Empty(t, cfg.Validate())
}

func TestLoad_RepositoryConfigHasLifecycleDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadRecursive(filepath.Join("..", "..", "configs", "config.yaml"), nil)
	require.NoError(t, err)
	require.Equal(t, LifecyclePolicyV2, cfg.Lifecycle.Policy)
	require.Equal(t, 7*24*time.Hour, cfg.Lifecycle.Conversation.ArchiveAfter)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Conversation.RetentionAfterLastInput)
	require.Equal(t, 180*24*time.Hour, cfg.Lifecycle.Content.Retention)
	require.Equal(t, 15*time.Minute, cfg.Execution.Queue.InteractiveTTL)
	require.Empty(t, cfg.Validate())
}
