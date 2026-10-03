package agentspec

import (
	"github.com/hrygo/hotplex/internal/worker"
)

// MapToStartParams projects an AgentSpec onto a base worker.SessionStartParams,
// overlaying only the fields AgentSpec owns at the entry layer (WorkerType and
// AllowedTools). Every other field (ID/UserID/BotID/BotName/WorkDir/Platform/
// PlatformKey/Title/ClientKey/WorkspaceID/InjectExclude) is passed through from
// base untouched (see the design spec §3.4.1 ownership table).
//
// This is the only mapper wired into the live webchat entry path in first-cut
// (under shadow mode). It is idempotent: re-applying it to its own output is a
// no-op.
func MapToStartParams(spec AgentSpec, base worker.SessionStartParams) worker.SessionStartParams {
	out := base
	// WorkerType: AgentSpec owns and authoritatively sets it (the Resolver
	// already applied the full precedence + boundary validation).
	out.WorkerType = worker.WorkerType(spec.Worker.Type)
	// AllowedTools: presence decides, not emptiness. An explicit clear is a
	// decision ("allow nothing") and must not be mistaken for "inherit".
	if spec.Policy.AllowedToolsSet || spec.Policy.AllowedTools != nil {
		out.AllowedTools = spec.Policy.AllowedTools
	}
	return out
}

// MapToSessionInfo projects an AgentSpec onto a base worker.SessionInfo,
// overlaying only AgentSpec-owned fields and never touching preserved-path
// fields (resume family, MCPConfig, SystemPrompt*, ConfigEnv, ConfigBlocklist,
// ACPCommand, WorkerSessionID, SessionID, UserID, ProjectDir, Env, Args).
//
// First-cut provides and unit-tests this as a CONTRACT; it is NOT wired into the
// live path (SessionInfo is built in the shared bridge layer — finding F8). The
// bridge-integration is a follow-up slice.
//
// Overlay rules (design spec §3.4.1): a zero/empty/nil AgentSpec field means "do
// not override" — the base value is preserved. This keeps the mapper idempotent
// and guarantees it can never clobber a field it does not own.
func MapToSessionInfo(spec AgentSpec, base worker.SessionInfo) worker.SessionInfo {
	out := base

	// Policy-owned.
	if spec.Policy.PermissionMode != "" {
		out.PermissionMode = spec.Policy.PermissionMode
	}
	out.SkipPermissions = spec.Policy.SkipPermissions
	if spec.Policy.AllowedToolsSet || spec.Policy.AllowedTools != nil {
		out.AllowedTools = spec.Policy.AllowedTools
	}
	if spec.Policy.DisallowedToolsSet || spec.Policy.DisallowedTools != nil {
		out.DisallowedTools = spec.Policy.DisallowedTools
	}

	// Worker-owned. AllowedModels is NOT injected in first-cut (F1): only a
	// non-nil slice overrides, and the Resolver leaves it nil.
	if spec.Worker.AllowedModels != nil {
		out.AllowedModels = spec.Worker.AllowedModels
	}

	// Sandbox-owned.
	if spec.Sandbox.Mode != "" {
		out.Sandbox = spec.Sandbox.Mode
	}
	if spec.Sandbox.AllowedDirs != nil {
		out.AllowedDirs = spec.Sandbox.AllowedDirs
	}

	// Budget-owned (non-zero overrides).
	if spec.Budget.MaxTurns != 0 {
		out.MaxTurns = spec.Budget.MaxTurns
	}
	if spec.Budget.MaxBudgetUSD != 0 {
		out.MaxBudgetUSD = spec.Budget.MaxBudgetUSD
	}

	return out
}
