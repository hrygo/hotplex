package config

import (
	"fmt"
	"strings"
	"time"
)

// ─── Config structs ───────────────────────────────────────────────────────────

// Config holds all gateway configuration.
type Config struct {
	Gateway     GatewayConfig   `mapstructure:"gateway"`
	DB          DBConfig        `mapstructure:"db"`
	Worker      WorkerConfig    `mapstructure:"worker"`
	Security    SecurityConfig  `mapstructure:"security"`
	Session     SessionConfig   `mapstructure:"session"`
	Pool        PoolConfig      `mapstructure:"pool"`
	Log         LogConfig       `mapstructure:"log"`
	Admin       AdminConfig     `mapstructure:"admin"`
	WebChat     WebChatConfig   `mapstructure:"webchat"`
	Messaging   MessagingConfig `mapstructure:"messaging"`
	AgentConfig AgentConfig     `mapstructure:"agent_config"`
	Skills      SkillsConfig    `mapstructure:"skills"`
	Cron        CronConfig      `mapstructure:"cron"`
	Webhook     WebhookConfig   `mapstructure:"webhook"`
	OAuth       OAuthConfig     `mapstructure:"oauth"`
	Events      EventsConfig    `mapstructure:"events"`
	Audit       AuditConfig     `mapstructure:"audit"`
	Inherits    string          `mapstructure:"inherits"` // path to parent config file; "" = no inheritance

	// ResolvedAPIKeyUsers is the runtime map of expanded API key value → userID.
	// Populated by resolveAPIKeyUsers() during load. Nil when no mapping configured.
	ResolvedAPIKeyUsers map[string]string `mapstructure:"-"`
}

// MessagingConfig holds messaging platform adapter settings.
// Shared defaults (WorkerType, STTConfig, TTSConfig) are set at this level and propagated
// to each platform config via propagateMessagingDefaults().
// Access control fields (DMPolicy, GroupPolicy, RequireMention, AllowFrom, AllowDMFrom, AllowGroupFrom) support per-bot overrides with platform-level fallback.
// Priority: platform-level > messaging-level > Default().
type MessagingConfig struct {
	TurnSummaryEnabled bool `mapstructure:"turn_summary_enabled"`

	// Shared defaults — platforms inherit; platform-level overrides take precedence.
	WorkerType string `mapstructure:"worker_type"`
	STTConfig  `mapstructure:",squash"`
	TTSConfig  `mapstructure:",squash"`

	// Platform-specific configs.
	Slack   SlackConfig   `mapstructure:"slack"`
	Feishu  FeishuConfig  `mapstructure:"feishu"`
	Yuanxin YuanxinConfig `mapstructure:"yuanxin"`
}

// STT constants for provider values.
const (
	STTProviderLocal       = "local"
	STTProviderFeishu      = "feishu"
	STTProviderFeishuLocal = "feishu+local"
)

// STTConfig holds speech-to-text configuration shared across messaging adapters.
type STTConfig struct {
	// Provider: "local" (external command), "feishu" (cloud API),
	// "feishu+local" (cloud primary, local fallback), "" (disabled).
	Provider string `mapstructure:"stt_provider"`
	// LocalCmd is the command template. {file} is replaced with the audio file path.
	// If {file} is present → ephemeral mode (fork per request).
	// If absent → persistent mode (long-lived subprocess, stdin/stdout JSON protocol).
	LocalCmd string `mapstructure:"stt_local_cmd"`
	// LocalIdleTTL controls auto-shutdown of persistent subprocess. 0 = disabled.
	LocalIdleTTL time.Duration `mapstructure:"stt_local_idle_ttl"`
}

// IsPersistent returns true when LocalCmd uses persistent mode (no {file} placeholder).
func (c STTConfig) IsPersistent() bool {
	return c.LocalCmd != "" && !strings.Contains(c.LocalCmd, "{file}")
}

// FillFrom propagates zero-value fields from defaults into c.
func (c *STTConfig) FillFrom(defaults STTConfig) {
	if c.Provider == "" {
		c.Provider = defaults.Provider
	}
	if c.LocalCmd == "" {
		c.LocalCmd = defaults.LocalCmd
	}
	if c.LocalIdleTTL == 0 {
		c.LocalIdleTTL = defaults.LocalIdleTTL
	}
}

// TTSConfig holds text-to-speech settings. Squashed into platform configs.
type TTSConfig struct {
	// TTSEnabled controls whether voice replies are sent for voice-triggered turns.
	TTSEnabled bool `mapstructure:"tts_enabled"`
	// Provider: "edge" (Microsoft Edge TTS, free), "edge+moss" (Edge primary + MOSS-TTS-Nano CPU fallback), "" (disabled).
	TTSProvider string `mapstructure:"tts_provider"`
	// Voice name for Edge TTS (e.g. "zh-CN-XiaoxiaoNeural", "zh-CN-YunxiNeural").
	Voice string `mapstructure:"tts_voice"`
	// MaxChars limits the LLM summary length before TTS synthesis.
	MaxChars int `mapstructure:"tts_max_chars"`
	// MossModelDir is the directory containing MOSS-TTS-Nano ONNX model assets (used when provider is "edge+moss").
	MossModelDir string `mapstructure:"tts_moss_model_dir"`
	// MossVoice is the MOSS built-in voice preset name (default "Xiaoyu").
	MossVoice string `mapstructure:"tts_moss_voice"`
	// MossPort is the localhost port for the MOSS sidecar HTTP server (default 18083).
	MossPort int `mapstructure:"tts_moss_port"`
	// MossIdleTimeout controls how long the MOSS sidecar stays resident after its
	// last use before being automatically shut down. Default 30m.
	MossIdleTimeout time.Duration `mapstructure:"tts_moss_idle_timeout"`
	// MossCpuThreads controls ONNX Runtime intra-op threads for the MOSS sidecar (0 = auto-detect = physical cores).
	MossCpuThreads int `mapstructure:"tts_moss_cpu_threads"`
}

