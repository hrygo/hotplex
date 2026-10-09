package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

// ─── Loading ─────────────────────────────────────────────────────────────────

// ErrConfigCycle is returned when a config inheritance chain contains a cycle.
var ErrConfigCycle = errors.New("config: inheritance cycle detected")

// Load reads configuration from the given file path, then applies defaults
// and environment overrides.  Configuration strategy: convention over configuration.
//
// Load order (later overrides earlier):
//  1. Sensible defaults (Default())
//  2. Parent config file (via inherits field), recursively, with cycle detection
//  3. Config file (YAML/JSON/TOML) — canonical source for non-sensitive values
//  4. Environment variables (HOTPLEX_*)
//
// If filePath is empty, only defaults + environment are used.
func Load(filePath string) (*Config, error) {
	cfg, err := loadRecursive(filePath, nil)
	if err != nil {
		return nil, err
	}

	// Environment variable overrides (e.g. HOTPLEX_LOG_FORMAT=text)
	v := viper.New()
	v.SetEnvPrefix("HOTPLEX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Explicitly bind common keys to ensure Unmarshal picks up environment variables.
	// Viper's AutomaticEnv only works during Unmarshal if keys are known.
	_ = v.BindEnv("log.level")
	_ = v.BindEnv("log.format")
	_ = v.BindEnv("log.file.enabled")
	_ = v.BindEnv("log.file.path")
	_ = v.BindEnv("log.file.max_size")
	_ = v.BindEnv("log.file.max_age")
	_ = v.BindEnv("log.file.max_backups")
	_ = v.BindEnv("log.file.compress")
	_ = v.BindEnv("log.file.local_time")
	_ = v.BindEnv("db.path")
	_ = v.BindEnv("db.wal_mode")
	_ = v.BindEnv("db.driver")
	_ = v.BindEnv("db.postgres.dsn")
	_ = v.BindEnv("db.postgres.max_open_conns")
	_ = v.BindEnv("gateway.addr")
	_ = v.BindEnv("admin.enabled")
	_ = v.BindEnv("admin.addr")
	_ = v.BindEnv("session.max_concurrent")
	_ = v.BindEnv("session.retention_period")
	_ = v.BindEnv("session.term_retention")
	_ = v.BindEnv("session.cron_term_retention")
	_ = v.BindEnv("pool.max_size")
	_ = v.BindEnv("pool.max_idle_per_user")
	_ = v.BindEnv("pool.max_memory_per_user")
	_ = v.BindEnv("pool.max_per_workspace")
	_ = v.BindEnv("worker.default_work_dir")
	_ = v.BindEnv("worker.max_lifetime")
	_ = v.BindEnv("worker.idle_timeout")
	_ = v.BindEnv("worker.execution_timeout")
	_ = v.BindEnv("worker.turn_timeout")
	_ = v.BindEnv("worker.auto_retry.enabled")
	_ = v.BindEnv("worker.auto_retry.max_retries")
	_ = v.BindEnv("worker.claude_code.command")
	_ = v.BindEnv("worker.claude_code.permission_mode")
	_ = v.BindEnv("worker.permission_deny_dedup.enabled")
	_ = v.BindEnv("worker.permission_deny_dedup.window")
	_ = v.BindEnv("worker.codex_cli.command")
	_ = v.BindEnv("worker.codex_cli.model")
	_ = v.BindEnv("worker.codex_cli.sandbox")
	_ = v.BindEnv("worker.codex_cli.approval_mode")
	_ = v.BindEnv("worker.acp.command")
	_ = v.BindEnv("worker.acp.auto_approve")
	_ = v.BindEnv("worker.acp.args")
	_ = v.BindEnv("worker.acp.debug")
	_ = v.BindEnv("worker.opencode_server.command")
	_ = v.BindEnv("worker.opencode_server.idle_drain_period")
	_ = v.BindEnv("worker.opencode_server.ready_timeout")
	_ = v.BindEnv("worker.opencode_server.ready_poll_interval")
	_ = v.BindEnv("worker.opencode_server.http_timeout")
	_ = v.BindEnv("worker.opencode_server.password")
	_ = v.BindEnv("worker.opencode_server.context_window")
	_ = v.BindEnv("security.api_key_header")
	_ = v.BindEnv("security.cookie_secret")
	_ = v.BindEnv("security.csp")
	_ = v.BindEnv("security.allowed_origins")
	_ = v.BindEnv("security.security_contact")
	_ = v.BindEnv("agent_config.enabled")
	_ = v.BindEnv("agent_config.config_dir")
	_ = v.BindEnv("skills.cache_ttl")
	_ = v.BindEnv("messaging.slack.work_dir")
	_ = v.BindEnv("messaging.slack.stt_provider")
	_ = v.BindEnv("messaging.slack.stt_local_cmd")
	_ = v.BindEnv("messaging.slack.stt_local_idle_ttl")
	_ = v.BindEnv("messaging.feishu.work_dir")
	_ = v.BindEnv("messaging.feishu.stt_provider")
	_ = v.BindEnv("messaging.feishu.stt_local_cmd")
	_ = v.BindEnv("messaging.feishu.stt_local_idle_ttl")
	_ = v.BindEnv("messaging.feishu.tts_enabled")
	_ = v.BindEnv("messaging.feishu.tts_provider")
	_ = v.BindEnv("messaging.feishu.tts_voice")
	_ = v.BindEnv("messaging.feishu.tts_max_chars")
	_ = v.BindEnv("messaging.feishu.tts_moss_model_dir")
	_ = v.BindEnv("messaging.feishu.tts_moss_voice")
	_ = v.BindEnv("messaging.feishu.tts_moss_port")
	_ = v.BindEnv("messaging.feishu.tts_moss_idle_timeout")
	_ = v.BindEnv("messaging.feishu.tts_moss_cpu_threads")
	_ = v.BindEnv("messaging.slack.tts_enabled")
	_ = v.BindEnv("messaging.slack.tts_provider")
	_ = v.BindEnv("messaging.slack.tts_voice")
	_ = v.BindEnv("messaging.slack.tts_max_chars")
	_ = v.BindEnv("messaging.slack.tts_moss_model_dir")
	_ = v.BindEnv("messaging.slack.tts_moss_voice")
	_ = v.BindEnv("messaging.slack.tts_moss_port")
	_ = v.BindEnv("messaging.slack.tts_moss_idle_timeout")
	_ = v.BindEnv("messaging.slack.tts_moss_cpu_threads")
	_ = v.BindEnv("webhook.enabled")
	_ = v.BindEnv("webhook.secret")
	_ = v.BindEnv("webhook.path")
	_ = v.BindEnv("webhook.max_body_size")
	_ = v.BindEnv("webhook.target_job_name")
	_ = v.BindEnv("events.retention")
	_ = v.BindEnv("execution.queue.enabled")
	_ = v.BindEnv("execution.queue.per_session")
	_ = v.BindEnv("execution.queue.global")
	_ = v.BindEnv("execution.queue.max_payload_bytes")
	_ = v.BindEnv("execution.queue.interactive_ttl")
	_ = v.BindEnv("execution.queue.ttl")
	_ = v.BindEnv("execution.queue.sweep_interval")
	_ = v.BindEnv("execution.queue.sweep_batch")
	_ = v.BindEnv("lifecycle.policy")
	_ = v.BindEnv("lifecycle.conversation.archive_after")
	_ = v.BindEnv("lifecycle.conversation.retention_after_last_input")
	_ = v.BindEnv("lifecycle.content.retention")
	_ = v.BindEnv("lifecycle.effect_payload.retention_after_settlement")
	_ = v.BindEnv("lifecycle.facts.retention_after_settlement")
	_ = v.BindEnv("lifecycle.audit.capture_content")
	_ = v.BindEnv("lifecycle.audit.facts_retention")
	_ = v.BindEnv("lifecycle.audit.content_retention")
	_ = v.BindEnv("lifecycle.media.retention")
	_ = v.BindEnv("lifecycle.trace.retention")
	_ = v.BindEnv("lifecycle.gc.batch_size")
	_ = v.BindEnv("lifecycle.gc.interval")
	_ = v.BindEnv("lifecycle.gc.max_lag")
	_ = v.BindEnv("audit.enabled")
	_ = v.BindEnv("audit.retention")
	_ = v.BindEnv("audit.full_content_retention")
	_ = v.BindEnv("audit.collector.channel_cap")
	_ = v.BindEnv("audit.collector.batch_interval")
	_ = v.BindEnv("audit.collector.batch_size")
	_ = v.BindEnv("audit.collector.spill_dir")

	if err := rejectExplicitlyEmptyLifecycleEnv(); err != nil {
		return nil, err
	}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("config: environment override: %w", err)
	}

	// Normalize path fields AFTER env overrides, because Viper's env binding
	// writes raw values (e.g. "~/.hotplex/...") that bypass ExpandAndAbs.
	cfg.normalizePaths()

	return cfg, nil
}

