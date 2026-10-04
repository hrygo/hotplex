package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/cli/pidutil"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/service"
	"github.com/hrygo/hotplex/internal/worker/proc"
)

func gatewayPIDPath() string {
	return pidutil.PIDPath()
}

func writeGatewayState(configPath string, devMode bool) error {
	pidPath := gatewayPIDPath()
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return err
	}
	state := pidutil.GatewayState{
		PID:        os.Getpid(),
		ConfigPath: configPath,
		DevMode:    devMode,
	}
	data, _ := json.Marshal(state)
	return os.WriteFile(pidPath, data, 0o644)
}

func readGatewayState() (*pidutil.GatewayState, error) {
	state, err := pidutil.ReadState()
	if err != nil {
		return nil, fmt.Errorf("gateway not running (no PID file)")
	}

	if err := proc.IsProcessAlive(state.PID); err != nil {
		removeGatewayState()
		if proc.IsProcessNotExist(err) {
			return nil, fmt.Errorf("gateway not running (PID %d stale)", state.PID)
		}
		return nil, fmt.Errorf("gateway not running (PID %d: %w)", state.PID, err)
	}

	return state, nil
}

func removeGatewayState() {
	_ = os.Remove(gatewayPIDPath())
}

func ensureNotRunning() error {
	inst, err := findRunningGateway()
	if err != nil {
		return nil
	}
	if inst.PID == os.Getpid() {
		return nil
	}
	return gatewayAlreadyRunningError(inst)
}

type discoverySource string

const (
	sourcePID     discoverySource = "pid"
	sourceService discoverySource = "service"
)

type gatewayInstance struct {
	PID        int
	Source     discoverySource
	Level      service.Level
	ConfigPath string
	DevMode    bool
}

func findRunningGateway() (*gatewayInstance, error) {
	return findRunningGatewayWith(service.NewManager())
}

func findRunningGatewayWith(mgr service.Manager) (*gatewayInstance, error) {
	for _, level := range []service.Level{service.LevelUser, service.LevelSystem} {
		s, err := mgr.Status("hotplex", level)
		if err == nil && s != nil && s.Running {
			return &gatewayInstance{PID: s.PID, Source: sourceService, Level: level}, nil
		}
	}

	if state, err := readGatewayState(); err == nil {
		return &gatewayInstance{
			PID:        state.PID,
			Source:     sourcePID,
			ConfigPath: state.ConfigPath,
			DevMode:    state.DevMode,
		}, nil
	}

	return nil, fmt.Errorf("gateway not running (no PID file and no service found)")
}

func gatewayAlreadyRunningError(inst *gatewayInstance) error {
	if inst.Source == sourceService {
		if inst.PID > 0 {
			return fmt.Errorf("gateway already running as %s service (PID %d); use 'hotplex service stop --level %s' first",
				inst.Level, inst.PID, inst.Level)
		}
		return fmt.Errorf("gateway is managed by the %s service, but no active process PID is available; use 'hotplex service stop --level %s' first",
			inst.Level, inst.Level)
	}
	return fmt.Errorf("gateway already running (PID %d, via %s); use 'hotplex gateway stop' first",
		inst.PID, inst.Source)
}

func gatewayStoppedMessage(inst *gatewayInstance) string {
	if inst.Source == sourceService {
		if inst.PID > 0 {
			return fmt.Sprintf("gateway service stopped (level=%s, PID=%d)", inst.Level, inst.PID)
		}
		return fmt.Sprintf("gateway service stopped (level=%s, process PID unavailable)", inst.Level)
	}
	return fmt.Sprintf("gateway stopped (PID %d, %s)", inst.PID, inst.Source)
}

func stopGateway(inst *gatewayInstance) error {
	return stopGatewayWithin(inst, proc.DefaultGracePeriod)
}

// stopGatewayWithin stops a pid-managed gateway and waits up to grace for it to
// actually leave before escalating. The grace period is a parameter so tests
// can exercise the escalation path without waiting the production window.
func stopGatewayWithin(inst *gatewayInstance, grace time.Duration) error {
	switch inst.Source {
	case sourcePID:
		// Use Terminate (direct PID signal) instead of GracefulTerminate (process
		// group signal). The gateway may not be a process group leader when started
		// in foreground mode (PGID inherited from parent shell).
		if err := proc.Terminate(inst.PID); err != nil {
			return fmt.Errorf("stop PID %d: %w", inst.PID, err)
		}
		// Signalling is not proof that the process stopped. On Windows Terminate
		// is a best-effort CTRL_BREAK_EVENT aimed at a process group the target
		// may not belong to, and GenerateConsoleCtrlEvent returns whether or not
		// the signal was delivered — so a gateway that kept running was reported
		// as stopped. Wait for the process to actually leave, then escalate, and
		// only clear the recorded state once it is gone.
		waitForProcessExit(inst.PID, grace)
		if proc.IsProcessAlive(inst.PID) == nil {
			if err := proc.ForceKillProcess(inst.PID); err != nil {
				return fmt.Errorf("gateway PID %d did not stop and could not be killed: %w", inst.PID, err)
			}
			waitForProcessExit(inst.PID, grace)
			if proc.IsProcessAlive(inst.PID) == nil {
				return fmt.Errorf("gateway PID %d is still running after a forced stop", inst.PID)
			}
		}
		removeGatewayState()
	case sourceService:
		if err := service.NewManager().Stop("hotplex", inst.Level); err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
	}
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.IsProcessAlive(pid); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cleanupWebchatOrphan terminates a running webchat dev process started by `make dev`.
// Returns the cleaned-up PID, or 0 if no orphan was found.
func cleanupWebchatOrphan() int {
	pidPath := filepath.Join(config.HotplexHome(), ".pids", "hotplex-webchat.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	if proc.IsProcessAlive(pid) != nil {
		_ = os.Remove(pidPath)
		return 0
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	_ = os.Remove(pidPath)
	return pid
}

// ─── Restart Cooldown Marker ──────────────────────────────────────────────────

type restartMarker struct {
	HelperPID int       `json:"helper_pid"`
	CreatedAt time.Time `json:"created_at"`
}

func restartMarkerPath() string {
	return filepath.Join(config.HotplexHome(), ".pids", "gateway.restart")
}