// FillFrom propagates zero-value fields from defaults into c.
func (c *TTSConfig) FillFrom(defaults TTSConfig) {
	if !c.TTSEnabled && defaults.TTSEnabled {
		c.TTSEnabled = defaults.TTSEnabled
	}
	if c.TTSProvider == "" {
		c.TTSProvider = defaults.TTSProvider
	}
	if c.Voice == "" {
		c.Voice = defaults.Voice
	}
	if c.MaxChars == 0 {
		c.MaxChars = defaults.MaxChars
	}
	if c.MossModelDir == "" {
		c.MossModelDir = defaults.MossModelDir
	}
	if c.MossVoice == "" {
		c.MossVoice = defaults.MossVoice
	}
	if c.MossPort == 0 {
		c.MossPort = defaults.MossPort
	}
	if c.MossIdleTimeout == 0 {
		c.MossIdleTimeout = defaults.MossIdleTimeout
	}
	if c.MossCpuThreads == 0 {
		c.MossCpuThreads = defaults.MossCpuThreads
	}
}

// MessagingPlatformConfig holds settings shared by all messaging adapters (Slack, Feishu, etc.).
type MessagingPlatformConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	WorkerType     string   `mapstructure:"worker_type"`
	WorkDir        string   `mapstructure:"work_dir"`
	DMPolicy       string   `mapstructure:"dm_policy"`
	GroupPolicy    string   `mapstructure:"group_policy"`
	RequireMention bool     `mapstructure:"require_mention"`
	AllowFrom      []string `mapstructure:"allow_from"`
	AllowDMFrom    []string `mapstructure:"allow_dm_from"`
	AllowGroupFrom []string `mapstructure:"allow_group_from"`

	InjectExclude []string `mapstructure:"inject_exclude"` // platform-level default: files to skip from agent config injection
	STTConfig     `mapstructure:",squash"`
	TTSConfig     `mapstructure:",squash"`
}

// MaxBotsPerPlatform is the maximum number of bots allowed per messaging platform.
const MaxBotsPerPlatform = 10

// SlackConfig holds Slack Socket Mode adapter settings.
// Supports single-bot (top-level bot_token/app_token) and multi-bot (bots[]) modes.
// normalizeSlackBots() resolves the two into a unified Bots slice.
type SlackConfig struct {
	MessagingPlatformConfig `mapstructure:",squash"`

	// Single-bot credentials (backward compatible).
	BotToken string `mapstructure:"bot_token"`
	AppToken string `mapstructure:"app_token"`

	SocketMode          bool          `mapstructure:"socket_mode"`
	AssistantAPIEnabled *bool         `mapstructure:"assistant_api_enabled"`
	ReconnectBaseDelay  time.Duration `mapstructure:"reconnect_base_delay"`
	ReconnectMaxDelay   time.Duration `mapstructure:"reconnect_max_delay"`

	// Branding for Assistant status (paid workspaces).
	DisplayName string `mapstructure:"display_name,omitempty"`
	IconEmoji   string `mapstructure:"icon_emoji,omitempty"`

	// Multi-bot configuration. When non-empty, takes precedence over top-level credentials.
	Bots []SlackBotConfig `mapstructure:"bots"`
}

// SlackBotConfig holds credentials and per-bot overrides for a single Slack bot.
type SlackBotConfig struct {
	Name       string `mapstructure:"name"`
	BotToken   string `mapstructure:"bot_token"`
	AppToken   string `mapstructure:"app_token"`
	WorkerType string `mapstructure:"worker_type,omitempty"`
	WorkDir    string `mapstructure:"work_dir,omitempty"`
	Sandbox    string `mapstructure:"sandbox,omitempty"`
	ACPCommand string `mapstructure:"acp_command,omitempty"` // per-bot ACP agent binary override

	// Per-bot access control (falls back to platform-level when empty).
	DMPolicy       string   `mapstructure:"dm_policy,omitempty"`
	GroupPolicy    string   `mapstructure:"group_policy,omitempty"`
	RequireMention *bool    `mapstructure:"require_mention,omitempty"`
	AllowFrom      []string `mapstructure:"allow_from,omitempty"`
	AllowDMFrom    []string `mapstructure:"allow_dm_from,omitempty"`
	AllowGroupFrom []string `mapstructure:"allow_group_from,omitempty"`

	// Per-bot agent config injection override (falls back to platform-level when nil).
	// No omitempty: inject_exclude: [] must produce a non-nil empty slice to
	// explicitly clear the parent-level exclusion (nil = inherit, [] = clear).
	InjectExclude []string `mapstructure:"inject_exclude"`

	// Per-bot branding override (falls back to platform-level when empty).
	DisplayName string `mapstructure:"display_name,omitempty"`
	IconEmoji   string `mapstructure:"icon_emoji,omitempty"`

	STTConfig `mapstructure:",squash"`
	TTSConfig `mapstructure:",squash"`

	// IsSingleBot marks a bot auto-wrapped from platform-level credentials
	// (single-bot mode). Not loaded from YAML/env — set by normalizeSlackBots.
	IsSingleBot bool `mapstructure:"-"`
}