func rejectExplicitlyEmptyLifecycleEnv() error {
	paths := []string{
		"lifecycle.policy",
		"lifecycle.conversation.archive_after",
		"lifecycle.conversation.retention_after_last_input",
		"lifecycle.content.retention",
		"lifecycle.effect_payload.retention_after_settlement",
		"lifecycle.facts.retention_after_settlement",
		"lifecycle.audit.capture_content",
		"lifecycle.audit.facts_retention",
		"lifecycle.audit.content_retention",
		"lifecycle.media.retention",
		"lifecycle.trace.retention",
		"lifecycle.gc.batch_size",
		"lifecycle.gc.interval",
		"lifecycle.gc.max_lag",
		"execution.queue.interactive_ttl",
	}
	for _, path := range paths {
		envKey := "HOTPLEX_" + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
		if value, ok := os.LookupEnv(envKey); ok && strings.TrimSpace(value) == "" {
			return fmt.Errorf("config: %s is explicitly empty", envKey)
		}
	}
	return nil
}

// loadRecursive loads a config file and its ancestors, detecting cycles.
// visited tracks file paths already loaded in the current chain; nil on the root call.
func loadRecursive(filePath string, visited []string) (*Config, error) {
	// Start with defaults.
	cfg := Default()

	// Track visited files for cycle detection.
	var ancestors []string
	if visited == nil {
		ancestors = []string{}
	} else {
		ancestors = make([]string, len(visited), len(visited)+1)
		copy(ancestors, visited)
	}

	var parentFile string
	var childViper *viper.Viper

	// If a config file is provided, unmarshal it over defaults.
	if filePath != "" {
		absPath, err := ExpandAndAbs(filePath)
		if err != nil {
			return nil, fmt.Errorf("config: resolve path %q: %w", filePath, err)
		}
		filePath = absPath

		// Check for cycle: if this file is already in the ancestor chain.
		if slices.Contains(ancestors, filePath) {
			return nil, fmt.Errorf("%w: %v → %s", ErrConfigCycle, append(ancestors, filePath), filePath)
		}

		childViper = viper.New()
		childViper.SetConfigFile(filePath)
		if err := childViper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read %q: %w", filePath, err)
		}
		if err := childViper.Unmarshal(cfg); err != nil {
			return nil, fmt.Errorf("config: unmarshal %q: %w", filePath, err)
		}

		parentFile = cfg.Inherits
	}

	// Recursively load parent config if inheritance is specified.
	// Resolve parentFile relative to the directory of the current file.
	if parentFile != "" {
		ancestors = append(ancestors, filePath)
		// If parentFile is relative, resolve it relative to the current file's directory.
		if !filepath.IsAbs(parentFile) && filePath != "" {
			parentFile = filepath.Join(filepath.Dir(filePath), parentFile)
		}
		parentCfg, err := loadRecursive(parentFile, ancestors)
		if err != nil {
			return nil, fmt.Errorf("config: inherits %q: %w", parentFile, err)
		}
		// Apply child values over parent using the already-loaded viper instance.
		// This avoids a second disk read and eliminates TOCTOU risk.
		if err := childViper.Unmarshal(parentCfg); err != nil {
			return nil, fmt.Errorf("config: merge %q: %w", filePath, err)
		}
		*cfg = *parentCfg
	}

	// Expand env vars in token slices (supports ${VAR} references in config files).
	for i, t := range cfg.Admin.Tokens {
		cfg.Admin.Tokens[i] = ExpandEnv(t)
	}
	for i, k := range cfg.Security.APIKeys {
		cfg.Security.APIKeys[i] = ExpandEnv(k)
	}

	// Numbered environment variables for slices (e.g. HOTPLEX_ADMIN_TOKEN_1..N)
	// This supports project conventions for secret rotation and .env clarity.
	cfg.Admin.Tokens = aggregateNumberedEnv(cfg.Admin.Tokens, "HOTPLEX_ADMIN_TOKEN_")
	cfg.Security.APIKeys = aggregateNumberedEnv(cfg.Security.APIKeys, "HOTPLEX_SECURITY_API_KEY_")

	// Resolve API key → user identity mappings from config.
	cfg.ResolvedAPIKeyUsers = resolveAPIKeyUsers(cfg.Security.APIKeyUsers, cfg.Security.APIKeys)

	// Messaging platform env var overrides.
	applyMessagingEnv(cfg)

	// Normalize multi-bot configs (backward compat: single-bot → bots[]).
	normalizeSlackBots(&cfg.Messaging.Slack)
	normalizeFeishuBots(&cfg.Messaging.Feishu)

	// Propagate shared messaging defaults to per-platform configs.
	// Priority: platform-level (YAML/env) > messaging-level > Default().
	propagateMessagingDefaults(cfg)

	// Expand env entries and drop those referencing unset vars without defaults.
	expanded := make([]string, 0, len(cfg.Worker.Environment))
	for _, e := range cfg.Worker.Environment {
		if entry, ok := expandEnvEntry(e); ok {
			expanded = append(expanded, entry)
		} else if _, loaded := warnedEnvEntries.LoadOrStore(e, true); !loaded {
			slog.Warn("excluding env entry: references unset variable", "entry", e)
		}
	}
	cfg.Worker.Environment = expanded

	cfg.normalizePaths()

	return cfg, nil
}

