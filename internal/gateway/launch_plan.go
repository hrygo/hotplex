package gateway

// Launch plan preparation (#946 plan unit D2).
//
// createAndLaunchWorker is the single chokepoint every Worker launch passes
// through — fresh start, resume, reset and crash recovery all converge there.
// That makes it the one place where "the plan that describes this launch" can
// be resolved ONCE and bound to the run, instead of being re-derived by a
// diagnostic that happens to run later against a config that may since have
// changed.
//
// D2 is deliberately SHADOW. It changes no dispatch behaviour: the legacy
// parameters stay authoritative, the plan is compared against them, and the
// comparison is recorded as field enums plus equal/unequal. Switching any
// entry×Worker combination to authoritative is D3's job, and only for the
// combinations named in the rollout allowlists.

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

// Entry kinds a launch can originate from. The rollout allowlist is written in
// these terms, so adding a new entry is an explicit decision rather than an
// accidental side effect of reusing an existing platform string.
const (
	entryWebChat   = "webchat"
	entryMessaging = "messaging"
	entryCron      = "cron"
)

// Parity field names. These are ENUMS: they name which decision disagreed, not
// what the values were. A tool list, a path, an env value or a prompt must never
// appear here — the divergence is meant to be loggable forever.
const (
	parityFieldPermission    = "permission_mode"
	parityFieldAllowedTools  = "allowed_tools"
	parityFieldDisallowed    = "disallowed_tools"
	parityFieldSandbox       = "sandbox_mode"
	parityFieldAllowedModels = "allowed_models"
)

// launchPlan is the prepared plan for exactly one Worker launch.
type launchPlan struct {
	// Plan is the resolved desired state. A blocked plan is reported, never
	// launched from, in any rollout mode.
	Plan agentspec.EffectiveRuntimePlan

	// Fingerprint is the internal identity of Plan. It identifies the plan,
	// never proof that the Worker applied it.
	Fingerprint string

	// Mode is the rollout mode actually in force for THIS entry×Worker pair:
	// "shadow" unless the pair is named in both allowlists.
	Mode string

	// SessionID, Entry and WorkerType name the launch the plan describes.
	SessionID  string
	Entry      string
	WorkerType string

	// DivergedFields lists parity field enums that disagreed with the legacy
	// parameters. Empty means the plan agrees with what actually launched.
	DivergedFields []string

	// BlockedCodes carries bounded fail-closed reasons. Non-empty means the
	// plan must not be treated as an executable desired state.
	BlockedCodes []string
}

// Blocked reports whether the plan failed closed.
func (p launchPlan) Blocked() bool { return len(p.BlockedCodes) > 0 }

// Diverged reports whether the plan disagreed with the launched parameters.
func (p launchPlan) Diverged() bool { return len(p.DivergedFields) > 0 }

