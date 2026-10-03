package agentspec

// The internal launch fingerprint (#946 plan unit D1).
//
// The public PlanHash answers "which redacted plan is this". It cannot answer
// "which decisions will actually be applied at start", because the redacted
// view deliberately omits everything execution-relevant that might be
// secret-shaped (commands, tool lists, paths). Binding a Worker launch to the
// public hash would mean two runs that differ in a tool list look identical.
//
// So the fingerprint is a second, INTERNAL identity computed over the
// execution-relevant decisions plus explicit coverage flags. Two rules keep it
// honest:
//
//  1. It is a fingerprint of a PLAN. It never asserts that the Worker, backend
//     or sandbox applied anything — only an ObservedSummary backed by
//     Worker-reported facts can support that claim. Nothing may use this value
//     as an authorization or approval cache key.
//  2. It is built from an ALLOWLIST of non-secret fields, never by hashing a
//     whole config. Hashing a struct that holds API keys would fold credential
//     rotation into the identity of every session and make the digest a
//     (weak) oracle for low-entropy values.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/hrygo/hotplex/internal/config"
)

// configPolicyRevision is the explicit, secret-free allowlist of config fields
// that change the EFFECTIVE policy of a launch. Free text, commands, paths, env
// values and credentials are excluded on purpose: they are either
// secret-shaped or not part of "which policy applies".
type configPolicyRevision struct {
	DefaultPermissionMode string `json:"default_permission_mode"`
	ClaudePermissionMode  string `json:"claude_permission_mode"`
	CodexSandbox          string `json:"codex_sandbox"`
	CodexApprovalMode     string `json:"codex_approval_mode"`
	ACPAutoApprove        *bool  `json:"acp_auto_approve"`
}

