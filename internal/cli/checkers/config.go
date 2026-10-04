package checkers

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/config"
)

var configPath atomic.Value // string

// SetConfigPath sets the config file path for all config checkers.
func SetConfigPath(path string) {
	configPath.Store(path)
}

// getConfigPath returns the configured config file path ("" when unset).
// The value is stored atomically: production writes it once at doctor/onboard
// entry, while tests may swap it between serial tests. Atomic access keeps
// concurrent reads race-free even when multiple parallel tests exercise
// checkers at once. Writers must still stay serial: a mid-check swap would
// only produce a logically inconsistent diagnostic, which no lock can fix.
// hotplexHome is the single indirection point for config.HotplexHome in
// checker registrations (#978). Tests override it via ... (see provider note).
func hotplexHome() string { return config.HotplexHome() }

func getConfigPath() string {
	v := configPath.Load()
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// loadConfig loads the config from the configured path. Returns (nil, nil) when
// no path is set, so callers distinguish "not configured" from a load error
// without repeating the empty-check + Load boilerplate at every call site.
func loadConfig() (*config.Config, error) {
	if getConfigPath() == "" {
		return nil, nil
	}
	return config.Load(getConfigPath())
}

// ─── config.exists ────────────────────────────────────────────────────────────

type configExistsChecker struct{}

func (c configExistsChecker) Name() string     { return "config.exists" }
func (c configExistsChecker) Category() string { return "config" }
func (c configExistsChecker) Check(ctx context.Context) cli.Diagnostic {
	if getConfigPath() == "" {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Config path not set",
			FixHint:  "Set config path via SetConfigPath() before running checks",
		}
	}

	_, err := os.Stat(getConfigPath())
	if err == nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "Config file exists",
			Detail:   getConfigPath(),
		}
	}

	if os.IsNotExist(err) {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Config file missing",
			Detail:   getConfigPath(),
			FixHint:  "Create config file or run onboard",
			FixFunc:  fixConfigExists,
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusFail,
		Message:  "Cannot access config file",
		Detail:   err.Error(),
	}
}

func fixConfigExists() error {
	templatePath := filepath.Join("configs", "config.yaml")
	if _, err := os.Stat(templatePath); err == nil {
		data, err := os.ReadFile(templatePath)
		if err != nil {
			return fmt.Errorf("read template: %w", err)
		}
		return os.WriteFile(getConfigPath(), data, 0o600)
	}

	minimal := `gateway:
  addr: ":8888"
admin:
  addr: ":9999"
db:
  path: "~/.hotplex/data/hotplex.db"
worker:
  type: "claude_code"
log:
  level: "info"
`
	return os.WriteFile(getConfigPath(), []byte(minimal), 0o600)
}

func init() {
	cli.DefaultRegistry.Register(configExistsChecker{})
}

// configSourceChecker makes the configuration boundary visible in diagnostics.
// It intentionally reports paths only; it must never print environment values.
type configSourceChecker struct{}

func (c configSourceChecker) Name() string     { return "config.source" }
func (c configSourceChecker) Category() string { return "config" }
func (c configSourceChecker) Check(ctx context.Context) cli.Diagnostic {
	if getConfigPath() == "" {
		return cli.Diagnostic{Name: c.Name(), Category: c.Category(), Status: cli.StatusFail, Message: "Config path not set", FixHint: "Set the config path before running diagnostics"}
	}

	if _, err := config.Load(getConfigPath()); err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Cannot resolve effective config source", Detail: err.Error(),
			FixHint: "Fix config syntax errors first",
		}
	}

	envPath := filepath.Join(filepath.Dir(getConfigPath()), ".env")
	_, err := os.Stat(envPath)
	if os.IsNotExist(err) {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Effective config loaded; adjacent .env is missing",
			Detail:  fmt.Sprintf("config=%s; env=%s", getConfigPath(), envPath),
			FixHint: "Run onboard or create the adjacent .env file",
		}
	}
	if err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Effective config loaded; cannot inspect adjacent .env",
			Detail:  fmt.Sprintf("config=%s; env=%s; error=%v", getConfigPath(), envPath, err),
			FixHint: "Check permissions for the config directory and adjacent .env",
		}
	}

	return cli.Diagnostic{
		Name: c.Name(), Category: c.Category(), Status: cli.StatusPass,
		Message: "Effective config and adjacent .env loaded",
		Detail:  fmt.Sprintf("config=%s; env=%s", getConfigPath(), envPath),
	}
}

