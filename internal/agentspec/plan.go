package agentspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hrygo/hotplex/internal/worker"
)

// PlanVersion is the schema version of EffectiveRuntimePlan and its redacted
// view. Bump on any breaking change to the plan/view shape so consumers can
// refuse to interpret a plan they do not understand (same contract as
// SnapshotVersion).
const PlanVersion = 1

// LaunchFingerprintVersion versions the INTERNAL launch fingerprint — the
// identity of the decisions that will actually be applied at start. It is a
// separate version line from PlanVersion because the two answer different
// questions and change for different reasons.
const LaunchFingerprintVersion = 1

// PlanResolverID names the resolver implementation that produced a plan. It is
// part of the canonical hash input, so a future resolver generation produces a
// different plan identity for the same effective fields.
const PlanResolverID = "agentspec.Resolver"

// ErrPlanBlocked marks an EffectiveRuntimePlan that must not be treated as an
// executable success (#946 spec §6.2). Shadow callers log it as a redacted
// diagnostic; fail-closed entry surfaces surface it. The returned plan still
// carries the bounded Blocked reasons for inspection.
var ErrPlanBlocked = errors.New("agentspec: plan blocked")

// Blocked reason codes (fail-closed; #946 spec §6.2). A plan carrying any of
// these must never be reported as a clean, executable desired state.
const (
	// BlockUnknownWorkerType: a non-empty worker type the registry cannot verify.
	BlockUnknownWorkerType = "unknown_worker_type"
	// BlockInvalidPermissionMode: a permission tier outside the 4-level ceiling.
	BlockInvalidPermissionMode = "invalid_permission_mode"
	// BlockInvalidSandboxMode: a sandbox mode outside the codex-baseline vocabulary.
	BlockInvalidSandboxMode = "invalid_sandbox_mode"
	// BlockFactsMissingOrConflicting: workspace/owner/origin facts the resolver
	// cannot establish or reconcile.
	BlockFactsMissingOrConflicting = "facts_missing_or_conflicting"
	// BlockCapabilityUnverifiable: a capability the plan requires but cannot verify.
	BlockCapabilityUnverifiable = "capability_unverifiable"
	// BlockSecretShapedValue: a secret-shaped value was attempted at the public
	// plan boundary and rejected.
	BlockSecretShapedValue = "secret_shaped_value"
)

// Warning codes (compat behaviors that keep running but must stay visible;
// #946 spec §6.2). Capabilities whose enforcement cannot be proven only ever
// surface as warnings with partial/unavailable/unknown semantics — never as
// silent success.
const (
	// WarnWorkerTypeUnresolved: the plan could not pin a worker type; the worker
	// registry decides at dispatch, so the desired state is unproven.
	WarnWorkerTypeUnresolved = "worker_type_unresolved"
	// WarnPermissionSourceConflict: session init metadata declared a different
	// permission tier than the workspace ceiling; documented precedence resolved
	// it, but the divergence is recorded.
	WarnPermissionSourceConflict = "permission_source_conflict"
)

// Plan source labels for PlanSourceRef. They name the precedence level a
// resolved field value came from (#946 spec §6.2 chain).
const (
	PlanSourceInitMetadata      = "init_metadata"
	PlanSourceWorkspaceOverride = "workspace_override"
	PlanSourcePlatformConfig    = "platform_config"
	PlanSourceBotConfig         = "bot_config"
	PlanSourceBaseConfig        = "base_config"
)

// Observed bootstrap states (#946 spec §6.5). The first slice only ever
// produces ObservedPlanned; the remaining states exist so the Worker-bootstrap
// follow-up has a fixed vocabulary. A config hash, an HTTP 200, a Worker done,
// or an audit row alone never justify ObservedEnforced.
const (
	ObservedPlanned  = "planned"
	ObservedUnknown  = "unknown"
	ObservedDeclared = "declared"
	ObservedPartial  = "partial"
	ObservedEnforced = "enforced"
)

// knownSandboxModes is the codex-baseline sandbox vocabulary (the only sandbox
// semantics the resolver can verify today). Any other non-empty mode is
// fail-closed, not silently passed through.
var knownSandboxModes = map[string]struct{}{
	"read-only":          {},
	"workspace-write":    {},
	"danger-full-access": {},
}