// FeishuConfig holds Feishu WebSocket adapter settings.
// Supports single-bot (top-level app_id/app_secret) and multi-bot (bots[]) modes.
// normalizeFeishuBots() resolves the two into a unified Bots slice.
type FeishuConfig struct {
	MessagingPlatformConfig `mapstructure:",squash"`

	// GatewayRestartAllowFrom is the dedicated host-operations allowlist used
	// for restart authorization and lifecycle notifications. It is intentionally
	// separate from ordinary messaging access control and empty by default.
	GatewayRestartAllowFrom []string `mapstructure:"gateway_restart_allow_from"`

	// Single-bot credentials (backward compatible).
	AppID     string `mapstructure:"app_id"`
	AppSecret string `mapstructure:"app_secret"`

	// Multi-bot configuration. When non-empty, takes precedence over top-level credentials.
	Bots []FeishuBotConfig `mapstructure:"bots"`
}

// FeishuBotConfig holds credentials and per-bot overrides for a single Feishu bot.
type FeishuBotConfig struct {
	Name       string `mapstructure:"name"`
	AppID      string `mapstructure:"app_id"`
	AppSecret  string `mapstructure:"app_secret"`
	WorkerType string `mapstructure:"worker_type,omitempty"`
	WorkDir    string `mapstructure:"work_dir,omitempty"`
	Sandbox    string `mapstructure:"sandbox,omitempty"`
	ACPCommand string `mapstructure:"acp_command,omitempty"` // per-bot ACP agent binary override

	// Per-bot access control (falls back to platform-level when empty).
	DMPolicy       string   `mapstructure:"dm_policy,omitempty"`
	GroupPolicy    string   `mapstructure:"group_policy,omitempty"`
	RequireMention *bool    `mapstructure:"require_mention,omitempty"`
	AllowFrom      []string `mapstructure:"allow_from,omitempty"`
	AllowDMFrom    []string `mapstructure:"allow_dm_from,omitempty"`
	AllowGroupFrom []string `mapstructure:"allow_group_from,omitempty"`

	// GatewayRestartAllowFrom uses nil to inherit the Feishu platform value;
	// an explicit empty slice disables restart and lifecycle notices for this bot.
	GatewayRestartAllowFrom []string `mapstructure:"gateway_restart_allow_from"`

	// Per-bot agent config injection override (falls back to platform-level when nil).
	// No omitempty: inject_exclude: [] must produce a non-nil empty slice to
	// explicitly clear the parent-level exclusion (nil = inherit, [] = clear).
	InjectExclude []string `mapstructure:"inject_exclude"`

	STTConfig `mapstructure:",squash"`
	TTSConfig `mapstructure:",squash"`

	// IsSingleBot marks a bot auto-wrapped from platform-level credentials
	// (single-bot mode). Not loaded from YAML/env — set by normalizeFeishuBots.
	IsSingleBot bool `mapstructure:"-"`
}

