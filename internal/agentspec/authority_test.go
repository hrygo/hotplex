package agentspec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
)

// claudeCfgWithDefault returns a messaging config whose claude_code default
// permission tier is the given one. With no workspace and no explicit request
// the resolver lands on exactly this tier (see resolvePermissionMode).
func claudeCfgWithDefault(tier string) *config.Config {
	cfg := config.Default()
	cfg.Worker.ClaudeCode.PermissionMode = tier
	return cfg
}

func TestPermissionRank_OrdersTiers(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0, PermissionRank(worker.PermissionModeReadOnly))
	require.Equal(t, 1, PermissionRank(worker.PermissionModeWorkspace))
	require.Equal(t, 2, PermissionRank(worker.PermissionModeAutoEdit))
	require.Equal(t, 3, PermissionRank(worker.PermissionModeBypass))
	require.Equal(t, -1, PermissionRank("nonsense"), "an unreadable tier must never rank as permissive")
	require.Equal(t, -1, PermissionRank(""), "an empty tier must never rank as permissive")
}

func TestAuthorityCeiling_EffectiveTakesTheTightest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		c        AuthorityCeiling
		want     string
		wantZero bool
	}{
		{name: "none", c: AuthorityCeiling{}, wantZero: true},
		{name: "workspace only", c: AuthorityCeiling{Workspace: worker.PermissionModeAutoEdit}, want: worker.PermissionModeAutoEdit},
		{name: "session only", c: AuthorityCeiling{Session: worker.PermissionModeReadOnly}, want: worker.PermissionModeReadOnly},
		{
			name: "session tighter than workspace",
			c:    AuthorityCeiling{Workspace: worker.PermissionModeBypass, Session: worker.PermissionModeReadOnly},
			want: worker.PermissionModeReadOnly,
		},
		{
			name: "workspace tighter than session",
			c:    AuthorityCeiling{Workspace: worker.PermissionModeReadOnly, Session: worker.PermissionModeBypass},
			want: worker.PermissionModeReadOnly,
		},
		{
			name: "unreadable ceiling contributes nothing",
			c:    AuthorityCeiling{Workspace: "garbage", Session: worker.PermissionModeWorkspace},
			want: worker.PermissionModeWorkspace,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.wantZero, tc.c.IsZero())
			require.Equal(t, tc.want, tc.c.Effective())
		})
	}
}

func TestResolvePlan_ExplicitRequestAboveCeilingBlocks(t *testing.T) {
	t.Parallel()
	for _, ceiling := range []string{
		worker.PermissionModeReadOnly,
		worker.PermissionModeWorkspace,
		worker.PermissionModeAutoEdit,
	} {
		t.Run("workspace="+ceiling, func(t *testing.T) {
			t.Parallel()
			plan, err := testResolver().ResolvePlan(Input{
				Cfg:           claudeCfgWithDefault(worker.PermissionModeBypass),
				Platform:      "slack",
				BotName:       "b1",
				WorkspacePerm: ceiling,
				InitMeta:      InitMetadata{WorkerType: "claude_code", PermissionMode: worker.PermissionModeBypass},
			})

			require.ErrorIs(t, err, ErrPlanBlocked)
			require.True(t, hasBlockCode(plan, BlockPermissionExceedsCeiling))
			require.Empty(t, plan.PlanHash, "a blocked plan has no executable identity")
			require.Empty(t, plan.LaunchFingerprint)
		})
	}
}

func TestResolvePlan_ExplicitRequestWithinCeilingIsKept(t *testing.T) {
	t.Parallel()
	plan, err := testResolver().ResolvePlan(Input{
		Cfg:           claudeCfgWithDefault(worker.PermissionModeBypass),
		Platform:      "slack",
		BotName:       "b1",
		WorkspacePerm: worker.PermissionModeAutoEdit,
		InitMeta:      InitMetadata{WorkerType: "claude_code", PermissionMode: worker.PermissionModeWorkspace},
	})

	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeWorkspace, plan.AgentSpec.Policy.PermissionMode)
	require.True(t, plan.Coverage.PermissionMode)
	require.False(t, hasWarnCode(plan, WarnPermissionClampedToCeiling))
}

func TestResolvePlan_ConfigDefaultAboveCeilingClampsWithWarning(t *testing.T) {
	t.Parallel()
	// The ceiling comes from SessionPerm, which resolvePermissionMode never
	// consults as a preference — so the resolved tier really is the config
	// default and really is above the ceiling.
	plan, err := testResolver().ResolvePlan(Input{
		Cfg:         claudeCfgWithDefault(worker.PermissionModeBypass),
		Platform:    "slack",
		BotName:     "b1",
		SessionPerm: worker.PermissionModeWorkspace,
	})

	require.NoError(t, err, "a config default is a guess, not an explicit request: clamp, never block")
	require.Equal(t, worker.PermissionModeWorkspace, plan.AgentSpec.Policy.PermissionMode)
	require.True(t, hasWarnCode(plan, WarnPermissionClampedToCeiling))
}