// normalizePaths expands ~ and resolves relative paths for all config path fields,
// and expands ${VAR} references in command templates.
func (c *Config) normalizePaths() {
	// 1. Expand environment variables in command templates and addresses.
	for _, ef := range []*string{
		&c.Gateway.Addr,
		&c.Admin.Addr,
		&c.Worker.ClaudeCode.Command,
		&c.Worker.OpenCodeServer.Command,
		&c.Worker.OpenCodeServer.Password,
		&c.Worker.CodexCLI.Command,
		&c.Messaging.Slack.LocalCmd,
		&c.Messaging.Feishu.LocalCmd,
		&c.Messaging.Feishu.MossModelDir,
		&c.Messaging.Slack.MossModelDir,
	} {
		if *ef != "" {
			*ef = ExpandEnv(*ef)
		}
	}
	// Per-bot env expansion (credentials + paths).
	var botFields []*string
	for i := range c.Messaging.Slack.Bots {
		botFields = append(botFields,
			&c.Messaging.Slack.Bots[i].BotToken,
			&c.Messaging.Slack.Bots[i].AppToken,
			&c.Messaging.Slack.Bots[i].WorkDir,
			&c.Messaging.Slack.Bots[i].LocalCmd,
			&c.Messaging.Slack.Bots[i].MossModelDir,
		)
	}
	for i := range c.Messaging.Feishu.Bots {
		botFields = append(botFields,
			&c.Messaging.Feishu.Bots[i].AppID,
			&c.Messaging.Feishu.Bots[i].AppSecret,
			&c.Messaging.Feishu.Bots[i].WorkDir,
			&c.Messaging.Feishu.Bots[i].LocalCmd,
			&c.Messaging.Feishu.Bots[i].MossModelDir,
		)
	}
	expandStringFields(botFields...)

	oauthFields := []*string{&c.OAuth.ExternalURL}
	for i := range c.OAuth.Providers {
		oauthFields = append(oauthFields,
			&c.OAuth.Providers[i].DisplayName,
			&c.OAuth.Providers[i].Issuer,
			&c.OAuth.Providers[i].ClientID,
			&c.OAuth.Providers[i].ClientSecret,
			&c.OAuth.Providers[i].UsernameClaim,
			&c.OAuth.Providers[i].DisplayNameClaim,
			&c.OAuth.Providers[i].EmailClaim,
		)
	}
	expandStringFields(oauthFields...)

	// 2. Expand ~ and normalize paths.
	for _, pf := range []*string{
		&c.DB.SQLite.Path,
		&c.DB.Path,
		&c.Worker.DefaultWorkDir,
		&c.Worker.PIDDir,
		&c.AgentConfig.ConfigDir,
		&c.Messaging.Slack.WorkDir,
		&c.Messaging.Slack.MossModelDir,
		&c.Messaging.Feishu.WorkDir,
		&c.Messaging.Feishu.MossModelDir,
		&c.Messaging.Yuanxin.WorkDir,
	} {
		if *pf != "" {
			absPath, err := ExpandAndAbs(*pf)
			if err != nil {
				if _, loaded := warnedEnvEntries.LoadOrStore("path:"+*pf, true); !loaded {
					slog.Warn("config: normalize path", "path", *pf, "err", err)
				}
				continue
			}
			*pf = absPath
		}
	}
	// Per-bot WorkDir and MossModelDir path normalization.
	var botPaths []*string
	for i := range c.Messaging.Slack.Bots {
		botPaths = append(botPaths, &c.Messaging.Slack.Bots[i].WorkDir, &c.Messaging.Slack.Bots[i].MossModelDir)
	}
	for i := range c.Messaging.Feishu.Bots {
		botPaths = append(botPaths, &c.Messaging.Feishu.Bots[i].WorkDir, &c.Messaging.Feishu.Bots[i].MossModelDir)
	}
	normalizePathFields(botPaths...)
}

