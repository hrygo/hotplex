package agentspec

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
)

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func webchatInput(mutate func(*Input)) Input {
	in := Input{
		Platform: "webchat",
		InitMeta: InitMetadata{WorkerType: "claude_code"},
	}
	if mutate != nil {
		mutate(&in)
	}
	return in
}

func planFor(t *testing.T, in Input) EffectiveRuntimePlan {
	t.Helper()
	plan, err := testResolver().ResolvePlan(in)
	require.NoError(t, err)
	return plan
}

func TestLaunchFingerprint_IsAStableDigest(t *testing.T) {
	t.Parallel()
	a := planFor(t, webchatInput(func(in *Input) { in.InitMeta.AllowedTools = []string{"Bash"} }))
	b := planFor(t, webchatInput(func(in *Input) { in.InitMeta.AllowedTools = []string{"Bash"} }))

	require.Regexp(t, fingerprintRe, a.LaunchFingerprint)
	require.Equal(t, a.LaunchFingerprint, b.LaunchFingerprint)
	require.NotEqual(t, a.PlanHash, a.LaunchFingerprint,
		"the internal fingerprint must be a distinct identity, not the public hash")
}

func TestLaunchFingerprint_ChangesWithEachExecutionRelevantDecision(t *testing.T) {
	t.Parallel()
	base := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{"Bash"}
		in.InitMeta.Model = "model-a"
	}))

	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"model", func(in *Input) { in.InitMeta.Model = "model-b" }},
		{"allowed tools narrowed", func(in *Input) { in.InitMeta.AllowedTools = []string{"Read"} }},
		{"allowed tools cleared", func(in *Input) { in.InitMeta.AllowedTools = nil }},
		{"disallowed tools added", func(in *Input) { in.InitMeta.DisallowedTools = []string{"Bash"} }},
		{"permission tier", func(in *Input) { in.InitMeta.PermissionMode = worker.PermissionModeReadOnly }},
		{"worker type", func(in *Input) { in.InitMeta.WorkerType = "codex_cli" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := webchatInput(func(in *Input) {
				in.InitMeta.AllowedTools = []string{"Bash"}
				in.InitMeta.Model = "model-a"
				tc.mutate(in)
			})
			require.NotEqual(t, base.LaunchFingerprint, planFor(t, in).LaunchFingerprint)
		})
	}
}

// TestPublicHashCannotStandInForTheFingerprint is the reason D1 exists: the
// redacted view omits tool lists (they are secret-shaped), so two runs that
// differ in exactly the field deciding what the Agent may do would share a
// public identity. The internal fingerprint must not.
func TestPublicHashCannotStandInForTheFingerprint(t *testing.T) {
	t.Parallel()
	restricted := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{"Read"}
	}))
	unrestricted := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{"Read", "Write", "Bash"}
	}))

	require.Equal(t, restricted.PlanHash, unrestricted.PlanHash,
		"the public hash genuinely cannot see tool lists — that is the gap being closed")
	require.NotEqual(t, restricted.LaunchFingerprint, unrestricted.LaunchFingerprint)
}

func TestLaunchFingerprint_DistinguishesExplicitClearFromInheritance(t *testing.T) {
	t.Parallel()
	explicitClear := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{}
	}))
	inherited := planFor(t, webchatInput(nil))

	require.True(t, explicitClear.Coverage.AllowedTools,
		"an explicit clear is a resolved decision")
	require.False(t, inherited.Coverage.AllowedTools,
		"no declaration means the decision was not made here")
	require.NotEqual(t, explicitClear.LaunchFingerprint, inherited.LaunchFingerprint)
}

func TestLaunchFingerprint_TracksCoverageNotJustValues(t *testing.T) {
	t.Parallel()
	cleared := EffectiveRuntimePlan{
		AgentSpec: AgentSpec{Policy: PolicySpec{AllowedToolsSet: true}},
		Coverage:  PlanCoverage{AllowedTools: true},
	}
	absent := EffectiveRuntimePlan{
		AgentSpec: AgentSpec{Policy: PolicySpec{}},
		Coverage:  PlanCoverage{},
	}

	require.NotEqual(t, CanonicalLaunchFingerprint(cleared), CanonicalLaunchFingerprint(absent))
}

