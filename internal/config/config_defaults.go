package config

import (
	"path/filepath"
	"time"
)

// ─── Defaults ────────────────────────────────────────────────────────────────

// Default returns a Config with sensible production defaults.
// All non-sensitive fields have values — the binary runs with zero config.
func Default() *Config {
	return &Config{
		Gateway: GatewayConfig{
			Addr:                  "localhost:8888",
			ReadBufferSize:        4096,
			WriteBufferSize:       4096,
			PingInterval:          54 * time.Second,
			PongTimeout:           60 * time.Second,
			WriteTimeout:          10 * time.Second,
			IdleTimeout:           5 * time.Minute,
			MaxFrameSize:          32 * 1024,
			BroadcastQueueSize:    256,
			PlatformWriteBuffer:   64,
			PlatformDropThreshold: 56,
			DeltaCoalesceInterval: 120 * time.Millisecond,
			DeltaCoalesceSize:     200,
		},
		DB: DBConfig{
			Driver: "sqlite",
			SQLite: SQLiteConfig{
				Path:              filepath.Join(HotplexHome(), "data", "hotplex.db"),
				WALMode:           true,
				BusyTimeout:       5 * time.Second,
				MaxOpenConns:      3, // 1 writer + 2 readers for shared session/event store
				VacuumThreshold:   0.2,
				CacheSizeKiB:      8192,
				MmapSizeMiB:       64,
				WalAutoCheckpoint: 2000,
			},
			// Legacy flat fields (kept for backward compat with existing consumers).
			Path:              filepath.Join(HotplexHome(), "data", "hotplex.db"),
			EventsPath:        "", // Deprecated: unused, events table lives in hotplex.db
			WALMode:           true,
			BusyTimeout:       5 * time.Second,
			MaxOpenConns:      3, // 1 writer + 2 readers for shared session/event store
			VacuumThreshold:   0.2,
			CacheSizeKiB:      8192,
			MmapSizeMiB:       64,
			WalAutoCheckpoint: 2000,
		},
		Worker: WorkerConfig{
			MaxLifetime:      24 * time.Hour,
			IdleTimeout:      60 * time.Minute,
			ExecutionTimeout: 30 * time.Minute,
			TurnTimeout:      0, // disabled by default; execution_timeout catches zombies
			EnvBlocklist:     nil,
			DefaultWorkDir:   filepath.Join(HotplexHome(), "workspace"),
			PIDDir:           filepath.Join(HotplexHome(), ".pids"),
			AutoRetry:        AutoRetryConfig{Enabled: true, MaxRetries: 9, BaseDelay: 5 * time.Second, MaxDelay: 120 * time.Second, RetryInput: "继续", NotifyUser: true},
			OpenCodeServer: OpenCodeServerConfig{
				Command:           "opencode",
				IdleDrainPeriod:   30 * time.Minute,
				ReadyTimeout:      10 * time.Second,
				ReadyPollInterval: 200 * time.Millisecond,
				HTTPTimeout:       30 * time.Second,
				ContextWindow:     200000, // OCS HTTP API does not expose model context window; configurable fallback (spec §5 O1)
			},
			ClaudeCode: ClaudeCodeConfig{
				Command: "claude",
			},
			CodexCLI: CodexCLIConfig{
				Command:         "codex",
				Sandbox:         "danger-full-access",
				ApprovalMode:    "never",
				Ephemeral:       true,
				Personality:     "friendly",
				StartupTimeout:  30 * time.Second,
				CallTimeout:     30 * time.Second,
				UseAppServer:    true,
				IdleDrainPeriod: 30 * time.Minute,
			},
			DefaultPermissionMode: "workspace", // r3 (#804): bridge injects this when a workspace has no explicit override; default tightened from bypass→workspace so injection is a blast-radius reduction, not an escalation
			PermissionDenyDedup: PermissionDenyDedupConfig{
				Enabled: true,
				Window:  60 * time.Second,
			},
			// Shadow is the only safe default: the plan is resolved, bound to
			// the Worker run and compared against the legacy parameters, but
			// nothing it says changes what actually launches. Both allowlists
			// stay EMPTY, so even an "authoritative" mode line cannot turn the
			// plan live for an unnamed entry or Worker.
			RuntimePlan: RuntimePlanConfig{
				Mode:                 RuntimePlanModeShadow,
				AuthoritativeEntries: nil,
				AuthoritativeWorkers: nil,
			},
		},
		// The queue is OFF by default. Accepting into a queue whose dispatch
		// path is not wired would accumulate inputs nobody ever sends, which is
		// worse than refusing the input outright.
		Execution: ExecutionConfig{
			Queue: ExecutionQueueConfig{
				Enabled:         false,
				PerSession:      20,
				Global:          1000,
				MaxPayloadBytes: 64 << 10,
				TTL:             24 * time.Hour,
				SweepInterval:   time.Minute,
				SweepBatch:      100,
			},
		},
		Security: SecurityConfig{
			APIKeyHeader:    "X-API-Key",
			APIKeys:         nil,
			CookieSecret:    "",
			TLSEnabled:      false,
			AllowedOrigins:  []string{"*"},
			CSP:             "", // empty → webchat/docs use package-level default
			SecurityContact: "",
			CookieSameSite:  "lax",
		},
		Session: SessionConfig{
			RetentionPeriod:   7 * 24 * time.Hour,
			GCScanInterval:    1 * time.Minute,
			MaxConcurrent:     1000,
			TermRetention:     7 * 24 * time.Hour,
			CronTermRetention: 24 * time.Hour,
		},
		Pool: PoolConfig{
			MinSize:          0,
			MaxSize:          100,
			MaxIdlePerUser:   5,
			MaxMemoryPerUser: 3 << 30, // 3 GB
			MaxPerWorkspace:  0,       // disabled by default; WebChat deployments configure per-workspace concurrency
		},
		Admin: AdminConfig{
			Enabled:            true,
			Addr:               "localhost:9999",
			Tokens:             nil,
			TokenScopes:        nil,
			DefaultScopes:      []string{"session:read", "session:write", "session:delete", "stats:read", "health:read", "admin:write"},
			IPWhitelistEnabled: false,
			AllowedCIDRs:       []string{"127.0.0.0/8", "10.0.0.0/8"},
			RateLimitEnabled:   true,
			RequestsPerSec:     10,
			Burst:              20,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
			File: LogFileConfig{
				Enabled:    false, // default off: stderr-only (historical behavior)
				MaxSize:    10,
				MaxAge:     30,
				MaxBackups: 100,
				Compress:   true,
				LocalTime:  true,
			},
		},
		WebChat: WebChatConfig{
			Addr:    "",
			Enabled: true,
		},
		Messaging: MessagingConfig{
			TurnSummaryEnabled: true,
			// Shared defaults — propagated to platforms via propagateMessagingDefaults().
			WorkerType: DefaultWorkerType,
			STTConfig: STTConfig{
				Provider:     "local",
				LocalCmd:     "python3 " + filepath.Join(HotplexHome(), "scripts", "stt_server.py"),
				LocalIdleTTL: time.Hour,
			},
			TTSConfig: TTSConfig{
				TTSEnabled:      true,
				TTSProvider:     "edge+moss",
				Voice:           "zh-CN-XiaoxiaoNeural",
				MaxChars:        150,
				MossModelDir:    filepath.Join(HotplexHome(), "models", "moss-tts-nano"),
				MossVoice:       "Xiaoyu",
				MossPort:        18083,
				MossIdleTimeout: 30 * time.Minute,
				MossCpuThreads:  0,
			},
			Feishu: FeishuConfig{
				MessagingPlatformConfig: defaultMessagingPlatformConfig(),
			},
			Slack: SlackConfig{
				MessagingPlatformConfig: defaultMessagingPlatformConfig(),
			},
			Yuanxin: YuanxinConfig{
				MessagingPlatformConfig: defaultMessagingPlatformConfig(),
			},
		},
		AgentConfig: AgentConfig{
			Enabled:   true,
			ConfigDir: filepath.Join(HotplexHome(), "agent-configs"),
		},
		Skills: SkillsConfig{
			CacheTTL: 5 * time.Minute,
		},
		Cron: CronConfig{
			Enabled:           true,
			MaxConcurrentRuns: 3,
			MaxJobs:           50,
			DefaultTimeoutSec: 300,
			TickIntervalSec:   60,
		},
		Webhook: WebhookConfig{
			MaxBodySize:   1 << 20, // 1MB
			Path:          "/api/webhook/github",
			TargetJobName: "pr-review-hotplex",
			Enabled:       false,
		},
		Events: EventsConfig{
			Retention: 720 * time.Hour, // 30 days
		},
		Audit: AuditConfig{
			Enabled:              true,
			Retention:            26280 * time.Hour, // 3 years
			FullContentRetention: 2160 * time.Hour,  // 90 days; compatibility field, not used by events/turns GC
			Collector: AuditCollectorConfig{
				ChannelCap:    4096,
				BatchInterval: 1 * time.Second,
				BatchSize:     100,
				SpillDir:      filepath.Join(HotplexHome(), "data", "audit-spill"),
			},
			Sinks: []AuditSinkConfig{
				{Name: "noop", Type: "noop", Config: map[string]any{}},
			},
		},
	}
}

