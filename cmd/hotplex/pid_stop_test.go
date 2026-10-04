package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker/proc"
)

const ignoreTermEnv = "HOTPLEX_TEST_IGNORE_SIGTERM"

// TestHelperIgnoreTerm is not a test. It is the body of a child process that
// refuses to die on SIGTERM, re-executed by
// TestStopGatewayEscalatesWhenTheProcessIgnoresTheSignal.
func TestHelperIgnoreTerm(t *testing.T) {
	if os.Getenv(ignoreTermEnv) != "1" {
		t.Skip("helper process body; only runs when re-executed as a child")
	}
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStopGatewayEscalatesWhenTheProcessIgnoresTheSignal pins that
// `hotplex gateway stop` reports what happened.
//
// Signalling a process is not proof that it stopped. On Windows Terminate is a
// best-effort CTRL_BREAK_EVENT aimed at a process group the target may not
// belong to, and the call returns whether or not the signal was delivered —
// so a gateway that ignored the stop used to be reported as stopped. The
// Windows release smoke found exactly that: the CLI printed
// "gateway stopped (PID …)", the process logged no shutdown, and it was still
// running a minute later.
func TestStopGatewayEscalatesWhenTheProcessIgnoresTheSignal(t *testing.T) {
	// Isolate the PID file the stop path removes, so the test can never take
	// down a real running gateway on the developer's machine. t.Setenv rules
	// out t.Parallel here, which is the trade this test makes deliberately.
	t.Setenv("HOTPLEX_HOME", t.TempDir())

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperIgnoreTerm", "-test.timeout=3m")
	cmd.Env = append(os.Environ(), ignoreTermEnv+"=1")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	// Reap from the moment it starts. This process is the child's parent, so
	// without a waiter the killed child lingers as a zombie and every liveness
	// probe reports it as alive.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	// Give the child time to install its SIGTERM handler; otherwise it could
	// die on the default disposition and the test would pass without ever
	// exercising the escalation.
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, proc.IsProcessAlive(pid), "child process should be running before the stop")

	inst := &gatewayInstance{PID: pid, Source: sourcePID}
	require.NoError(t, stopGatewayWithin(inst, 300*time.Millisecond),
		"a process that ignores the signal must still be stopped, not reported as stopped")

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the child process never exited")
	}
	require.Error(t, proc.IsProcessAlive(pid),
		"stopGateway returned success but the process is still running")
}