// ExpandAndAbs returns an absolute path, resolving ~ and relative paths.
// If the path starts with ~ and $HOME is not set, the original path is returned.
func ExpandAndAbs(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			// In test environments, $HOME may not be set. Return the path as-is
			// rather than failing, but log a warning for visibility.
			// The path will fail later when actually accessed, which is acceptable.
			return p, nil
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	// Resolve symlinks to prevent TOCTOU attacks on SwitchWorkDir.
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// ResolvePlatformWorkDir returns the workdir for the given platform,
// falling back to Worker.DefaultWorkDir when the platform has no override.
func (c *Config) ResolvePlatformWorkDir(platform string) string {
	switch platform {
	case "slack":
		if c.Messaging.Slack.WorkDir != "" {
			return c.Messaging.Slack.WorkDir
		}
	case "feishu":
		if c.Messaging.Feishu.WorkDir != "" {
			return c.Messaging.Feishu.WorkDir
		}
	case "yuanxin":
		if c.Messaging.Yuanxin.WorkDir != "" {
			return c.Messaging.Yuanxin.WorkDir
		}
	}
	return c.Worker.DefaultWorkDir
}

// DefaultWorkerType is the compile-time default worker type — level 5 of the
// worker_type fallback. It mirrors worker.TypeClaudeCode but is declared here
// because internal/config cannot import internal/worker (import cycle). Keep in
// sync with the worker package constant.
const DefaultWorkerType = "claude_code"

// ResolveWorkerType resolves the effective worker type for a messaging-platform
// session via the documented 5-level fallback (#847 / F3):
//
//	per-bot  (bots[].worker_type)
//	→ platform YAML/env (slack/feishu/yuanxin .worker_type — env override and
//	  messaging shared-default propagation are already applied at config load)
//	→ compile default (DefaultWorkerType).
//
// It is the single convergence point for a chain otherwise scattered across
// Default()+propagatePlatform (load-time levels 5→4→2) and
// cmd/hotplex/messaging_init.go (startup-time level-1 per-bot override).
// WebChat sessions do NOT use this resolver: their worker_type is request-driven
// (gateway/api.go: body > query > workspace.WorkerPreference > default).
func (c *Config) ResolveWorkerType(platform, botName string) string {
	if wt := c.botWorkerType(platform, botName); wt != "" {
		return wt
	}
	if wt := c.platformWorkerType(platform); wt != "" {
		return wt
	}
	// Level 4: messaging shared default. Normally already propagated into the
	// platform value at load (propagatePlatform); consulted explicitly here so
	// the documented chain still holds for a config that skipped propagation
	// (e.g. unit tests). In a loaded config the platform value is already
	// non-empty here, so this is a defensive no-op in production.
	if c.Messaging.WorkerType != "" {
		return c.Messaging.WorkerType
	}
	return DefaultWorkerType
}

// botWorkerType returns the per-bot worker_type override (level 1), or "" when
// the platform has no bots or the named bot sets no override.
func (c *Config) botWorkerType(platform, botName string) string {
	if botName == "" {
		return ""
	}
	switch platform {
	case "slack":
		for _, b := range c.Messaging.Slack.Bots {
			if b.Name == botName {
				return b.WorkerType
			}
		}
	case "feishu":
		for _, b := range c.Messaging.Feishu.Bots {
			if b.Name == botName {
				return b.WorkerType
			}
		}
	}
	return ""
}

// platformWorkerType returns the platform-level worker_type (levels 2-4 already
// collapsed by load-time env override + messaging-default propagation), or "" for
// platforms without a worker_type field.
func (c *Config) platformWorkerType(platform string) string {
	switch platform {
	case "slack":
		return c.Messaging.Slack.WorkerType
	case "feishu":
		return c.Messaging.Feishu.WorkerType
	case "yuanxin":
		return c.Messaging.Yuanxin.WorkerType
	}
	return ""
}