func defaultMessagingPlatformConfig() MessagingPlatformConfig {
	return MessagingPlatformConfig{
		RequireMention: true,
		DMPolicy:       "allowlist",
		GroupPolicy:    "allowlist",
	}
}

// propagateMessagingDefaults copies shared fields from MessagingConfig to each
// platform config. Only zero-value fields on the platform side are filled;
// existing values are never overwritten.
//
// Priority: platform-level (YAML / env) > messaging-level > Default().
func propagateMessagingDefaults(cfg *Config) {
	msg := &cfg.Messaging
	propagatePlatform(&msg.Slack.MessagingPlatformConfig, msg)
	propagatePlatform(&msg.Feishu.MessagingPlatformConfig, msg)
	propagatePlatform(&msg.Yuanxin.MessagingPlatformConfig, msg)

	// Propagate shared defaults into per-bot configs.
	for i := range msg.Slack.Bots {
		propagateBotDefaults(&msg.Slack.MessagingPlatformConfig, &msg.Slack.Bots[i].STTConfig, &msg.Slack.Bots[i].TTSConfig)
	}
	for i := range msg.Feishu.Bots {
		propagateBotDefaults(&msg.Feishu.MessagingPlatformConfig, &msg.Feishu.Bots[i].STTConfig, &msg.Feishu.Bots[i].TTSConfig)
	}
}