func TestResolvePlan_SessionCeilingSurvivesAConfigWiden(t *testing.T) {
	t.Parallel()
	// The workspace ceiling is wide, but the session captured a narrow tier
	// when it started. A later config edit must not widen a live session.
	plan, err := testResolver().ResolvePlan(Input{
		Cfg:           claudeCfgWithDefault(worker.PermissionModeBypass),
		Platform:      "slack",
		BotName:       "b1",
		WorkspacePerm: worker.PermissionModeBypass,
		SessionPerm:   worker.PermissionModeReadOnly,
	})

	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeReadOnly, plan.AgentSpec.Policy.PermissionMode)
	require.True(t, hasWarnCode(plan, WarnPermissionClampedToCeiling))
}

func TestResolvePlan_NoCeilingLeavesTheTierAlone(t *testing.T) {
	t.Parallel()
	plan, err := testResolver().ResolvePlan(Input{
		Cfg:      claudeCfgWithDefault(worker.PermissionModeBypass),
		Platform: "slack",
		BotName:  "b1",
	})

	require.NoError(t, err)
	require.Equal(t, worker.PermissionModeBypass, plan.AgentSpec.Policy.PermissionMode)
	require.False(t, hasWarnCode(plan, WarnPermissionClampedToCeiling))
}

func TestResolvePlan_CeilingWithNoTierWarnsThatItIsUnenforced(t *testing.T) {
	t.Parallel()
	// claude_code always resolves a tier from config, so use a worker with no
	// permission source at all to reach the "ceiling exists, tier does not"
	// state that leaves the Worker default in charge.
	plan, err := testResolver().ResolvePlan(Input{
		Platform:    "webchat",
		SessionPerm: worker.PermissionModeReadOnly,
		InitMeta:    InitMetadata{WorkerType: "opencode_server"},
	})

	require.NoError(t, err)
	require.Empty(t, plan.AgentSpec.Policy.PermissionMode)
	require.True(t, hasWarnCode(plan, WarnPermissionCeilingUnenforced))
	require.False(t, plan.Coverage.PermissionMode, "an unresolved tier must be reported as uncovered")
}

func TestResolvePlan_UnverifiedRequiredCapabilityBlocks(t *testing.T) {
	t.Parallel()
	plan, err := testResolver().ResolvePlan(Input{
		Platform:             "webchat",
		InitMeta:             InitMetadata{WorkerType: "claude_code"},
		RequiredCapabilities: []string{"fs_isolation", "net_isolation"},
		VerifiedCapabilities: []string{"fs_isolation"},
	})

	require.ErrorIs(t, err, ErrPlanBlocked)
	require.True(t, hasBlockCode(plan, BlockRequiredCapabilityUnverified))
	require.Empty(t, plan.PlanHash)
}

func TestResolvePlan_VerifiedRequiredCapabilityPasses(t *testing.T) {
	t.Parallel()
	plan, err := testResolver().ResolvePlan(Input{
		Platform:             "webchat",
		InitMeta:             InitMetadata{WorkerType: "claude_code"},
		RequiredCapabilities: []string{"fs_isolation"},
		VerifiedCapabilities: []string{"fs_isolation", "net_isolation"},
	})

	require.NoError(t, err)
	require.NotEmpty(t, plan.PlanHash)
}

func TestResolve_NeverReturnsAnOverCeilingTier(t *testing.T) {
	t.Parallel()
	// Even the non-plan Resolve path — which has no way to report a blocked
	// plan — must never hand back the tier that was refused.
	spec, err := testResolver().Resolve(Input{
		Platform:      "webchat",
		WorkspacePerm: worker.PermissionModeReadOnly,
		InitMeta:      InitMetadata{WorkerType: "claude_code", PermissionMode: worker.PermissionModeBypass},
	})

	require.NoError(t, err)
	require.NotEqual(t, worker.PermissionModeBypass, spec.Policy.PermissionMode)
	require.LessOrEqual(t, PermissionRank(spec.Policy.PermissionMode), PermissionRank(worker.PermissionModeReadOnly))
}

func TestMissingRequiredCapabilities(t *testing.T) {
	t.Parallel()
	require.Nil(t, missingRequiredCapabilities(nil, nil))
	require.Nil(t, missingRequiredCapabilities([]string{"a"}, []string{"a", "b"}))
	require.Equal(t, []string{"b", "c"}, missingRequiredCapabilities([]string{"a", "b", "c"}, []string{"a"}))
	require.Nil(t, missingRequiredCapabilities([]string{""}, nil), "a blank requirement names nothing")
}

func TestApplyPermissionCeiling_UnreadableCeilingFallsBackToStrictest(t *testing.T) {
	t.Parallel()
	out := applyPermissionCeiling(worker.PermissionModeBypass,
		AuthorityCeiling{Workspace: "not-a-tier"}, false)

	require.Equal(t, worker.PermissionModeReadOnly, out.Effective)
	require.Equal(t, WarnPermissionClampedToCeiling, out.WarningCode)
}

func hasBlockCode(p EffectiveRuntimePlan, code string) bool {
	for _, b := range p.Blocked {
		if b.Code == code {
			return true
		}
	}
	return false
}

func hasWarnCode(p EffectiveRuntimePlan, code string) bool {
	for _, w := range p.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
