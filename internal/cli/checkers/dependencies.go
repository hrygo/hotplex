package checkers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/config"
)

type workerBinaryChecker struct{}

func (c workerBinaryChecker) Name() string     { return "dependencies.worker_binary" }
func (c workerBinaryChecker) Category() string { return "dependencies" }
func (c workerBinaryChecker) Check(ctx context.Context) cli.Diagnostic {
	cfg, err := loadConfig()
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Failed to load config for worker binary check",
			Detail:   err.Error(),
		}
	}

	if cfg == nil {
		cfg = config.Default()
	}

	type checkTarget struct {
		name   string
		cmdStr string
	}
	targets := []checkTarget{
		{"claude_code", cfg.Worker.ClaudeCode.Command},
		{"opencode_server", cfg.Worker.OpenCodeServer.Command},
		{"codex_cli", cfg.Worker.CodexCLI.Command},
		{"acp", cfg.Worker.ACP.Command},
	}

	var installed []string
	var missing []string

	for _, target := range targets {
		name := target.name
		cmdStr := target.cmdStr
		if cmdStr == "" {
			switch name {
			case "claude_code":
				cmdStr = "claude"
			case "opencode_server":
				cmdStr = "opencode"
			case "codex_cli":
				cmdStr = "codex"
			case "acp":
				cmdStr = "hermes"
			}
		}

		parts := strings.Fields(cmdStr)
		var binary string
		if len(parts) > 0 {
			binary = parts[0]
		}

		if binary != "" {
			if _, err := exec.LookPath(binary); err == nil {
				installed = append(installed, name)
			} else {
				missing = append(missing, name)
			}
		} else {
			missing = append(missing, name)
		}
	}

	if len(installed) > 0 {
		msg := "Installed workers: " + strings.Join(installed, ", ")
		if len(missing) > 0 {
			msg += ". Missing: " + strings.Join(missing, ", ")
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  msg,
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusFail,
		Message:  "No worker binary found. Missing: " + strings.Join(missing, ", "),
		FixHint:  "Install at least one worker CLI (e.g. claude-code, opencode, codex, hermes) and ensure it is in PATH.",
	}
}

type sqlitePathChecker struct {
	dbPath string
}

func (c sqlitePathChecker) Name() string     { return "dependencies.sqlite_path" }
func (c sqlitePathChecker) Category() string { return "dependencies" }
func (c sqlitePathChecker) Check(ctx context.Context) cli.Diagnostic {
	parentDir := filepath.Dir(c.dbPath)
	info, err := os.Stat(parentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return cli.Diagnostic{
				Name:     c.Name(),
				Category: c.Category(),
				Status:   cli.StatusWarn,
				Message:  "SQLite parent directory does not exist: " + parentDir,
				FixHint:  "Directory will be created automatically",
				FixFunc: func() error {
					return os.MkdirAll(parentDir, 0o755)
				},
			}
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Cannot stat SQLite parent directory: " + err.Error(),
		}
	}
	if !info.IsDir() {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "SQLite parent path is not a directory: " + parentDir,
		}
	}
	testPath := filepath.Join(parentDir, ".hotplex_write_test")
	f, err := os.Create(testPath)
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "SQLite parent directory is not writable: " + parentDir,
			Detail:   err.Error(),
		}
	}
	_ = f.Close()
	_ = os.Remove(testPath)
	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  "SQLite path is valid and writable: " + c.dbPath,
	}
}

func init() {
	cli.DefaultRegistry.Register(workerBinaryChecker{})
	cli.DefaultRegistry.Register(sqlitePathChecker{dbPath: filepath.Join(hotplexHome(), "data", "hotplex.db")})
}
