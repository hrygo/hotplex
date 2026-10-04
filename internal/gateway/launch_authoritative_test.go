package gateway

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// authoritativeCfg turns the plan loose for exactly one entry×Worker pair.
func authoritativeCfg(entry, wt string) *config.Config {
	cfg := config.Default()
	cfg.Worker.RuntimePlan = config.RuntimePlanConfig{
		Mode:                 config.RuntimePlanModeAuthoritative,
		AuthoritativeEntries: []string{entry},
		AuthoritativeWorkers: []string{wt},
	}
	return cfg
}

// TestSharedRuntimeGuard_RefusesIncompatibleProcessProfile is the singleton
// boundary: opencode-server and codex CLI run ONE process for many sessions, so
// a process-scoped profile cannot be faked per session. The guard refuses and
// leaves the running process alone.
func TestSharedRuntimeGuard_RefusesIncompatibleProcessProfile(t *testing.T) {
	t.Parallel()
	g := newSharedRuntimeGuard()

	require.NoError(t, g.checkAndRecord(worker.TypeClaudeCode, "p1"),
		"claude_code runs one process per session and has no shared-profile conflict")
	require.NoError(t, g.checkAndRecord(worker.TypeOpenCodeSrv, "p1"))
	require.NoError(t, g.checkAndRecord(worker.TypeOpenCodeSrv, "p1"),
		"the same profile again is fine — that is the normal multi-session case")

	require.ErrorIs(t, g.checkAndRecord(worker.TypeOpenCodeSrv, "p2"), ErrPlanLaunchRefused)

	require.NoError(t, g.checkAndRecord(worker.TypeCodexCLI, "p2"),
		"a different worker type has its own process and its own recorded profile")
	require.ErrorIs(t, g.checkAndRecord(worker.TypeCodexCLI, "p3"), ErrPlanLaunchRefused,
		"codex_cli shares a process too, so it gets the same guard")
}

func TestProcessScopedProfile_ExcludesPerSessionDecisions(t *testing.T) {
	t.Parallel()
	// Two plans that differ only in per-session decisions (tools, permission
	// tier) must produce the SAME process profile, or every ordinary session
	// would look like a conflict on a shared process.
	base := launchPlan{}
	base.Plan.ConfigHash = "cfg-1"
	base.Plan.EnvProfile = "strict"
	base.Plan.AgentSpec.Policy.PermissionMode = worker.PermissionModeReadOnly

	other := launchPlan{}
	other.Plan.ConfigHash = "cfg-1"
	other.Plan.EnvProfile = "strict"
	other.Plan.AgentSpec.Policy.PermissionMode = worker.PermissionModeBypass
	other.Plan.AgentSpec.Policy.AllowedTools = []string{"Read", "Bash"}

	require.Equal(t, processScopedProfile(base), processScopedProfile(other))

	changed := launchPlan{}
	changed.Plan.ConfigHash = "cfg-2"
	require.NotEqual(t, processScopedProfile(base), processScopedProfile(changed),
		"a config revision change is a genuine process-level conflict")
}

// TestAuthoritativeLaunch_RefusesBlockedPlanWithoutStarting pins the rule that
// makes the plan more than a log line: a blocked plan stops the launch. There
// is deliberately no "log it and fall back to legacy" branch.
func TestAuthoritativeLaunch_RefusesBlockedPlanWithoutStarting(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-blocked-plan"
	w := &mockBridgeWorker{
		workerType: worker.TypeCodexCLI,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeCodexCLI,
		State: events.StateRunning, Platform: "slack",
		// A sandbox mode outside the verifiable vocabulary blocks the plan.
		PlatformKey: map[string]string{"_sandbox": "totally-bogus-mode"},
	}, nil).Once()
	// StartFreshWorker checks for an existing worker before deciding to do a
	// fresh start; there is none.
	sm.On("GetWorker", sessionID).Return(nil).Maybe()

	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: newTestHub(t), SM: sm})
	b.SetConfigProvider(func() *config.Config {
		return authoritativeCfg(entryMessaging, "codex_cli")
	})
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{w}})

	_, err := b.StartFreshWorker(context.Background(), sessionID)
	require.ErrorIs(t, err, ErrPlanLaunchRefused)
	require.True(t, w.terminated.Load(), "the refused worker must be torn down")

	recorded, ok := b.LaunchPlanFor(sessionID)
	require.False(t, ok, "a refused launch must not leave a plan bound to the session")
	require.Empty(t, recorded.PlanHash)
}

// TestAuthoritativeLaunch_AppliesThePlan proves the plan actually drives the
// start, and that this is distinguishable from a plan that merely agreed.
func TestAuthoritativeLaunch_AppliesThePlan(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-authoritative"
	eventsCh := make(chan *events.Envelope)
	w := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: eventsCh},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, Platform: "slack",
		AllowedTools: []string{"Read"},
	}, nil).Once()
	sm.On("AttachWorker", sessionID, w).Return(nil).Once()
	sm.On("GetWorker", sessionID).Return(w).Maybe()
	sm.On("DetachWorkerIf", sessionID, w).Return(true).Maybe()

	cfg := authoritativeCfg(entryMessaging, "claude_code")
	cfg.Worker.ClaudeCode.PermissionMode = worker.PermissionModeReadOnly
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: newTestHub(t), SM: sm})
	b.SetConfigProvider(func() *config.Config { return cfg })
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{w}})

	_, err := b.StartFreshWorker(context.Background(), sessionID)
	require.NoError(t, err)

	require.Equal(t, worker.PermissionModeReadOnly, w.startInfo.PermissionMode,
		"the worker started from the plan's tier")
	require.True(t, b.LaunchPlanApplied(sessionID),
		"an applied plan must be distinguishable from a merely-agreeing one")

	recorded, ok := b.LaunchPlanFor(sessionID)
	require.True(t, ok)
	require.Regexp(t, `^[0-9a-f]{64}$`, recorded.LaunchFingerprint)
}

// TestShadowLaunch_ChangesNothing guards that D2 behaviour survived D3:
// outside the allowlists the legacy parameters stay authoritative.
func TestShadowLaunch_ChangesNothing(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-shadow"
	eventsCh := make(chan *events.Envelope)
	w := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: eventsCh},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, Platform: "slack",
		PermissionCeiling: worker.PermissionModeReadOnly,
	}, nil).Once()
	sm.On("AttachWorker", sessionID, w).Return(nil).Once()
	sm.On("GetWorker", sessionID).Return(w).Maybe()
	sm.On("DetachWorkerIf", sessionID, w).Return(true).Maybe()

	// Authoritative for a DIFFERENT worker: this combination must stay shadow.
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: newTestHub(t), SM: sm})
	b.SetConfigProvider(func() *config.Config {
		return authoritativeCfg(entryMessaging, "codex_cli")
	})
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{w}})

	_, err := b.StartFreshWorker(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, b.LaunchPlanApplied(sessionID))
	require.Equal(t, worker.PermissionModeReadOnly, w.startInfo.PermissionMode)
}

func TestLaunchPlanFor_NoRunIsNotAnError(t *testing.T) {
	t.Parallel()
	b := planBridge(t, config.Default())
	_, ok := b.LaunchPlanFor("never-launched")
	require.False(t, ok)
	require.False(t, b.LaunchPlanApplied("never-launched"))
}