// prepareLaunchPlan resolves the runtime plan for one launch and compares it
// against the parameters that are about to be used.
//
// Two facts must not be confused here. The SESSION ceiling is immutable for
// the life of the session; the WORKSPACE ceiling is the current authorization.
// Both are passed as ceilings rather than as preferences, so the resolver's
// desired tier comes from config and gets clamped — a bridge does not take a
// permission REQUEST from a client, it derives the effective tier.
func (b *Bridge) prepareLaunchPlan(params workerLaunchParams) launchPlan {
	info := params.workerInfo
	out := launchPlan{
		SessionID:  info.SessionID,
		Entry:      entryKindFor(params.platform, params.facts.platformKey),
		WorkerType: string(params.wt),
	}

	cfg := b.currentConfig()
	// The plan must describe THIS launch, so the worker type actually being
	// launched is the top-precedence input rather than a re-resolution that
	// could disagree with the factory about to be called.
	in := agentspec.Input{
		Cfg: cfg,
		InitMeta: agentspec.InitMetadata{
			WorkerType:      string(params.wt),
			AllowedTools:    info.AllowedTools,
			DisallowedTools: info.DisallowedTools,
		},
		Platform:      params.platform,
		PlatformKey:   params.facts.platformKey,
		UserID:        info.UserID,
		WorkspaceID:   params.facts.workspaceID,
		WorkspacePerm: b.resolveWorkspacePermissionMode(params.facts.workspaceID),
		SessionPerm:   params.facts.sessionCeiling,
	}

	plan, err := (agentspec.Resolver{}).ResolvePlan(in)
	out.Plan = plan
	out.Fingerprint = plan.LaunchFingerprint
	for _, r := range plan.Blocked {
		out.BlockedCodes = append(out.BlockedCodes, r.Code)
	}
	if err != nil && len(out.BlockedCodes) == 0 {
		// A resolution failure that produced no bounded code is a defect in the
		// resolver, not a plan outcome. Report it as blocked rather than let an
		// unexplained plan through.
		out.BlockedCodes = []string{"plan_resolution_failed"}
	}
	out.Mode = b.planRolloutMode(out.Entry, out.WorkerType, cfg)

	// Parity is computed against the parameters that WILL launch, before any
	// mapping is applied, so the comparison answers "would the plan have changed
	// anything?" rather than "does the plan agree with itself?".
	out.DivergedFields = parityDivergences(plan.AgentSpec, info)

	b.recordLaunchPlan(out)
	return out
}

// currentConfig returns the live config snapshot, or nil when no provider was
// wired. A nil config is not an error: a minimal bridge resolves a partial plan
// and the coverage flags record exactly what is missing.
func (b *Bridge) currentConfig() *config.Config {
	if b.cfgProvider == nil {
		return nil
	}
	return b.cfgProvider()
}

// launchFactsFor lifts the session-scoped authority facts out of a session
// record. They live on session.SessionInfo rather than worker.SessionInfo
// because a Worker adapter has no use for a workspace id or a captured
// permission ceiling, and adding them would widen the Worker contract for a
// gateway-only concern.
func launchFactsFor(si *session.SessionInfo) launchFacts {
	if si == nil {
		return launchFacts{}
	}
	return launchFacts{
		workspaceID:    si.WorkspaceID,
		sessionCeiling: si.PermissionCeiling,
		platformKey:    si.PlatformKey,
	}
}

// launchFactsForStart lifts the same facts for a session that does not exist
// yet. There is no captured ceiling to carry — the session captures one after
// its first successful start — so the workspace ceiling is the only bound.
func launchFactsForStart(p *worker.SessionStartParams) launchFacts {
	if p == nil {
		return launchFacts{}
	}
	return launchFacts{
		workspaceID: p.WorkspaceID,
		platformKey: p.PlatformKey,
	}
}

// entryKindFor classifies a launch by origin.
func entryKindFor(platform string, platformKey map[string]string) string {
	if _, isCron := platformKey["cron_job_id"]; isCron {
		return entryCron
	}
	switch platform {
	case platformWebChat:
		return entryWebChat
	case "slack", "feishu", "yuanxin":
		return entryMessaging
	case "":
		return entryWebChat
	default:
		return entryMessaging
	}
}

// planRolloutMode decides the rollout mode for one entry×Worker pair.
//
// "authoritative" requires BOTH allowlists to name the pair. A half-configured
// rollout therefore stays shadow, which is the whole point: a partially rolled
// out plan must never report itself as live.
func (b *Bridge) planRolloutMode(entry, workerType string, cfg *config.Config) string {
	if cfg == nil {
		return config.RuntimePlanModeShadow
	}
	rollout := cfg.Worker.RuntimePlan
	if rollout.NormalizePlanRolloutMode() != config.RuntimePlanModeAuthoritative {
		return config.RuntimePlanModeShadow
	}
	if !slices.Contains(rollout.AuthoritativeEntries, entry) {
		return config.RuntimePlanModeShadow
	}
	if !slices.Contains(rollout.AuthoritativeWorkers, workerType) {
		return config.RuntimePlanModeShadow
	}
	return config.RuntimePlanModeAuthoritative
}

