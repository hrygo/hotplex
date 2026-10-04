package gateway

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

func planBridge(t *testing.T, cfg *config.Config) *Bridge {
	t.Helper()
	b := NewBridge(BridgeDeps{Log: slog.Default()})
	if cfg != nil {
		b.SetConfigProvider(func() *config.Config { return cfg })
	}
	return b
}

func TestPrepareLaunchPlan_ResolvesAndBindsAFingerprint(t *testing.T) {
	t.Parallel()
	b := planBridge(t, config.Default())
	p := b.prepareLaunchPlan(workerLaunchParams{
		ctx:        t.Context(),
		wt:         worker.TypeClaudeCode,
		workerInfo: worker.SessionInfo{SessionID: "s-1", UserID: "u-1"},
		platform:   platformWebChat,
	})

	require.False(t, p.Blocked(), "codes: %v", p.BlockedCodes)
	require.Equal(t, entryWebChat, p.Entry)
	require.Equal(t, "claude_code", p.WorkerType)
	require.Equal(t, config.RuntimePlanModeShadow, p.Mode)
	require.Regexp(t, `^[0-9a-f]{64}$`, p.Fingerprint)
	require.Equal(t, "s-1", p.SessionID)
}

func TestPrepareLaunchPlan_ClassifiesTheEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		platform  string
		platformK map[string]string
		want      string
	}{
		{name: "webchat", platform: platformWebChat, want: entryWebChat},
		{name: "slack", platform: "slack", want: entryMessaging},
		{name: "feishu", platform: "feishu", want: entryMessaging},
		{name: "empty defaults to webchat", platform: "", want: entryWebChat},
		{name: "unknown platform is messaging", platform: "newplatform", want: entryMessaging},
		{
			name:      "cron wins over platform",
			platform:  "slack",
			platformK: map[string]string{"cron_job_id": "job-1"},
			want:      entryCron,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, entryKindFor(tc.platform, tc.platformK))
		})
	}
}

