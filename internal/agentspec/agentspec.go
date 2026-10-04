// Package agentspec defines AgentSpec — a normalized, read-only, secret-free
// view of an agent's runtime options, plus a pure Resolver that derives it from
// the fragmented upstream sources (config / WS-REST init metadata / workspace
// override / bot-platform config) and mappers that project it back onto the
// existing worker.SessionStartParams / worker.SessionInfo without changing the
// Worker interface.
//
// It is the dependency-chain root of the v2 roadmap (#847): #848 (identity),
// #849 (runtime events), #851 (queue), #852 (context) and #866 (snapshot) all
// consume AgentSpec fields. The package exists to give those consumers a stable
// import target and to keep normalization isolated from gateway connection
// lifecycle code (see the design spec §3.1 — the package is a preference, not an
// import-cycle necessity).
//
// First-cut scope (#847): the contract (types + Resolver + mappers) is complete
// and unit-tested; only MapToStartParams is wired into the live webchat entry
// path under shadow mode. MapToSessionInfo is provided and tested as a contract
// but NOT wired (SessionInfo is built in the shared bridge layer; wiring it is a
// follow-up slice — see the design spec §3.5/§9, finding F8).
package agentspec

// AgentSpec is the normalized, read-only, secret-free view of an agent runtime.
//
// Secret-free invariant: no field may carry an API key, env value, or
// credential. ConfigEnv / credential-bearing fields deliberately stay out of
// AgentSpec (they remain in the SessionInfo construction stage). An AgentSpec is
// therefore always safe to log and audit.
type AgentSpec struct {
	Worker   WorkerSpec   // provider / worker selection and command
	Policy   PolicySpec   // permission / tool boundaries
	Sandbox  SandboxSpec  // filesystem / network isolation
	Budget   BudgetSpec   // resource ceilings
	Identity IdentityRefs // identity references (IDs only, non-sensitive; the identity value object is #848)
}

// WorkerSpec captures provider/worker selection.
type WorkerSpec struct {
	// Type is the normalized worker type (claude_code | opencode_server |
	// codex_cli | acp). Empty means "not resolved here" (e.g. a webchat session
	// whose client sent no worker_type and whose downstream boundary decides).
	Type string
	// Command is the resolved launch command (contract; includes per-bot
	// override where applicable). First-cut: informational, not wired.
	Command string
	// Model is the requested/default model (contract).
	Model string
	// ModelSet records that Model was explicitly requested. It is what
	// separates "the caller asked for this model" from "no model was named",
	// which a zero-value string cannot express — and it is deliberately NOT
	// derived from AllowedModels: a whitelist says which models are permitted,
	// never which one is selected.
	ModelSet bool
	// AllowedModels lists models allowed for the session. First-cut does NOT
	// inject this (finding F1 — injection is a behavior change); contract only.
	AllowedModels []string
}

// PolicySpec captures permission and tool boundaries.
type PolicySpec struct {
	// PermissionMode is the normalized 4-tier ceiling:
	// read-only | workspace | auto-edit | bypass. Empty means "worker default"
	// (the bridge leaves it empty when no explicit override exists; each worker
	// then applies its own default). AgentSpec produces the tier only — the
	// tier→worker-native translation stays inline in each worker adapter
	// (finding F2).
	PermissionMode string
	// SkipPermissions bypasses all permission checks.
	SkipPermissions bool
	// AllowedTools is the tool whitelist; nil = no restriction.
	AllowedTools []string
	// AllowedToolsSet records an explicit declaration, including an explicit
	// CLEAR. A nil slice alone cannot distinguish "inherit" from "allow
	// nothing", and a mapper that treats nil as inherit silently drops the
	// second case.
	AllowedToolsSet bool
	// DisallowedTools is the tool blacklist; nil = none.
	DisallowedTools []string
	// DisallowedToolsSet is the presence counterpart of AllowedToolsSet.
	DisallowedToolsSet bool
}

// SandboxSpec captures filesystem/network isolation (codex semantics as the
// baseline vocabulary).
type SandboxSpec struct {
	// Mode is one of read-only | workspace-write | danger-full-access (contract).
	Mode string
	// AllowedDirs lists additional directories the worker may access.
	AllowedDirs []string
}

// BudgetSpec captures resource ceilings.
type BudgetSpec struct {
	MaxTurns     int
	MaxBudgetUSD float64
}

// IdentityRefs carries identity references (IDs only — never secrets). The
// AgentIdentity value object is #848; AgentSpec holds only the raw references.
type IdentityRefs struct {
	UserID      string
	WorkspaceID string
	BotName     string
	Platform    string
}
