//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

// SetSysProcAttr configures the command to create a new process group (Windows).
func SetSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// GracefulTerminate sends CTRL_BREAK_EVENT to the process group.
// NOTE: This only works for processes sharing the caller's console. Processes
// created with CREATE_NEW_PROCESS_GROUP get their own console, so this signal
// may not reach them. The Job Object (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
// handles the actual process tree cleanup — this is a best-effort hint.
func GracefulTerminate(pgid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pgid))
}

// Terminate gracefully stops a single process by PID.
// On Windows this uses the same mechanism as GracefulTerminate since
// GenerateConsoleCtrlEvent targets a process group ID.
func Terminate(pid int) error {
	return GracefulTerminate(pid)
}

// ForceKill terminates the process via TerminateProcess.
// NOTE: Only kills the target process, not its children. The Manager creates a
// Job Object (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) at process start to handle
// full tree cleanup. This function serves as a fallback for non-Manager callers.
func ForceKill(pgid int) error {
	return forceKillProcess(pgid)
}

// ForceKillProcess kills a single process by PID (not its group).
func ForceKillProcess(pid int) error {
	return forceKillProcess(pid)
}

func forceKillProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d for termination: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 1)
}

// stillActive is the exit code Win32 reports for a process that has not exited.
// x/sys/windows does not export the constant.
const stillActive = 259

// IsProcessAlive reports whether a process is still running.
//
// A successful OpenProcess does not prove that: a process that has exited
// stays openable for as long as any handle to it remains open, and whoever
// started it usually holds one. The exit code is the reliable signal —
// STILL_ACTIVE while the process runs, the real code once it is gone. Reading
// only the open result made an already-stopped gateway look alive to
// `hotplex gateway stop`, which escalated to a kill and then reported a stop
// that had in fact already happened.
func IsProcessAlive(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("process %d check: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return fmt.Errorf("process %d exit code: %w", pid, err)
	}
	if code != stillActive {
		return fmt.Errorf("process %d has exited (code %d)", pid, code)
	}
	return nil
}

// IsProcessGroupAlive checks if a process (group leader) is still running.
func IsProcessGroupAlive(pgid int) error {
	return IsProcessAlive(pgid)
}

// IsProcessNotExist returns true if the error indicates the process does not exist.
func IsProcessNotExist(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

// DefaultGracePeriod is the default time to wait after graceful termination
// before escalating to force kill. Must match signal_unix.go value.
const DefaultGracePeriod = 5 * time.Second