// TestPlanRolloutMode_RequiresBothAllowlists guards against a partially
// rolled-out plan reporting itself as live. Naming only the entry, or only the
// Worker, must leave the combination in shadow.
func TestPlanRolloutMode_RequiresBothAllowlists(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mode    string
		entries []string
		workers []string
		want    string
	}{
		{name: "default is shadow", want: config.RuntimePlanModeShadow},
		{
			name: "authoritative without allowlists stays shadow",
			mode: config.RuntimePlanModeAuthoritative,
			want: config.RuntimePlanModeShadow,
		},
		{
			name:    "entry only stays shadow",
			mode:    config.RuntimePlanModeAuthoritative,
			entries: []string{entryWebChat},
			want:    config.RuntimePlanModeShadow,
		},
		{
			name:    "worker only stays shadow",
			mode:    config.RuntimePlanModeAuthoritative,
			workers: []string{"claude_code"},
			want:    config.RuntimePlanModeShadow,
		},
		{
			name:    "both named is authoritative",
			mode:    config.RuntimePlanModeAuthoritative,
			entries: []string{entryWebChat},
			workers: []string{"claude_code"},
			want:    config.RuntimePlanModeAuthoritative,
		},
		{
			name:    "unlisted worker stays shadow",
			mode:    config.RuntimePlanModeAuthoritative,
			entries: []string{entryWebChat},
			workers: []string{"codex_cli"},
			want:    config.RuntimePlanModeShadow,
		},
		{
			name:    "unlisted entry stays shadow",
			mode:    config.RuntimePlanModeAuthoritative,
			entries: []string{entryMessaging},
			workers: []string{"claude_code"},
			want:    config.RuntimePlanModeShadow,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Worker.RuntimePlan = config.RuntimePlanConfig{
				Mode:                 tc.mode,
				AuthoritativeEntries: tc.entries,
				AuthoritativeWorkers: tc.workers,
			}
			b := planBridge(t, cfg)
			got := b.planRolloutMode(entryWebChat, "claude_code", cfg)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPlanRolloutMode_UnknownModeIsNeverAuthoritative(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Worker.RuntimePlan = config.RuntimePlanConfig{
		Mode:                 "AUTOMATIC",
		AuthoritativeEntries: []string{entryWebChat},
		AuthoritativeWorkers: []string{"claude_code"},
	}
	b := planBridge(t, cfg)
	require.Equal(t, config.RuntimePlanModeShadow,
		b.planRolloutMode(entryWebChat, "claude_code", cfg))
}

func TestPlanRolloutMode_NoConfigIsShadow(t *testing.T) {
	t.Parallel()
	b := planBridge(t, nil)
	require.Equal(t, config.RuntimePlanModeShadow,
		b.planRolloutMode(entryWebChat, "claude_code", nil))
}

func TestParityDivergences_ReportsFieldEnumsOnly(t *testing.T) {
	t.Parallel()
	legacy := worker.SessionInfo{
		PermissionMode: worker.PermissionModeWorkspace,
		AllowedTools:   []string{"Read"},
		Sandbox:        "workspace-write",
	}

	agreeing := parityDivergences(agentspec.AgentSpec{
		Worker: agentspec.WorkerSpec{Type: "claude_code"},
		Policy: agentspec.PolicySpec{
			PermissionMode:  worker.PermissionModeWorkspace,
			AllowedTools:    []string{"Read"},
			AllowedToolsSet: true,
		},
		Sandbox: agentspec.SandboxSpec{Mode: "workspace-write"},
	}, legacy)
	require.Empty(t, agreeing)

	diverged := parityDivergences(agentspec.AgentSpec{
		Policy: agentspec.PolicySpec{
			PermissionMode:     worker.PermissionModeReadOnly,
			AllowedTools:       []string{"Read", "Write"},
			AllowedToolsSet:    true,
			DisallowedTools:    []string{"Bash"},
			DisallowedToolsSet: true,
		},
		Sandbox: agentspec.SandboxSpec{Mode: "read-only"},
	}, legacy)
	require.ElementsMatch(t, []string{
		parityFieldPermission,
		parityFieldAllowedTools,
		parityFieldDisallowed,
		parityFieldSandbox,
	}, diverged)
}

// TestParityDivergences_UnresolvedIsNotDivergent keeps an ABSENT decision
// distinguishable from a DISAGREEING one. Coverage reports the former; only
// the latter belongs in parity.
func TestParityDivergences_UnresolvedIsNotDivergent(t *testing.T) {
	t.Parallel()
	legacy := worker.SessionInfo{
		PermissionMode: worker.PermissionModeWorkspace,
		AllowedTools:   []string{"Read"},
	}
	got := parityDivergences(agentspec.AgentSpec{
		Policy: agentspec.PolicySpec{
			// Neither field declared → no decision was made here.
			AllowedToolsSet: false,
			PermissionMode:  "",
		},
	}, legacy)
	require.Empty(t, got)
}

func TestLaunchFactsLift_SessionFacts(t *testing.T) {
	t.Parallel()
	f := launchFactsFor(&session.SessionInfo{
		WorkspaceID:       "ws-1",
		PermissionCeiling: worker.PermissionModeReadOnly,
		PlatformKey:       map[string]string{"_sandbox": "read-only"},
	})
	require.Equal(t, "ws-1", f.workspaceID)
	require.Equal(t, worker.PermissionModeReadOnly, f.sessionCeiling)
	require.Equal(t, "read-only", f.platformKey["_sandbox"])

	require.Equal(t, launchFacts{}, launchFactsFor(nil))
}

func TestLaunchFactsLift_StartParamsHaveNoCapturedCeiling(t *testing.T) {
	t.Parallel()
	f := launchFactsForStart(&worker.SessionStartParams{
		WorkspaceID: "ws-2",
		PlatformKey: map[string]string{"a": "b"},
	})
	require.Equal(t, "ws-2", f.workspaceID)
	require.NotEmpty(t, f.platformKey)
	require.Equal(t, launchFacts{}, launchFactsForStart(nil))
}

func TestPrepareLaunchPlan_SessionCeilingBoundsTheLaunch(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Worker.ClaudeCode.PermissionMode = worker.PermissionModeBypass
	b := planBridge(t, cfg)

	p := b.prepareLaunchPlan(workerLaunchParams{
		ctx: t.Context(),
		wt:  worker.TypeClaudeCode,
		workerInfo: worker.SessionInfo{
			SessionID:      "s-2",
			PermissionMode: worker.PermissionModeReadOnly,
		},
		platform: "slack",
		facts:    launchFacts{sessionCeiling: worker.PermissionModeReadOnly},
	})

	require.Equal(t, worker.PermissionModeReadOnly, p.Plan.AgentSpec.Policy.PermissionMode,
		"a config edit must not widen a live session")
}

func TestPrepareLaunchPlan_NoConfigProviderShowsTheCoverageGap(t *testing.T) {
	t.Parallel()
	b := planBridge(t, nil)
	p := b.prepareLaunchPlan(workerLaunchParams{
		ctx:        t.Context(),
		wt:         worker.TypeClaudeCode,
		workerInfo: worker.SessionInfo{SessionID: "s-3"},
		platform:   platformWebChat,
	})

	require.False(t, p.Plan.Coverage.PermissionMode,
		"with no config snapshot no tier was resolved, and the plan must say so")
	require.False(t, p.Plan.Coverage.ConfigMaterial)
	require.NotEmpty(t, p.Fingerprint)
}

// TestWorkerRunBinding_StaysComparable guards a runtime panic that unit tests
// missed: workerRunBinding values live in a sync.Map and are removed with
// CompareAndDelete, which compares the whole struct. Binding the launch plan
// by value carried slices (divergence/blocked codes) into the struct and made
// it uncomparable, so every run teardown panicked. The plan is held by pointer
// for exactly this reason.
func TestWorkerRunBinding_StaysComparable(t *testing.T) {
	t.Parallel()
	a := workerRunBinding{
		id:         "run-1",
		launchPlan: &launchPlan{DivergedFields: []string{parityFieldSandbox}},
	}
	b := workerRunBinding{
		id:         "run-2",
		launchPlan: &launchPlan{BlockedCodes: []string{"permission_exceeds_ceiling"}},
	}

	// Compile-time and runtime proof that the binding is comparable; this is
	// exactly the operation sync.Map.CompareAndDelete performs internally.
	require.NotEqual(t, a, b)
	require.True(t, a == a)
}

func TestBindAndClearWorkerRun_WithPlanAttached(t *testing.T) {
	t.Parallel()
	b := planBridge(t, config.Default())
	w := &mockWorkerForHandler{}
	plan := launchPlan{SessionID: "s-9", Fingerprint: "fp-1", WorkerType: "claude_code"}

	binding := b.bindWorkerRun("s-9", w, "run-9", plan)
	require.NotNil(t, binding.launchPlan)
	require.Equal(t, "fp-1", binding.launchPlan.Fingerprint)

	require.NotPanics(t, func() {
		b.clearWorkerRun("s-9", w, "run-9")
	})
	_, stillBound := b.workerRuns.Load("s-9")
	require.False(t, stillBound, "the run must actually be removed, not just survive the call")
}