func init() {
	cli.DefaultRegistry.Register(configSourceChecker{})
}

// ─── config.syntax ────────────────────────────────────────────────────────────

type configSyntaxChecker struct{}

func (c configSyntaxChecker) Name() string     { return "config.syntax" }
func (c configSyntaxChecker) Category() string { return "config" }
func (c configSyntaxChecker) Check(ctx context.Context) cli.Diagnostic {
	if getConfigPath() == "" {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Config path not set",
		}
	}

	_, err := config.Load(getConfigPath())
	if err == nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "Config syntax valid",
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusFail,
		Message:  "Config syntax error",
		Detail:   err.Error(),
	}
}

func init() {
	cli.DefaultRegistry.Register(configSyntaxChecker{})
}

// ─── config.required ──────────────────────────────────────────────────────────

type configRequiredChecker struct{}

func (c configRequiredChecker) Name() string     { return "config.required" }
func (c configRequiredChecker) Category() string { return "config" }
func (c configRequiredChecker) Check(ctx context.Context) cli.Diagnostic {
	if getConfigPath() == "" {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Config path not set",
		}
	}

	cfg, err := config.Load(getConfigPath())
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Cannot load config for validation",
			Detail:   err.Error(),
		}
	}

	var missing []string

	if cfg.Messaging.Slack.Enabled {
		if len(cfg.Messaging.Slack.Bots) > 0 {
			// Multi-bot mode: check each bot's credentials.
			for i, bot := range cfg.Messaging.Slack.Bots {
				prefix := fmt.Sprintf("messaging.slack.bots[%d]", i)
				if bot.Name != "" {
					prefix = fmt.Sprintf("messaging.slack.bots[%q]", bot.Name)
				}
				if bot.BotToken == "" {
					missing = append(missing, prefix+".bot_token")
				}
				if bot.AppToken == "" {
					missing = append(missing, prefix+".app_token")
				}
			}
		} else {
			// Single-bot mode: check top-level credentials.
			if cfg.Messaging.Slack.BotToken == "" {
				missing = append(missing, "messaging.slack.bot_token")
			}
			if cfg.Messaging.Slack.AppToken == "" {
				missing = append(missing, "messaging.slack.app_token")
			}
		}
	}
	if cfg.Messaging.Feishu.Enabled {
		if len(cfg.Messaging.Feishu.Bots) > 0 {
			// Multi-bot mode: check each bot's credentials.
			for i, bot := range cfg.Messaging.Feishu.Bots {
				prefix := fmt.Sprintf("messaging.feishu.bots[%d]", i)
				if bot.Name != "" {
					prefix = fmt.Sprintf("messaging.feishu.bots[%q]", bot.Name)
				}
				if bot.AppID == "" {
					missing = append(missing, prefix+".app_id")
				}
				if bot.AppSecret == "" {
					missing = append(missing, prefix+".app_secret")
				}
			}
		} else {
			// Single-bot mode: check top-level credentials.
			if cfg.Messaging.Feishu.AppID == "" {
				missing = append(missing, "messaging.feishu.app_id")
			}
			if cfg.Messaging.Feishu.AppSecret == "" {
				missing = append(missing, "messaging.feishu.app_secret")
			}
		}
	}
	if !cfg.Messaging.Slack.Enabled && !cfg.Messaging.Feishu.Enabled {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "No messaging platform enabled",
			Detail:   "Slack and Feishu are both disabled. Enable one to use messaging features.",
			FixHint:  "Run onboard to configure Slack or Feishu",
		}
	}

	if len(missing) > 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Missing required fields",
			Detail:   strings.Join(missing, ", "),
			FixHint:  "Run onboard to configure required fields",
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  "Required fields present",
	}
}

func init() {
	cli.DefaultRegistry.Register(configRequiredChecker{})
}

// ─── config.values ────────────────────────────────────────────────────────────

type configValuesChecker struct{}

