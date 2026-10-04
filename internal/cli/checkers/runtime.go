package checkers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/proc"
)

type diskSpaceChecker struct{}

func (c diskSpaceChecker) Name() string     { return "runtime.disk_space" }
func (c diskSpaceChecker) Category() string { return "runtime" }
func (c diskSpaceChecker) Check(ctx context.Context) cli.Diagnostic {
	freeMB, err := GetDiskFreeMB(".")
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "Cannot determine disk space",
			Detail:   err.Error(),
		}
	}
	if freeMB >= 100 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  fmt.Sprintf("Disk space available: %d MB (minimum 100 MB)", freeMB),
		}
	}
	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusFail,
		Message:  fmt.Sprintf("Low disk space: %d MB available (minimum 100 MB)", freeMB),
		FixHint:  "Free up disk space or move data directory to a larger volume",
	}
}

type portAvailableChecker struct{}

func (c portAvailableChecker) Name() string     { return "runtime.port_available" }
func (c portAvailableChecker) Category() string { return "runtime" }
func (c portAvailableChecker) Check(ctx context.Context) cli.Diagnostic {
	var blocked []string
	for _, port := range []int{8888, 9999} {
		addr := fmt.Sprintf(":%d", port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			blocked = append(blocked, fmt.Sprintf(":%d (%s)", port, err.Error()))
			continue
		}
		_ = l.Close()
	}
	if len(blocked) == 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "Ports 8888 and 9999 are available",
		}
	}
	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusFail,
		Message:  "Ports in use: " + strings.Join(blocked, ", "),
		FixHint:  "Kill the process using the port (lsof -i :PORT | grep LISTEN) then kill -9 <PID>",
	}
}

type gatewayHealthChecker struct {
	client     *http.Client
	endpointFn func(*config.Config) (string, error)
}

func (c gatewayHealthChecker) Name() string     { return "runtime.gateway_health" }
func (c gatewayHealthChecker) Category() string { return "runtime" }
func (c gatewayHealthChecker) Check(ctx context.Context) cli.Diagnostic {
	cfg, err := loadConfig()
	if err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Cannot load Gateway config", Detail: err.Error(),
			FixHint: "Fix config syntax errors first",
		}
	}
	if cfg == nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Gateway health check skipped (config path not set)",
			FixHint: "Run doctor with the same --config path used to start Gateway",
		}
	}

	endpointFn := c.endpointFn
	if endpointFn == nil {
		endpointFn = gatewayHealthEndpoint
	}
	endpoint, err := endpointFn(cfg)
	if err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusFail,
			Message: "Invalid Gateway health endpoint", Detail: err.Error(),
			FixHint: "Fix gateway.addr in the effective config",
		}
	}

	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusFail,
			Message: "Cannot create Gateway health request", Detail: err.Error(),
			FixHint: "Fix gateway.addr in the effective config",
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusWarn,
			Message: "Gateway is not running or health endpoint is unreachable",
			Detail:  err.Error(),
			FixHint: "Start Gateway with `hotplex service start` or `hotplex gateway start`",
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return cli.Diagnostic{
			Name: c.Name(), Category: c.Category(), Status: cli.StatusFail,
			Message: fmt.Sprintf("Gateway health returned HTTP %d", resp.StatusCode),
			FixHint: "Inspect Gateway logs and run `hotplex gateway restart` after fixing the reported error",
		}
	}

	return cli.Diagnostic{
		Name: c.Name(), Category: c.Category(), Status: cli.StatusPass,
		Message: fmt.Sprintf("Gateway health OK (HTTP %d)", resp.StatusCode),
		Detail:  endpoint,
	}
}

func gatewayHealthEndpoint(cfg *config.Config) (string, error) {
	addr := strings.TrimSpace(cfg.Gateway.Addr)
	if addr == "" {
		return "", fmt.Errorf("gateway.addr is empty")
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/") + "/health", nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse gateway.addr %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/health", nil
}

type orphanPIDsChecker struct {
	pidDir string
}

func (c orphanPIDsChecker) Name() string     { return "runtime.orphan_pids" }
func (c orphanPIDsChecker) Category() string { return "runtime" }
func (c orphanPIDsChecker) Check(ctx context.Context) cli.Diagnostic {
	entries, err := os.ReadDir(c.pidDir)
	if err != nil {
		if os.IsNotExist(err) {
			return cli.Diagnostic{
				Name:     c.Name(),
				Category: c.Category(),
				Status:   cli.StatusPass,
				Message:  "PID directory does not exist (no orphans possible)",
			}
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "Cannot read PID directory: " + c.pidDir,
			Detail:   err.Error(),
		}
	}

	var orphanFiles []string
	var alive []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		pidStr := strings.TrimSuffix(name, filepath.Ext(name))
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		if err := proc.IsProcessAlive(pid); err != nil {
			orphanFiles = append(orphanFiles, name)
		} else {
			alive = append(alive, pid)
		}
	}

	if len(orphanFiles) == 0 && len(alive) == 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "No PID files found",
		}
	}

	if len(orphanFiles) > 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  fmt.Sprintf("%d orphan PID file(s) found (processes not running)", len(orphanFiles)),
			Detail:   "Orphans: " + strings.Join(orphanFiles, ", "),
			FixHint:  "Run doctor --fix to remove stale PID files",
			FixFunc: func() error {
				for _, f := range orphanFiles {
					if err := os.Remove(filepath.Join(c.pidDir, f)); err != nil {
						return fmt.Errorf("remove %s: %w", f, err)
					}
				}
				return nil
			},
		}
	}

	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  fmt.Sprintf("%d worker process(es) running", len(alive)),
		Detail:   "PIDs: " + fmt.Sprint(alive),
	}
}

type dataDirWritableChecker struct {
	dataDir string
}

func (c dataDirWritableChecker) Name() string     { return "runtime.data_dir_writable" }
func (c dataDirWritableChecker) Category() string { return "runtime" }
func (c dataDirWritableChecker) Check(ctx context.Context) cli.Diagnostic {
	info, err := os.Stat(c.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return cli.Diagnostic{
				Name:     c.Name(),
				Category: c.Category(),
				Status:   cli.StatusWarn,
				Message:  "Data directory does not exist: " + c.dataDir,
				FixHint:  "Directory will be created automatically",
				FixFunc: func() error {
					return os.MkdirAll(c.dataDir, 0o755)
				},
			}
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Cannot stat data directory: " + c.dataDir,
			Detail:   err.Error(),
		}
	}
	if !info.IsDir() {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Data path is not a directory: " + c.dataDir,
		}
	}

	testPath := filepath.Join(c.dataDir, ".hotplex_write_test")
	f, err := os.Create(testPath)
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusFail,
			Message:  "Data directory is not writable: " + c.dataDir,
			Detail:   err.Error(),
			FixHint:  "Check directory permissions or run with sudo",
		}
	}
	_ = f.Close()
	_ = os.Remove(testPath)
	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusPass,
		Message:  "Data directory is writable: " + c.dataDir,
	}
}

func init() {
	home := hotplexHome()
	cli.DefaultRegistry.Register(diskSpaceChecker{})
	cli.DefaultRegistry.Register(portAvailableChecker{})
	cli.DefaultRegistry.Register(gatewayHealthChecker{})
	cli.DefaultRegistry.Register(orphanPIDsChecker{pidDir: filepath.Join(home, ".pids")})
	cli.DefaultRegistry.Register(dataDirWritableChecker{dataDir: filepath.Join(home, "data")})
}