// propagatePlatform fills zero-value fields on the platform from the messaging-level shared config.
func propagatePlatform(p *MessagingPlatformConfig, msg *MessagingConfig) {
	if p.WorkerType == "" {
		p.WorkerType = msg.WorkerType
	}
	p.STTConfig.FillFrom(msg.STTConfig)
	p.TTSConfig.FillFrom(msg.TTSConfig)
}

// normalizeSlackBots resolves SlackConfig to a unified Bots slice.
// If Bots is already populated, it takes precedence.
// If Bots is empty but top-level BotToken is set, auto-wraps as a single bot
// named "slack" with IsSingleBot=true. The flag tells bot_config_adapter to
// keep targeting the platform-level agent-config directory (dir/slack/),
// preserving single-bot mode semantics, while giving the bot a non-empty name
// for display, routing, and registry lookup.
func normalizeSlackBots(cfg *SlackConfig) {
	if len(cfg.Bots) > 0 {
		return
	}
	if cfg.BotToken == "" {
		return
	}
	cfg.Bots = []SlackBotConfig{
		{Name: "slack", BotToken: cfg.BotToken, AppToken: cfg.AppToken, IsSingleBot: true},
	}
}

// normalizeFeishuBots resolves FeishuConfig to a unified Bots slice.
// Same backward-compat logic as normalizeSlackBots: auto-wraps platform-level
// credentials as a single bot named "feishu" with IsSingleBot=true.
func normalizeFeishuBots(cfg *FeishuConfig) {
	if len(cfg.Bots) > 0 {
		return
	}
	if cfg.AppID == "" {
		return
	}
	cfg.Bots = []FeishuBotConfig{
		{Name: "feishu", AppID: cfg.AppID, AppSecret: cfg.AppSecret, IsSingleBot: true},
	}
}

// propagateBotDefaults fills zero-value fields on each bot config from the
// platform-level MessagingPlatformConfig and messaging-level shared config.
func propagateBotDefaults(platformCfg *MessagingPlatformConfig, botSTT *STTConfig, botTTS *TTSConfig) {
	botSTT.FillFrom(platformCfg.STTConfig)
	botTTS.FillFrom(platformCfg.TTSConfig)
}

// expandStringFields expands env vars in non-empty string fields.
func expandStringFields(fields ...*string) {
	for _, f := range fields {
		if *f != "" {
			*f = ExpandEnv(*f)
		}
	}
}

// normalizePathFields resolves ~ and normalizes paths for non-empty string fields.
func normalizePathFields(fields ...*string) {
	for _, f := range fields {
		if *f != "" {
			if absPath, err := ExpandAndAbs(*f); err == nil {
				*f = absPath
			}
		}
	}
}