// parityDivergences compares the plan's effective decisions against the
// parameters that will actually launch, returning FIELD NAMES only.
//
// The comparison is deliberately one-directional: it asks whether the plan
// would have produced something different, not whether the legacy path is
// "correct". A field the plan did not resolve is NOT a divergence — that is
// reported through PlanCoverage instead, so an absent decision and a
// disagreeing decision stay distinguishable.
func parityDivergences(spec agentspec.AgentSpec, legacy worker.SessionInfo) []string {
	var out []string
	add := func(field string, diverged bool) {
		if diverged {
			out = append(out, field)
		}
	}

	add(parityFieldPermission, spec.Policy.PermissionMode != "" &&
		spec.Policy.PermissionMode != legacy.PermissionMode)
	add(parityFieldAllowedTools, spec.Policy.AllowedToolsSet &&
		!slices.Equal(spec.Policy.AllowedTools, legacy.AllowedTools))
	add(parityFieldDisallowed, spec.Policy.DisallowedToolsSet &&
		!slices.Equal(spec.Policy.DisallowedTools, legacy.DisallowedTools))
	add(parityFieldSandbox, spec.Sandbox.Mode != "" && spec.Sandbox.Mode != legacy.Sandbox)
	// Model has no parity field on purpose: worker.SessionInfo carries no
	// selected-model field at all, so there is no legacy value to disagree
	// with. Until one exists, comparing it would compare the plan against
	// nothing and always report agreement.
	//
	// Worker type is absent for the mirror-image reason: the plan takes the
	// launching worker type as its TOP-precedence input precisely so it
	// describes this launch, which makes agreement structural. A vacuous
	// "always equal" check reported as parity evidence is worse than no check.
	add(parityFieldAllowedModels, spec.Worker.AllowedModels != nil &&
		!slices.Equal(spec.Worker.AllowedModels, legacy.AllowedModels))

	return out
}

// recordLaunchPlan emits the bounded diagnostics and low-cardinality metrics
// for one prepared launch.
//
// Metric labels are limited to enums and rollout modes. The fingerprint is a
// digest of internal decisions — useful in logs for correlation, but never a
// metric label, because it has unbounded cardinality.
func (b *Bridge) recordLaunchPlan(p launchPlan) {
	ctx := b.shutdownCtx
	if ctx == nil {
		ctx = context.Background()
	}

	result := "ok"
	switch {
	case p.Blocked():
		result = "blocked"
	case p.Diverged():
		result = "diverged"
	}

	observability.RuntimePlanResolutions().Add(ctx, 1, metric.WithAttributes(
		attribute.String("entry", p.Entry),
		attribute.String("worker_type", p.WorkerType),
		attribute.String("mode", p.Mode),
		attribute.String("result", result),
	))
	for _, code := range p.BlockedCodes {
		observability.RuntimePlanBlocked().Add(ctx, 1, metric.WithAttributes(
			attribute.String("code", code)))
	}

	switch {
	case p.Blocked():
		b.log.Warn("launch plan: blocked",
			"session_id", p.SessionID,
			"entry", p.Entry,
			"worker_type", p.WorkerType,
			"blocked", p.BlockedCodes,
			"mode", p.Mode)
	case p.Diverged():
		b.log.Warn("launch plan: divergence from launched parameters",
			"session_id", p.SessionID,
			"entry", p.Entry,
			"worker_type", p.WorkerType,
			"fields", p.DivergedFields,
			"mode", p.Mode,
			"plan_fingerprint", p.Fingerprint)
	default:
		b.log.Debug("launch plan: parity",
			"session_id", p.SessionID,
			"entry", p.Entry,
			"worker_type", p.WorkerType,
			"mode", p.Mode,
			"plan_fingerprint", p.Fingerprint)
	}
}