// ConfigPolicyRevision returns the secret-free revision of the config fields
// that participate in effective policy. Returns "" when cfg is nil.
func ConfigPolicyRevision(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	b, err := json.Marshal(configPolicyRevision{
		DefaultPermissionMode: cfg.Worker.DefaultPermissionMode,
		ClaudePermissionMode:  cfg.Worker.ClaudeCode.PermissionMode,
		CodexSandbox:          cfg.Worker.CodexCLI.Sandbox,
		CodexApprovalMode:     cfg.Worker.CodexCLI.ApprovalMode,
		ACPAutoApprove:        cfg.Worker.ACP.AutoApprove,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fingerprintBody is the canonical input of the launch fingerprint.
type fingerprintBody struct {
	Version    int
	Resolver   string
	WorkerType string
	// Model is the REQUESTED model only. AllowedModels is a permission
	// whitelist — it says what may be used, never what is used, so folding it
	// in here would make a pure authorization change look like a retarget.
	Model    string
	ModelSet bool

	PermissionMode     string
	SkipPermissions    bool
	AllowedTools       []string
	AllowedToolsSet    bool
	DisallowedTools    []string
	DisallowedToolsSet bool

	SandboxMode  string
	MaxTurns     int
	MaxBudgetUSD float64

	EnvProfile    string
	EnvKeys       []string
	CapabilityIDs []string
	SkillHash     string
	ConfigHash    string

	Coverage PlanCoverage
}

// CanonicalLaunchFingerprint returns the SHA-256 hex digest of the plan's
// execution-relevant decisions. Canonicalization follows the same rules as
// CanonicalPlanHash (fixed field set, nil≡empty, unordered lists sorted,
// fingerprint field excluded) so the digest is stable across processes.
func CanonicalLaunchFingerprint(p EffectiveRuntimePlan) string {
	spec := p.AgentSpec
	body := fingerprintBody{
		Version:            LaunchFingerprintVersion,
		Resolver:           p.Resolver,
		WorkerType:         spec.Worker.Type,
		Model:              spec.Worker.Model,
		ModelSet:           spec.Worker.ModelSet,
		PermissionMode:     spec.Policy.PermissionMode,
		SkipPermissions:    spec.Policy.SkipPermissions,
		AllowedTools:       normalizeStringList(spec.Policy.AllowedTools),
		AllowedToolsSet:    spec.Policy.AllowedToolsSet,
		DisallowedTools:    normalizeStringList(spec.Policy.DisallowedTools),
		DisallowedToolsSet: spec.Policy.DisallowedToolsSet,
		SandboxMode:        spec.Sandbox.Mode,
		MaxTurns:           spec.Budget.MaxTurns,
		MaxBudgetUSD:       spec.Budget.MaxBudgetUSD,
		EnvProfile:         p.EnvProfile,
		EnvKeys:            normalizeStringList(p.EnvKeys),
		CapabilityIDs:      normalizeStringList(p.CapabilityIDs),
		SkillHash:          p.SkillHash,
		ConfigHash:         p.ConfigHash,
		Coverage:           p.Coverage,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FieldOwnership is the single table of "which layer owns this decision".
// It exists so a second resolver generation does not have to re-derive the
// precedence chain from source: the table names, per field, who may DECIDE,
// who may CLAMP, and which non-obvious rule a future change must not break.
type FieldOwnership struct {
	Field string
	// Deciders lists the precedence layers allowed to supply a value, most
	// specific first.
	Deciders []string
	// ClampTo names the ceiling that bounds the field, or "" when unbounded.
	ClampTo string
	// Note records the non-obvious rule a future change must not break.
	Note string
}

// OwnershipTable is the authoritative field-ownership table for a plan.
var OwnershipTable = []FieldOwnership{
	{
		Field:    "worker_type",
		Deciders: []string{PlanSourceInitMetadata, PlanSourceBotConfig, PlanSourcePlatformConfig, PlanSourceBaseConfig},
		Note: "config-driven only for messaging platforms; webchat is request-driven. " +
			"An absent value stays uncovered and the registry decides at dispatch.",
	},
	{
		Field:    "permission_mode",
		Deciders: []string{PlanSourceInitMetadata, PlanSourceWorkspaceOverride, PlanSourceBaseConfig},
		ClampTo:  "workspace ceiling / session-captured ceiling",
		Note: "resolve FIRST, clamp SECOND. An explicit request above the ceiling is " +
			"blocked; a config default above it is clamped with a warning.",
	},
	{
		Field:    "model",
		Deciders: []string{PlanSourceInitMetadata},
		Note:     "the REQUESTED model only. AllowedModels is an authorization list and must never be read as the selection.",
	},
	{
		Field:    "allowed_tools",
		Deciders: []string{PlanSourceInitMetadata},
		Note:     "presence flag distinguishes an explicit clear from inheritance.",
	},
	{
		Field:    "disallowed_tools",
		Deciders: []string{PlanSourceInitMetadata},
		Note:     "presence flag distinguishes an explicit clear from inheritance.",
	},
	{
		Field:    "sandbox_mode",
		Deciders: []string{PlanSourceBotConfig, PlanSourceBaseConfig},
		Note:     "codex vocabulary only; anything else blocks rather than passes through.",
	},
	{
		Field:    "budget",
		Deciders: []string{},
		Note:     "no init source yet, so the coverage flag stays false and the fingerprint records the absence.",
	},
	{
		Field:    "env_profile",
		Deciders: []string{PlanSourceBaseConfig},
		Note:     "key NAMES only. A value at this boundary is rejected as secret-shaped, not hashed.",
	},
	{
		Field:    "capability_ids",
		Deciders: []string{PlanSourceInitMetadata},
		ClampTo:  "verified capability evidence",
		Note:     "a required capability with no verification blocks the plan.",
	},
}

// OwnershipFor returns the ownership record for a field, if the table names it.
func OwnershipFor(field string) (FieldOwnership, bool) {
	for _, o := range OwnershipTable {
		if o.Field == field {
			return o, true
		}
	}
	return FieldOwnership{}, false
}

// OwnedFields lists the fields the ownership table covers, in table order.
func OwnedFields() []string {
	fields := make([]string, 0, len(OwnershipTable))
	for _, o := range OwnershipTable {
		fields = append(fields, o.Field)
	}
	return fields
}