func (c configValuesChecker) Name() string     { return "config.values" }
func (c configValuesChecker) Category() string { return "config" }
func (c configValuesChecker) Check(ctx context.Context) cli.Diagnostic {
	if getConfigPath() == "" {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Config path not set",
		}
	}

	cfg, err := config.Load(getConfigPath())
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Cannot load config",
			Detail:   err.Error(),
		}
	}

	var issues []string

	gatewayPort := extractPort(cfg.Gateway.Addr)
	if gatewayPort < 1 || gatewayPort > 65535 {
		issues = append(issues, fmt.Sprintf("gateway.addr port out of range: %s", cfg.Gateway.Addr))
	}

	adminPort := extractPort(cfg.Admin.Addr)
	if cfg.Admin.Enabled && (adminPort < 1 || adminPort > 65535) {
		issues = append(issues, fmt.Sprintf("admin.addr port out of range: %s", cfg.Admin.Addr))
	}

	if cfg.DB.Path != "" {
		dir := filepath.Dir(cfg.DB.Path)
		if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
			issues = append(issues, fmt.Sprintf("db.path directory does not exist: %s", dir))
		}
	}

	if len(issues) > 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Invalid config values",
			Detail:   strings.Join(issues, "; "),
			FixHint:  "Reset to defaults",
			FixFunc: func() error {
				return fixConfigValues(cfg)
			},
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  "Config values valid",
	}
}

func extractPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}

func fixConfigValues(cfg *config.Config) error {
	defaults := config.Default()
	needsFix := false

	if gatewayPort := extractPort(cfg.Gateway.Addr); gatewayPort < 1 || gatewayPort > 65535 {
		cfg.Gateway.Addr = defaults.Gateway.Addr
		needsFix = true
	}
	if cfg.Admin.Enabled {
		if adminPort := extractPort(cfg.Admin.Addr); adminPort < 1 || adminPort > 65535 {
			cfg.Admin.Addr = defaults.Admin.Addr
			needsFix = true
		}
	}
	if cfg.DB.Path != "" {
		dir := filepath.Dir(cfg.DB.Path)
		if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
			cfg.DB.Path = defaults.DB.Path
			needsFix = true
		}
	}
	if !needsFix {
		return nil
	}

	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	yaml := string(data)
	if cfg.Gateway.Addr != extractPortAddr(yaml, "gateway.addr") {
		yaml = replaceYAMLValue(yaml, "gateway.addr", cfg.Gateway.Addr)
	}
	if cfg.Admin.Addr != extractPortAddr(yaml, "admin.addr") {
		yaml = replaceYAMLValue(yaml, "admin.addr", cfg.Admin.Addr)
	}
	if cfg.DB.Path != defaults.DB.Path {
		yaml = replaceYAMLValue(yaml, "db.path", cfg.DB.Path)
	}

	return os.WriteFile(getConfigPath(), []byte(yaml), 0o600)
}

func extractPortAddr(yaml, key string) string {
	lines := strings.Split(yaml, "\n")
	prefix := key + ":"
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			val := strings.TrimSpace(trimmed[len(prefix):])
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

func replaceYAMLValue(yaml, key, value string) string {
	lines := strings.Split(yaml, "\n")
	prefix := key + ":"
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = leading + prefix + " " + strconv.Quote(value)
			break
		}
	}
	return strings.Join(lines, "\n")
}

func init() {
	cli.DefaultRegistry.Register(configValuesChecker{})
}

// ─── config.env_vars ──────────────────────────────────────────────────────────

type configEnvVarsChecker struct{}

func (c configEnvVarsChecker) Name() string     { return "config.env_vars" }
func (c configEnvVarsChecker) Category() string { return "config" }
func (c configEnvVarsChecker) Check(ctx context.Context) cli.Diagnostic {

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		adminToken = os.Getenv("HOTPLEX_ADMIN_TOKEN_1")
	}

	var missing []string

	if adminToken == "" {
		missing = append(missing, "ADMIN_TOKEN (or HOTPLEX_ADMIN_TOKEN_1)")
	}

	if len(missing) > 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "Critical environment variables not set",
			Detail:   strings.Join(missing, ", "),
			FixHint:  "Write missing vars to .env file",
			FixFunc:  fixEnvVars,
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  "Critical environment variables present",
	}
}

func fixEnvVars() error {
	existing := make(map[string]bool)
	envPath := envFilePath()
	if data, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if idx := strings.Index(line, "="); idx > 0 {
				existing[strings.TrimSpace(line[:idx])] = true
			}
		}
	}

	var lines []string

	if !existing["HOTPLEX_ADMIN_TOKEN_1"] {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate admin token: %w", err)
		}
		lines = append(lines, fmt.Sprintf("HOTPLEX_ADMIN_TOKEN_1=%x", b))
	}

	if len(lines) == 0 {
		return nil
	}

	f, err := os.OpenFile(envPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open .env: %w", err)
	}
	defer func() { _ = f.Close() }()

	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			return fmt.Errorf("write .env: %w", err)
		}
	}

	return nil
}

func init() {
	cli.DefaultRegistry.Register(configEnvVarsChecker{})
}