// ResolveGatewayRestartAllowFrom resolves the platform/Bot inheritance used by
// Feishu's dedicated Gateway operator policy. A non-nil Bot slice, including
// an explicit empty slice, overrides the platform value.
func ResolveGatewayRestartAllowFrom(platform, bot []string) []string {
	if bot != nil {
		return cloneStrings(bot)
	}
	return cloneStrings(platform)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

// YuanxinConfig holds Yuanxin Pulsar adapter settings.
type YuanxinConfig struct {
	MessagingPlatformConfig `mapstructure:",squash"`

	Tenant        string `mapstructure:"tenant"`
	Namespace     string `mapstructure:"namespace"`
	PulsarURL     string `mapstructure:"pulsar_url"`
	AppID         string `mapstructure:"app_id"`
	ProducerTopic string `mapstructure:"producer_topic"`
}

type AdminConfig struct {
	Enabled            bool                `mapstructure:"enabled"`
	Addr               string              `mapstructure:"addr"`
	Tokens             []string            `mapstructure:"tokens"`
	TokenScopes        map[string][]string `mapstructure:"token_scopes"`
	DefaultScopes      []string            `mapstructure:"default_scopes"`
	IPWhitelistEnabled bool                `mapstructure:"ip_whitelist_enabled"`
	AllowedCIDRs       []string            `mapstructure:"allowed_cidrs"`
	RateLimitEnabled   bool                `mapstructure:"rate_limit_enabled"`
	RequestsPerSec     int                 `mapstructure:"requests_per_sec"`
	Burst              int                 `mapstructure:"burst"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level  string        `mapstructure:"level"`
	Format string        `mapstructure:"format"` // "json" or "text"
	File   LogFileConfig `mapstructure:"file"`
}

// LogFileConfig holds file logging + rotation settings.
// Enabled=false (default) keeps historical behavior: logs go to stderr only.
// When Enabled=true, logs are teed to both the file (rotated via lumberjack)
// and stderr, so daemon-mode stderr redirection and foreground debugging still work.
type LogFileConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	Path       string `mapstructure:"path"`        // empty → default ~/.hotplex/logs/gateway.log
	MaxSize    int    `mapstructure:"max_size"`    // MB per file before rotation; 0 → lumberjack default
	MaxAge     int    `mapstructure:"max_age"`     // days to retain rotated logs
	MaxBackups int    `mapstructure:"max_backups"` // number of rotated backups to keep
	Compress   bool   `mapstructure:"compress"`    // gzip rotated backups
	LocalTime  bool   `mapstructure:"local_time"`  // use local time for rotation filenames
}

// WebChatConfig holds webchat UI serving settings.
type WebChatConfig struct {
	Addr    string `mapstructure:"addr"`    // informational: banner display
	Enabled bool   `mapstructure:"enabled"` // serve embedded webchat SPA from gateway
}

// GatewayConfig holds WebSocket gateway settings.
type GatewayConfig struct {
	Addr            string        `mapstructure:"addr"`
	ReadBufferSize  int           `mapstructure:"read_buffer_size"`
	WriteBufferSize int           `mapstructure:"write_buffer_size"`
	PingInterval    time.Duration `mapstructure:"ping_interval"`
	PongTimeout     time.Duration `mapstructure:"pong_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	// Deprecated: unused — session IDLE GC uses Worker.IdleTimeout (issue #817).
	// Kept for backward compat; a non-default value triggers a startup warning.
	IdleTimeout        time.Duration `mapstructure:"idle_timeout"`
	MaxFrameSize       int64         `mapstructure:"max_frame_size"`
	BroadcastQueueSize int           `mapstructure:"broadcast_queue_size"`

	// PlatformWriteBuffer is the per-conn channel capacity for async platform writes.
	// 64 slots accommodate ~8 batches at 120ms coalesce window, providing ample headroom
	// even under burst conditions without excessive memory overhead.
	PlatformWriteBuffer int `mapstructure:"platform_write_buffer"`
	// PlatformDropThreshold is the fill level at which droppable events (delta/raw)
	// begin being silently dropped. Set to 87.5% of PlatformWriteBuffer to provide
	// backpressure relief while preserving space for guaranteed events.
	PlatformDropThreshold int `mapstructure:"platform_drop_threshold"`
	// DeltaCoalesceInterval is the time window for batching consecutive delta events.
	// 120ms targets Feishu CardKit's 10 updates/sec per-card rate limit (8.3/sec with margin),
	// while keeping first-token latency well under the 200ms human perception threshold.
	// At 100 tok/s input, this yields ~12x API call reduction.
	DeltaCoalesceInterval time.Duration `mapstructure:"delta_coalesce_interval"`
	// DeltaCoalesceSize is the rune threshold for immediate delta flush, serving as a
	// burst safety valve. 200 runes ≈ 40 tokens triggers early flush only during spikes,
	// while average batches at 100 tok/s / 120ms ≈ 48 runes stay well below this threshold.
	DeltaCoalesceSize int `mapstructure:"delta_coalesce_size"`
}

// DBConfig holds database settings.
// Supports both SQLite and PostgreSQL backends.
// For backward compatibility, legacy flat fields (Path, WALMode, etc.) are kept
// on DBConfig. New code should prefer the structured SQLite.* or Postgres.* fields.
type DBConfig struct {
	// Driver specifies the database driver: "sqlite" (default) or "postgres".
	// When set to "postgres", the Postgres sub-config is used instead of SQLite.
	Driver string `mapstructure:"driver"`

	// SQLite-specific configuration. Also the target for legacy flat fields
	// when using the sqlite driver (default).
	SQLite SQLiteConfig `mapstructure:"sqlite"`

	// Postgres-specific configuration. Active when Driver is "postgres".
	Postgres PostgresConfig `mapstructure:"postgres"`

	// ── Legacy flat fields (deprecated, use SQLite.* instead) ──
	Path string `mapstructure:"path"`
	// EventsPath is a legacy config field. Unused since events table lives in hotplex.db.
	// Retained for backward compatibility but no longer normalized (spec #833 §11.2).
	EventsPath        string        `mapstructure:"events_path"`
	WALMode           bool          `mapstructure:"wal_mode"`
	BusyTimeout       time.Duration `mapstructure:"busy_timeout"`
	MaxOpenConns      int           `mapstructure:"max_open_conns"`
	VacuumThreshold   float64       `mapstructure:"vacuum_threshold"`
	CacheSizeKiB      int           `mapstructure:"cache_size_kib"`
	MmapSizeMiB       int           `mapstructure:"mmap_size_mib"`
	WalAutoCheckpoint int           `mapstructure:"wal_autocheckpoint"`
}

// DSN returns the connection string for the configured database driver.
// Delegates to the active sub-config's DSN method based on Driver.
func (d DBConfig) DSN() string {
	switch strings.ToLower(d.Driver) {
	case "postgres", "pg", "postgresql":
		return d.Postgres.DSN()
	default:
		return d.SQLite.DSN()
	}
}

// EffectiveSQLitePath returns the effective SQLite database path,
// preferring the structured SQLite.Path with legacy flat Path as fallback.
func (d DBConfig) EffectiveSQLitePath() string {
	if d.SQLite.Path != "" {
		return d.SQLite.Path
	}
	return d.Path
}

// EffectiveMaxOpenConns returns the effective max open connections,
// preferring the structured SQLite.MaxOpenConns with legacy flat MaxOpenConns as fallback.
func (d DBConfig) EffectiveMaxOpenConns() int {
	if d.SQLite.MaxOpenConns > 0 {
		return d.SQLite.MaxOpenConns
	}
	return d.MaxOpenConns
}

// EffectiveWALMode returns the effective WAL mode setting.
// Note: explicitly setting WALMode=false (zero value) falls through to the legacy field.
// This is acceptable because WALMode=false is not a meaningful config — users who want
// to disable WAL should omit the field and set the legacy wal_mode: false instead.
func (d DBConfig) EffectiveWALMode() bool {
	if d.SQLite.WALMode {
		return true
	}
	return d.WALMode
}

// EffectiveBusyTimeout returns the effective busy timeout for SQLite.
func (d DBConfig) EffectiveBusyTimeout() time.Duration {
	if d.SQLite.BusyTimeout > 0 {
		return d.SQLite.BusyTimeout
	}
	return d.BusyTimeout
}

// EffectiveCacheSizeKiB returns the effective SQLite cache size in KiB.
func (d DBConfig) EffectiveCacheSizeKiB() int {
	if d.SQLite.CacheSizeKiB > 0 {
		return d.SQLite.CacheSizeKiB
	}
	return d.CacheSizeKiB
}

// EffectiveMmapSizeMiB returns the effective SQLite mmap size in MiB.
func (d DBConfig) EffectiveMmapSizeMiB() int {
	if d.SQLite.MmapSizeMiB > 0 {
		return d.SQLite.MmapSizeMiB
	}
	return d.MmapSizeMiB
}

// EffectiveWalAutoCheckpoint returns the effective WAL auto-checkpoint threshold.
func (d DBConfig) EffectiveWalAutoCheckpoint() int {
	if d.SQLite.WalAutoCheckpoint > 0 {
		return d.SQLite.WalAutoCheckpoint
	}
	return d.WalAutoCheckpoint
}

// SQLiteConfig holds SQLite-specific database settings.
type SQLiteConfig struct {
	Path              string        `mapstructure:"path"`
	WALMode           bool          `mapstructure:"wal_mode"`
	BusyTimeout       time.Duration `mapstructure:"busy_timeout"`
	MaxOpenConns      int           `mapstructure:"max_open_conns"`
	VacuumThreshold   float64       `mapstructure:"vacuum_threshold"`
	CacheSizeKiB      int           `mapstructure:"cache_size_kib"`
	MmapSizeMiB       int           `mapstructure:"mmap_size_mib"`
	WalAutoCheckpoint int           `mapstructure:"wal_autocheckpoint"`
}

// DSN returns the SQLite database path. Defaults to ":memory:" when empty.
func (s SQLiteConfig) DSN() string {
	if s.Path == "" {
		return ":memory:"
	}
	return s.Path
}

// PostgresConfig holds PostgreSQL-specific database settings.
type PostgresConfig struct {
	ConnStr      string `mapstructure:"dsn"`
	MaxOpenConns int    `mapstructure:"max_open_conns"`
}

// DSN returns the PostgreSQL connection string. Returns empty when ConnStr is not configured.
func (p PostgresConfig) DSN() string {
	return p.ConnStr
}

// WorkerConfig holds per-worker defaults.
type WorkerConfig struct {
	MaxLifetime           time.Duration             `mapstructure:"max_lifetime"`
	IdleTimeout           time.Duration             `mapstructure:"idle_timeout"`
	ExecutionTimeout      time.Duration             `mapstructure:"execution_timeout"`
	TurnTimeout           time.Duration             `mapstructure:"turn_timeout"`
	EnvBlocklist          []string                  `mapstructure:"env_blocklist"`
	DefaultWorkDir        string                    `mapstructure:"default_work_dir"`
	PIDDir                string                    `mapstructure:"pid_dir"`
	AutoRetry             AutoRetryConfig           `mapstructure:"auto_retry"`
	OpenCodeServer        OpenCodeServerConfig      `mapstructure:"opencode_server"`
	ClaudeCode            ClaudeCodeConfig          `mapstructure:"claude_code"`
	CodexCLI              CodexCLIConfig            `mapstructure:"codex_cli"`
	ACP                   ACPConfig                 `mapstructure:"acp"`
	Environment           []string                  `mapstructure:"environment"`
	DefaultPermissionMode string                    `mapstructure:"default_permission_mode"` // r3 (#804): bridge injects this for workspaces with no explicit override; seeded "workspace" by Default()
	PermissionDenyDedup   PermissionDenyDedupConfig `mapstructure:"permission_deny_dedup"`
	RuntimePlan           RuntimePlanConfig         `mapstructure:"runtime_plan"`
}

// RuntimePlanConfig controls how far the EffectiveRuntimePlan is trusted at
// launch (#946 plan unit D2). The default is SHADOW: the plan is resolved,
// bound to the Worker run and compared against the legacy parameters, but
// nothing it says changes what actually launches.
//
// Mode is deliberately two-valued. "authoritative" is not a global switch —
// it applies ONLY to the entry×Worker combinations named in AuthoritativeEntries
// and AuthoritativeWorkers, and every other combination stays explicitly in
// shadow. A partially rolled-out rollout must report itself as partial, never
// as fully live.
type RuntimePlanConfig struct {
	// Mode is "shadow" (default) or "authoritative".
	Mode string `mapstructure:"mode"`
	// AuthoritativeEntries lists entry kinds allowed to launch from the plan:
	// "webchat", "messaging", "cron". Empty = none, so "authoritative" alone
	// never turns the plan live by accident.
	AuthoritativeEntries []string `mapstructure:"authoritative_entries"`
	// AuthoritativeWorkers lists worker types allowed to launch from the plan:
	// "claude_code", "codex_cli", "opencode_server", "acp".
	AuthoritativeWorkers []string `mapstructure:"authoritative_workers"`
}

// NormalizePlanRolloutMode maps an absent or unrecognized mode onto the safe
// default. An unknown value must never be read as "authoritative".
func (c RuntimePlanConfig) NormalizePlanRolloutMode() string {
	if c.Mode == RuntimePlanModeAuthoritative {
		return RuntimePlanModeAuthoritative
	}
	return RuntimePlanModeShadow
}

// Runtime plan rollout modes.
const (
	RuntimePlanModeShadow        = "shadow"
	RuntimePlanModeAuthoritative = "authoritative"
)

// PermissionDenyDedupConfig controls suppression of repeated permission cards
// after a user denial. Within Window after a deny, the same owner+fingerprint is
// auto-denied locally without forwarding a new card to the client — closes the
// "deny → agent retries the same tool seconds later → new card" loop. See
// docs/specs/Permission-Deny-Dedup-Spec.md.
type PermissionDenyDedupConfig struct {
	Enabled bool          `mapstructure:"enabled"`
	Window  time.Duration `mapstructure:"window"`
}

// MCPServerConfig defines a single MCP server for worker startup.
type MCPServerConfig struct {
	Command string            `mapstructure:"command" json:"command"`
	Args    []string          `mapstructure:"args" json:"args,omitempty"`
	Env     map[string]string `mapstructure:"env" json:"env,omitempty"`
	URL     string            `mapstructure:"url" json:"url,omitempty"`
}

// Validate checks that the server config specifies exactly one of Command (stdio) or URL (remote).
func (c *MCPServerConfig) Validate() error {
	if c.Command == "" && c.URL == "" {
		return fmt.Errorf("mcp server config: command or url required")
	}
	if c.Command != "" && c.URL != "" {
		return fmt.Errorf("mcp server config: command and url are mutually exclusive")
	}
	return nil
}

// ClaudeCodeConfig holds Claude Code worker startup settings.
type ClaudeCodeConfig struct {
	Command               string                      `mapstructure:"command"`                 // binary + optional subcommand, e.g. "claude" or "ccr code"
	PermissionMode        string                      `mapstructure:"permission_mode"`         // operator default PermissionMode tier for sessions with no explicit override (platform/cron: bridge injects ""); "" = bypass. Valid: read-only|workspace|auto-edit|bypass. Counterpart to codex_cli.sandbox+approval_mode / acp.auto_approve.
	PermissionPrompt      bool                        `mapstructure:"permission_prompt"`       // enable --permission-prompt-tool stdio for interaction chain
	PermissionAutoApprove []string                    `mapstructure:"permission_auto_approve"` // tool names to auto-approve without user interaction
	MCPServers            map[string]*MCPServerConfig `mapstructure:"mcp_servers"`             // user-configured MCP servers; empty = default discovery
}

// CodexCLIConfig holds Codex CLI worker startup settings.
type CodexCLIConfig struct {
	Command          string        `mapstructure:"command"`             // codex binary path, default "codex"
	Model            string        `mapstructure:"model"`               // model name, empty = use Codex default
	Sandbox          string        `mapstructure:"sandbox"`             // sandbox mode, default "danger-full-access" (YOLO: full filesystem + network)
	ApprovalMode     string        `mapstructure:"approval_mode"`       // approval mode, default "never" (YOLO: no approval prompts)
	Ephemeral        bool          `mapstructure:"ephemeral"`           // ephemeral sessions, default true
	Personality      string        `mapstructure:"personality"`         // agent personality for app-server mode, default "friendly"
	StartupTimeout   time.Duration `mapstructure:"startup_timeout"`     // process startup timeout, default 30s
	CallTimeout      time.Duration `mapstructure:"call_timeout"`        // JSON-RPC call timeout, default 30s
	UseAppServer     bool          `mapstructure:"use_app_server"`      // deprecated: always true, app-server is the only mode
	IdleDrainPeriod  time.Duration `mapstructure:"idle_drain_period"`   // idle drain timeout for app-server mode, default 30m
	Color            bool          `mapstructure:"color"`               // colored output (--color)
	OutputFile       string        `mapstructure:"output_file"`         // output-only-last-message mode (--output-last-message)
	StrictConfig     bool          `mapstructure:"strict_config"`       // strict config validation (--strict-config)
	SkipGitRepoCheck bool          `mapstructure:"skip_git_repo_check"` // bypass git repo check (--skip-git-repo-check)
	IgnoreUserConfig bool          `mapstructure:"ignore_user_config"`  // ignore user-level config (--ignore-user-config)
	IgnoreRules      bool          `mapstructure:"ignore_rules"`        // ignore project rules (--ignore-rules)
	LocalProvider    bool          `mapstructure:"local_provider"`      // force local model provider (--local-provider)
	ConfigProfile    string        `mapstructure:"config_profile"`      // codex config profile (--profile)
	BypassHookTrust  bool          `mapstructure:"bypass_hook_trust"`   // bypass hook trust (--dangerously-bypass-hook-trust)
}

// OpenCodeServerConfig holds OpenCode Server singleton process settings.
type OpenCodeServerConfig struct {
	Command           string        `mapstructure:"command"` // binary + optional subcommand, e.g. "opencode" or "opencode serve"
	Password          string        `mapstructure:"password"`
	IdleDrainPeriod   time.Duration `mapstructure:"idle_drain_period"`
	ReadyTimeout      time.Duration `mapstructure:"ready_timeout"`
	ReadyPollInterval time.Duration `mapstructure:"ready_poll_interval"`
	HTTPTimeout       time.Duration `mapstructure:"http_timeout"`
	ContextWindow     int64         `mapstructure:"context_window"` // fallback context window size; OCS HTTP API does not expose the real value
}

// ACPConfig holds ACP (Agent Client Protocol) worker settings.
// ACP is a universal worker type that connects to any ACP-compatible agent via stdio.
type ACPConfig struct {
	Command     string   `mapstructure:"command" json:"command"`                               // ACP agent binary (e.g. "hermes-acp")
	AutoApprove *bool    `mapstructure:"auto_approve,omitempty" json:"auto_approve,omitempty"` // auto-approve permission requests
	Args        []string `mapstructure:"args,omitempty" json:"args,omitempty"`                 // extra args appended after the command
	Debug       bool     `mapstructure:"debug,omitempty" json:"debug,omitempty"`               // enable JSON-RPC protocol trace logging
}

// AutoRetryConfig controls automatic retry behavior when LLM provider returns
// temporary errors (429 rate limit, 529 overload, 400 bad request, etc.).
type AutoRetryConfig struct {
	Enabled    bool          `mapstructure:"enabled"`
	MaxRetries int           `mapstructure:"max_retries"`
	BaseDelay  time.Duration `mapstructure:"base_delay"`
	MaxDelay   time.Duration `mapstructure:"max_delay"`
	RetryInput string        `mapstructure:"retry_input"`
	NotifyUser bool          `mapstructure:"notify_user"`
	Patterns   []string      `mapstructure:"patterns"`
}

// Defaults applies sensible defaults to AutoRetryConfig and returns the updated struct.
func (c AutoRetryConfig) Defaults() AutoRetryConfig {
	if c.MaxRetries <= 0 {
		c.MaxRetries = 9
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 5 * time.Second
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 120 * time.Second
	}
	if c.RetryInput == "" {
		c.RetryInput = "继续"
	}
	return c
}

// SecurityConfig holds auth and input validation settings.
type SecurityConfig struct {
	APIKeyHeader   string   `mapstructure:"api_key_header"`
	APIKeys        []string `mapstructure:"api_keys"`
	CookieSecret   string   `mapstructure:"cookie_secret"`
	TLSEnabled     bool     `mapstructure:"tls_enabled"`
	TLSCertFile    string   `mapstructure:"tls_cert_file"`
	TLSKeyFile     string   `mapstructure:"tls_key_file"`
	AllowedOrigins []string `mapstructure:"allowed_origins"`

	// APIKeyUsers maps environment variable names (or literal key values) to user IDs.
	// Enterprise multi-user: each API key gets a distinct identity for session isolation.
	// Keys not present in this map default to "api_user" (backward compatible).
	APIKeyUsers map[string]string `mapstructure:"api_key_users"`

	// WorkDir security settings
	WorkDirAllowedBasePatterns []string `mapstructure:"work_dir_allowed_base_patterns"` // extra whitelist patterns (supports ~ and ${VAR})
	WorkDirForbiddenDirs       []string `mapstructure:"work_dir_forbidden_dirs"`        // extra blacklist directories

	// CSP overrides the Content-Security-Policy header served with the embedded
	// webchat SPA and docs portal. Empty string keeps the package-level default
	// (localhost-friendly). Set this when serving from a remote host — the
	// browser blocks fetch/ws/connect calls that do not match a directive in CSP.
	CSP string `mapstructure:"csp"`

	// SecurityContact enables /.well-known/security.txt (RFC 9116) when non-empty.
	// Examples: "mailto:security@example.com", "https://example.com/security".
	// Overridable via HOTPLEX_SECURITY_SECURITY_CONTACT env var.
	SecurityContact string `mapstructure:"security_contact"`

	// CookieSameSite controls the SameSite attribute of the session cookie.
	// Valid values: "lax", "strict", "none", "default" (case-insensitive).
	// Defaults to "lax" to preserve same-origin webchat compatibility;
	// cross-origin HTTPS deployments should set to "none".
	CookieSameSite string `mapstructure:"cookie_same_site"`
}

// SessionConfig holds session lifecycle settings.
type SessionConfig struct {
	RetentionPeriod   time.Duration `mapstructure:"retention_period"`    // max session lifetime (default 7d)
	GCScanInterval    time.Duration `mapstructure:"gc_scan_interval"`    // GC scan interval (default 1m)
	MaxConcurrent     int           `mapstructure:"max_concurrent"`      // max concurrent sessions
	TermRetention     time.Duration `mapstructure:"term_retention"`      // DB retention for terminated sessions (default 7d)
	CronTermRetention time.Duration `mapstructure:"cron_term_retention"` // DB retention for terminated cron sessions (default 24h)
}

// PoolConfig holds session pool settings.
type PoolConfig struct {
	MinSize          int   `mapstructure:"min_size"`
	MaxSize          int   `mapstructure:"max_size"`
	MaxIdlePerUser   int   `mapstructure:"max_idle_per_user"`
	MaxMemoryPerUser int64 `mapstructure:"max_memory_per_user"` // bytes; 0 = unlimited
	MaxPerWorkspace  int   `mapstructure:"max_per_workspace"`   // WebChat per-workspace concurrency (spec ①); 0 = unlimited
}

type AgentConfig struct {
	Enabled       bool     `mapstructure:"enabled"`        // enable agent config loading
	ConfigDir     string   `mapstructure:"config_dir"`     // default: ~/.hotplex/agent-configs/
	InjectExclude []string `mapstructure:"inject_exclude"` // global default: files to skip from agent config injection
}

// SkillsConfig holds skill discovery and caching settings.
type SkillsConfig struct {
	CacheTTL time.Duration `mapstructure:"cache_ttl"` // TTL for skills list cache, default 5m
}

// WebhookConfig holds GitHub webhook receiver settings.
type WebhookConfig struct {
	MaxBodySize   int64    `mapstructure:"max_body_size"`   // default: 1MB
	AllowedRepos  []string `mapstructure:"allowed_repos"`   // repos to accept events from; empty = accept all (env override not supported for slices)
	TargetJobName string   `mapstructure:"target_job_name"` // cron job to trigger on matching events
	Secret        string   `mapstructure:"secret"`
	Path          string   `mapstructure:"path"` // default: "/api/webhook/github"
	Enabled       bool     `mapstructure:"enabled"`
}

// CronConfig holds AI-native cronjob scheduler settings.
type CronConfig struct {
	Enabled           bool             `mapstructure:"enabled"`
	MaxConcurrentRuns int              `mapstructure:"max_concurrent_runs"` // default 3
	MaxJobs           int              `mapstructure:"max_jobs"`            // default 50
	DefaultTimeoutSec int              `mapstructure:"default_timeout_sec"` // default 300
	TickIntervalSec   int              `mapstructure:"tick_interval_sec"`   // default 60
	YAMLConfigPath    string           `mapstructure:"yaml_config_path"`    // optional external YAML
	Jobs              []map[string]any `mapstructure:"jobs"`                // inline job definitions
}

// EventsConfig holds event and turn retention settings.
type EventsConfig struct {
	Retention time.Duration `mapstructure:"retention"` // TTL for events + turns, default 720h (30 days)
}

// AuditConfig holds user behavior audit system settings.
type AuditConfig struct {
	Enabled              bool                 `mapstructure:"enabled"`
	Retention            time.Duration        `mapstructure:"retention"`
	FullContentRetention time.Duration        `mapstructure:"full_content_retention"` // Compatibility field (default 90d); does not extend events/turns retention.
	Collector            AuditCollectorConfig `mapstructure:"collector"`
	Sinks                []AuditSinkConfig    `mapstructure:"sinks"`
}

// EffectiveEventsRetention computes the events/turns TTL to use at startup.
// Audit keeps its plaintext independently, so full_content_retention never
// extends this short-lived operational copy. A zero events.retention falls back
// to the package default (720h).
func EffectiveEventsRetention(cfg *Config) time.Duration {
	if cfg == nil {
		return 720 * time.Hour
	}
	ev := cfg.Events.Retention
	if ev <= 0 {
		ev = 720 * time.Hour // 30 days, matches Default()
	}
	return ev
}

// AuditCollectorConfig holds audit event collector tuning parameters.
type AuditCollectorConfig struct {
	ChannelCap    int           `mapstructure:"channel_cap"`
	BatchInterval time.Duration `mapstructure:"batch_interval"`
	BatchSize     int           `mapstructure:"batch_size"`
	SpillDir      string        `mapstructure:"spill_dir"`
}

// AuditSinkConfig defines a single audit event sink.
type AuditSinkConfig struct {
	Name   string         `mapstructure:"name"`
	Type   string         `mapstructure:"type"`
	Config map[string]any `mapstructure:"config"`
}