func TestLaunchFingerprint_NeverReachesThePublicView(t *testing.T) {
	t.Parallel()
	plan := planFor(t, webchatInput(func(in *Input) { in.InitMeta.AllowedTools = []string{"Bash"} }))
	require.NotEmpty(t, plan.LaunchFingerprint)

	raw, err := json.Marshal(plan.Redacted())
	require.NoError(t, err)
	require.NotContains(t, string(raw), plan.LaunchFingerprint)
	require.NotContains(t, string(raw), "fingerprint")
}

func TestLaunchFingerprint_BlockedPlansHaveNone(t *testing.T) {
	t.Parallel()
	plan, err := testResolver().ResolvePlan(Input{
		Platform:      "webchat",
		WorkspacePerm: worker.PermissionModeReadOnly,
		InitMeta:      InitMetadata{WorkerType: "claude_code", PermissionMode: worker.PermissionModeBypass},
	})

	require.ErrorIs(t, err, ErrPlanBlocked)
	require.Empty(t, plan.LaunchFingerprint, "a blocked plan has nothing to launch")
}

func TestLaunchFingerprint_OrderIndependentForUnorderedLists(t *testing.T) {
	t.Parallel()
	a := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{"Read", "Write"}
		in.InitMeta.DisallowedTools = []string{"Bash", "Edit"}
	}))
	b := planFor(t, webchatInput(func(in *Input) {
		in.InitMeta.AllowedTools = []string{"Write", "Read"}
		in.InitMeta.DisallowedTools = []string{"Edit", "Bash"}
	}))

	require.Equal(t, a.LaunchFingerprint, b.LaunchFingerprint)
}

func TestConfigPolicyRevision_TracksPolicyOnly(t *testing.T) {
	t.Parallel()
	base := ConfigPolicyRevision(config.Default())
	require.Regexp(t, fingerprintRe, base)

	next := config.Default()
	next.Worker.CodexCLI.Sandbox = "read-only"
	require.NotEqual(t, base, ConfigPolicyRevision(next),
		"a real policy change must move the revision")

	rotated := config.Default()
	rotated.Messaging.Feishu.AppSecret = "rotated-secret-value"
	rotated.Messaging.Slack.BotToken = "xoxb-rotated"
	require.Equal(t, base, ConfigPolicyRevision(rotated),
		"folding credentials in would make every key rotation a new session identity")

	require.Empty(t, ConfigPolicyRevision(nil))
}

func TestPlanCoverage_ReportsAbsenceNotZeroValues(t *testing.T) {
	t.Parallel()
	plan := planFor(t, webchatInput(func(in *Input) {
		in.Cfg = config.Default()
		in.InitMeta.AllowedTools = []string{"Bash"}
	}))

	require.True(t, plan.Coverage.WorkerType)
	require.True(t, plan.Coverage.AllowedTools)
	require.False(t, plan.Coverage.DisallowedTools)
	require.False(t, plan.Coverage.Model)
	require.False(t, plan.Coverage.Budget)
	require.False(t, plan.Coverage.SkillMaterial)
	require.True(t, plan.Coverage.ConfigMaterial, "the config revision is bound into the plan")
	require.Regexp(t, fingerprintRe, plan.ConfigHash)

	// With no config snapshot at all the revision is genuinely absent, and the
	// coverage flag says so rather than implying a binding that never happened.
	require.False(t, planFor(t, webchatInput(nil)).Coverage.ConfigMaterial)
}

func TestOwnershipTable_NamesTheRulesThatMustNotBreak(t *testing.T) {
	t.Parallel()
	for _, field := range OwnedFields() {
		o, ok := OwnershipFor(field)
		require.True(t, ok)
		require.NotEmpty(t, o.Note, "%s must record the rule a future change must not break", field)
	}

	perm, ok := OwnershipFor("permission_mode")
	require.True(t, ok)
	require.NotEmpty(t, perm.ClampTo, "permission_mode is the clamped field")

	model, ok := OwnershipFor("model")
	require.True(t, ok)
	require.Contains(t, model.Note, "AllowedModels",
		"the table must state that a whitelist is never the selection")

	_, ok = OwnershipFor("no_such_field")
	require.False(t, ok)
}