// ObservedSummary is the bounded observed-bootstrap projection served
// alongside the desired-state plan (#946 spec §6.5/§6.6): only verifiable
// facts (worker type, the Worker-reported permission tier) plus the mapping
// state. A config hash, an HTTP 200, a Worker done, or an audit row alone
// never lift the state to ObservedEnforced.
type ObservedSummary struct {
	// State is one of the Observed* constants.
	State string `json:"state"`
	// WorkerType is the worker type the session actually runs (observed fact).
	WorkerType string `json:"worker_type,omitempty"`
	// PermissionCeiling is the first effective permission tier the Worker
	// reported after a successful start/resume — declared by the Worker, not
	// independently enforced (empty when never reported).
	PermissionCeiling string `json:"permission_ceiling,omitempty"`
}

// PlanSourceRef records which precedence level supplied a resolved field. The
// slice order is semantic (field precedence) and MUST NOT be reordered by
// canonicalization (#946 spec §6.3 rule 4).
type PlanSourceRef struct {
	Field  string `json:"field"`
	Source string `json:"source"`
}

// PlanWarning is a bounded compat warning attached to a plan.
type PlanWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PlanBlockReason is a bounded fail-closed reason attached to a plan.
type PlanBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EffectiveRuntimePlan is the desired-state runtime plan for a session (#946).
// It is a diagnostic projection, not an observed success: PlanHash identifies
// what the gateway WANTS to run, never proof that the Worker/backend/sandbox
// actually applied it. The full plan is internal; only Redacted() may reach
// Admin/doctor/public surfaces.
//
// First slice: EnvProfile/EnvKeys/CapabilityIDs/SkillHash/ConfigHash stay
// zero-valued — their sources (#867 strict env allowlist, capability registry,
// skill/config fingerprinting) are later iterations. The fields exist so the
// contract is stable.
type EffectiveRuntimePlan struct {
	Version       int
	Resolver      string
	PlanHash      string
	AgentSpec     AgentSpec
	EnvProfile    string
	EnvKeys       []string
	CapabilityIDs []string
	SkillHash     string
	ConfigHash    string
	SourceRefs    []PlanSourceRef
	Warnings      []PlanWarning
	Blocked       []PlanBlockReason

	// LaunchFingerprint is the INTERNAL identity of the decisions this plan
	// will actually apply. It is deliberately NOT the public PlanHash:
	//
	//   - PlanHash stays a redacted, publicly servable identity. It covers only
	//     what a public view may contain, so it changes when harmless
	//     presentation fields change.
	//   - LaunchFingerprint covers the execution-relevant decisions plus the
	//     coverage flags saying which ones were actually resolved.
	//
	// Neither may be used as an authorization or approval cache key: a
	// fingerprint identifies a PLAN, never proof that it was applied. Only the
	// observed bootstrap state (ObservedSummary) can support such a claim, and
	// that requires Worker-reported facts.
	LaunchFingerprint string

	// Coverage records which execution-relevant decisions this plan actually
	// pinned. An uncovered decision is not a zero value — it is an ABSENT one,
	// and the fingerprint must make that difference visible so two plans that
	// look identical cannot be confused.
	Coverage PlanCoverage
}

// PlanCoverage names the execution-relevant decisions a plan pinned. Every flag
// is a statement about the plan, not about the Worker: "resolved" never means
// "enforced".
type PlanCoverage struct {
	WorkerType      bool `json:"worker_type"`
	PermissionMode  bool `json:"permission_mode"`
	AllowedTools    bool `json:"allowed_tools"`
	DisallowedTools bool `json:"disallowed_tools"`
	Model           bool `json:"model"`
	SandboxMode     bool `json:"sandbox_mode"`
	Budget          bool `json:"budget"`
	EnvProfile      bool `json:"env_profile"`
	CapabilityIDs   bool `json:"capability_ids"`
	SkillMaterial   bool `json:"skill_material"`
	ConfigMaterial  bool `json:"config_material"`
}

// coverageOf reports which decisions the plan actually pinned. Presence, not
// emptiness, is the test: a caller that explicitly CLEARED the tool list
// resolved that decision, while a caller that never mentioned it did not.
func coverageOf(spec AgentSpec, plan EffectiveRuntimePlan) PlanCoverage {
	return PlanCoverage{
		WorkerType:      spec.Worker.Type != "",
		PermissionMode:  spec.Policy.PermissionMode != "",
		AllowedTools:    spec.Policy.AllowedToolsSet || spec.Policy.AllowedTools != nil,
		DisallowedTools: spec.Policy.DisallowedToolsSet || spec.Policy.DisallowedTools != nil,
		Model:           spec.Worker.ModelSet || spec.Worker.Model != "",
		SandboxMode:     spec.Sandbox.Mode != "",
		Budget:          spec.Budget.MaxTurns != 0 || spec.Budget.MaxBudgetUSD != 0,
		EnvProfile:      plan.EnvProfile != "",
		CapabilityIDs:   len(plan.CapabilityIDs) > 0,
		SkillMaterial:   plan.SkillHash != "",
		ConfigMaterial:  plan.ConfigHash != "",
	}
}

// EffectiveRuntimePlanView is the redacted public projection of a plan (#946
// spec §6.1): worker type, permission summary, sandbox summary, environment
// key NAMES, hashes, source refs, warnings and blocked codes — nothing else.
// Internal contract fields (launch command, model, tool lists, budget,
// identity refs, sandbox directories) deliberately never appear here, so a
// view is always safe to serve, log, and audit.
type EffectiveRuntimePlanView struct {
	Version         int               `json:"version"`
	Resolver        string            `json:"resolver"`
	PlanHash        string            `json:"plan_hash,omitempty"`
	WorkerType      string            `json:"worker_type,omitempty"`
	PermissionMode  string            `json:"permission_mode,omitempty"`
	SkipPermissions bool              `json:"skip_permissions,omitempty"`
	SandboxMode     string            `json:"sandbox_mode,omitempty"`
	EnvProfile      string            `json:"env_profile,omitempty"`
	EnvKeys         []string          `json:"env_keys,omitempty"`
	CapabilityIDs   []string          `json:"capability_ids,omitempty"`
	SkillHash       string            `json:"skill_hash,omitempty"`
	ConfigHash      string            `json:"config_hash,omitempty"`
	SourceRefs      []PlanSourceRef   `json:"source_refs,omitempty"`
	Warnings        []PlanWarning     `json:"warnings,omitempty"`
	Blocked         []PlanBlockReason `json:"blocked,omitempty"`
}

// ResolvePlan normalizes the input into a desired-state EffectiveRuntimePlan.
//
// It reuses Resolve (same precedence chain: init metadata > workspace override
// > platform/bot config > base config > compiled defaults) and then applies
// the fail-closed rules of #946 spec §6.2:
//
//   - unknown worker type / invalid permission mode (already rejected by
//     Resolve) become bounded Blocked reasons plus ErrPlanBlocked;
//   - a sandbox mode outside the verifiable vocabulary blocks the plan;
//   - unresolved worker type and init/workspace permission divergence surface
//     as warnings (compat keeps running, never silently).
//
// On any blocked outcome the returned error wraps ErrPlanBlocked and the
// returned plan carries the reasons but NO PlanHash — a blocked plan has no
// executable identity. Shadow callers (WS/REST) must log the redacted
// diagnostic and leave the legacy dispatch path untouched (#946 spec §6.4).
func (r Resolver) ResolvePlan(in Input) (EffectiveRuntimePlan, error) {
	plan := EffectiveRuntimePlan{Version: PlanVersion, Resolver: PlanResolverID}

	spec, authority, err := r.resolveWithAuthority(in)
	if err != nil {
		reason := blockReasonForResolveError(err)
		plan.Blocked = []PlanBlockReason{reason}
		return plan, fmt.Errorf("%w: %s: %w", ErrPlanBlocked, reason.Code, err)
	}
	plan.AgentSpec = spec

	// Authority is evaluated before any other desired-state rule: no
	// precedence answer can make an over-ceiling request legal.
	if authority.BlockedCode != "" {
		reason := PlanBlockReason{
			Code:    authority.BlockedCode,
			Message: boundedMessage("explicitly requested permission tier is above every ceiling for this session"),
		}
		plan.Blocked = []PlanBlockReason{reason}
		return plan, fmt.Errorf("%w: %s", ErrPlanBlocked, authority.BlockedCode)
	}
	if missing := missingRequiredCapabilities(in.RequiredCapabilities, in.VerifiedCapabilities); len(missing) > 0 {
		reason := PlanBlockReason{
			Code:    BlockRequiredCapabilityUnverified,
			Message: boundedMessage("required capability could not be verified against the backend"),
		}
		plan.Blocked = []PlanBlockReason{reason}
		return plan, fmt.Errorf("%w: %s", ErrPlanBlocked, BlockRequiredCapabilityUnverified)
	}

	if mode := spec.Sandbox.Mode; mode != "" {
		if _, ok := knownSandboxModes[mode]; !ok {
			reason := PlanBlockReason{
				Code:    BlockInvalidSandboxMode,
				Message: boundedMessage("sandbox mode cannot be verified: " + mode),
			}
			plan.Blocked = []PlanBlockReason{reason}
			return plan, fmt.Errorf("%w: %s: %s", ErrPlanBlocked, BlockInvalidSandboxMode, mode)
		}
	}

	plan.Warnings = planWarnings(in, spec)
	if authority.WarningCode != "" {
		plan.Warnings = append(plan.Warnings, PlanWarning{
			Code:    authority.WarningCode,
			Message: authorityWarningMessage(authority.WarningCode),
		})
	}
	plan.SourceRefs = planSourceRefs(in, spec)
	// Bind the plan to the config policy revision it was resolved against, so
	// a later config edit is visible as a different launch rather than an
	// invisible one. Only the secret-free allowlist feeds this hash.
	plan.ConfigHash = ConfigPolicyRevision(in.Cfg)
	plan.Coverage = coverageOf(spec, plan)
	plan.PlanHash = CanonicalPlanHash(plan.Redacted())
	plan.LaunchFingerprint = CanonicalLaunchFingerprint(plan)
	return plan, nil
}

// Redacted projects the plan onto its public, secret-free view. It is the
// single choke point for public surfaces (Admin API, doctor, logs): internal
// contract fields (Worker.Command, Model, tool lists, budget, identity refs,
// sandbox directories — paths are secret-shaped) are dropped here, and any
// env entry that does not look like a bare key NAME is rejected with a bounded
// blocked reason instead of being echoed.
func (p EffectiveRuntimePlan) Redacted() EffectiveRuntimePlanView {
	envKeys, blocked := scrubEnvKeys(p.EnvKeys)
	view := EffectiveRuntimePlanView{
		Version:         p.Version,
		Resolver:        p.Resolver,
		PlanHash:        p.PlanHash,
		WorkerType:      p.AgentSpec.Worker.Type,
		PermissionMode:  p.AgentSpec.Policy.PermissionMode,
		SkipPermissions: p.AgentSpec.Policy.SkipPermissions,
		SandboxMode:     p.AgentSpec.Sandbox.Mode,
		EnvProfile:      p.EnvProfile,
		EnvKeys:         envKeys,
		CapabilityIDs:   cloneStrings(p.CapabilityIDs),
		SkillHash:       p.SkillHash,
		ConfigHash:      p.ConfigHash,
		SourceRefs:      cloneSourceRefs(p.SourceRefs),
		Warnings:        cloneWarnings(p.Warnings),
		Blocked:         append(cloneBlocked(p.Blocked), blocked...),
	}
	return view
}

// CanonicalPlanHash returns the SHA-256 hex digest of the redacted view's
// canonical encoding (#946 spec §6.3). It is the desired-plan identity only —
// it never proves the Worker/backend/sandbox applied anything.
//
// Canonicalization rules:
//  1. fixed field set + version (struct declaration order);
//  2. nil and empty slices are normalized identically;
//  3. semantically unordered ID/key lists (EnvKeys, CapabilityIDs) are sorted;
//  4. semantically ordered lists (SourceRefs, Warnings, Blocked) keep order;
//  5. SHA-256 over the UTF-8 JSON encoding;
//  6. the PlanHash field itself is excluded (hash over the body, not itself).
//
// Prompts, metadata values, credentials, tokens, full commands, host env
// values, absolute paths, PIDs, timestamps and evidence refs can never enter
// the view, so they can never enter the hash.
func CanonicalPlanHash(view EffectiveRuntimePlanView) string {
	canon := view
	canon.PlanHash = ""
	canon.EnvKeys = normalizeStringList(view.EnvKeys)
	canon.CapabilityIDs = normalizeStringList(view.CapabilityIDs)
	canon.SourceRefs = cloneSourceRefs(view.SourceRefs)
	canon.Warnings = cloneWarnings(view.Warnings)
	canon.Blocked = cloneBlocked(view.Blocked)
	b, err := json.Marshal(canon)
	if err != nil {
		// The view holds only JSON-safe scalars and []string/struct slices; a
		// marshal failure is impossible in practice. An empty hash keeps callers
		// fail-safe rather than panicking in the request path.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blockReasonForResolveError maps a Resolve boundary rejection onto the
// bounded blocked-reason vocabulary.
func blockReasonForResolveError(err error) PlanBlockReason {
	msg := err.Error()
	code := BlockFactsMissingOrConflicting
	switch {
	case strings.Contains(msg, "worker type"):
		code = BlockUnknownWorkerType
	case strings.Contains(msg, "permission mode"):
		code = BlockInvalidPermissionMode
	}
	return PlanBlockReason{Code: code, Message: boundedMessage(msg)}
}

// planWarnings derives the compat warnings for a successfully resolved spec.
func planWarnings(in Input, spec AgentSpec) []PlanWarning {
	var warnings []PlanWarning
	if spec.Worker.Type == "" {
		warnings = append(warnings, PlanWarning{
			Code: WarnWorkerTypeUnresolved,
			Message: "worker type not resolved by the plan; the worker registry decides " +
				"at dispatch (desired state unproven)",
		})
	}
	if in.InitMeta.PermissionMode != "" && in.WorkspacePerm != "" &&
		in.InitMeta.PermissionMode != in.WorkspacePerm {
		warnings = append(warnings, PlanWarning{
			Code: WarnPermissionSourceConflict,
			Message: "session init permission tier differs from the workspace ceiling; " +
				"documented precedence applied",
		})
	}
	return warnings
}

// planSourceRefs records the precedence source of every resolved field in a
// fixed (semantic) field order.
func planSourceRefs(in Input, spec AgentSpec) []PlanSourceRef {
	refs := make([]PlanSourceRef, 0, 6)
	if spec.Worker.Type != "" {
		source := PlanSourcePlatformConfig // messaging 5-level config chain
		if in.InitMeta.WorkerType != "" {
			source = PlanSourceInitMetadata
		}
		refs = append(refs, PlanSourceRef{Field: "worker_type", Source: source})
	}
	if spec.Worker.Model != "" {
		refs = append(refs, PlanSourceRef{Field: "model", Source: PlanSourceInitMetadata})
	}
	if spec.Policy.PermissionMode != "" {
		source := PlanSourceBaseConfig
		switch {
		case in.InitMeta.PermissionMode != "":
			source = PlanSourceInitMetadata
		case in.WorkspacePerm != "":
			source = PlanSourceWorkspaceOverride
		}
		refs = append(refs, PlanSourceRef{Field: "permission_mode", Source: source})
	}
	if spec.Policy.AllowedTools != nil {
		refs = append(refs, PlanSourceRef{Field: "allowed_tools", Source: PlanSourceInitMetadata})
	}
	if spec.Policy.DisallowedTools != nil {
		refs = append(refs, PlanSourceRef{Field: "disallowed_tools", Source: PlanSourceInitMetadata})
	}
	if spec.Sandbox.Mode != "" {
		source := PlanSourceBaseConfig
		if in.PlatformKey[worker.SandboxPlatformKey] != "" {
			source = PlanSourceBotConfig
		}
		refs = append(refs, PlanSourceRef{Field: "sandbox_mode", Source: source})
	}
	return refs
}

// scrubEnvKeys keeps only bare environment key NAMES. Anything carrying a
// value (=), whitespace, quotes, or an implausible length is treated as a
// secret-shaped value attempt: dropped, and reported as a bounded blocked
// reason WITHOUT echoing the offending content.
func scrubEnvKeys(keys []string) (kept []string, blocked []PlanBlockReason) {
	for _, k := range keys {
		if isBareEnvKeyName(k) {
			kept = append(kept, k)
			continue
		}
		blocked = append(blocked, PlanBlockReason{
			Code:    BlockSecretShapedValue,
			Message: "secret-shaped value rejected at the public plan boundary",
		})
	}
	return normalizeStringList(kept), blocked
}

// isBareEnvKeyName reports whether s looks like an environment variable name
// (KEY_NAME), not a value.
func isBareEnvKeyName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	return !strings.ContainsAny(s, "= \t\n\r'\"`")
}

// normalizeStringList returns a sorted copy (nil for empty) so semantically
// unordered ID/key lists hash identically regardless of input order, and nil
// vs empty never differ (#946 spec §6.3 rules 2-3).
func normalizeStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

// cloneSourceRefs copies refs preserving order (semantic precedence; rule 4).
// A nil input yields nil so nil/empty normalize identically (rule 2).
func cloneSourceRefs(refs []PlanSourceRef) []PlanSourceRef {
	if len(refs) == 0 {
		return nil
	}
	return slices.Clone(refs)
}

// cloneWarnings copies warnings preserving resolve order (rule 4).
func cloneWarnings(warnings []PlanWarning) []PlanWarning {
	if len(warnings) == 0 {
		return nil
	}
	return slices.Clone(warnings)
}

// cloneBlocked copies blocked reasons preserving resolve order (rule 4).
func cloneBlocked(blocked []PlanBlockReason) []PlanBlockReason {
	if len(blocked) == 0 {
		return nil
	}
	return slices.Clone(blocked)
}

// boundedMessage truncates a human-facing diagnostic message so bounded
// reasons can never blow up audit/log/event payloads.
func boundedMessage(msg string) string {
	const max = 256
	if len(msg) <= max {
		return msg
	}
	return msg[:max]
}
