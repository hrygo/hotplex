package agentspec

import (
	"fmt"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
)

// Input carries every upstream fact the Resolver needs to normalize an
// AgentSpec. The caller is responsible for collecting it (including any
// already-resolved workspace permission override), so the Resolver stays a pure
// function with no store/global dependency — enabling table-driven tests and the
// WS≡REST equivalence proof.
type Input struct {
	Cfg      *config.Config // config snapshot (may be nil in minimal tests)
	InitMeta InitMetadata   // explicit per-request intent (WS init / REST create)

	// WorkspacePerm is the caller-resolved workspace permission ceiling override
	// ("" = no override). First-cut webchat callers pass "" — permission is
	// actually resolved in the bridge layer (finding F8); this field exists for
	// the contract, MapToSessionInfo tests, and the future bridge slice.
	WorkspacePerm string

	// SessionPerm is the permission ceiling captured when this session started.
	// Unlike WorkspacePerm it is immutable for the life of the session, so a
	// later config edit cannot widen what the session may now do.
	SessionPerm string

	// RequiredCapabilities lists capabilities the plan cannot run without;
	// VerifiedCapabilities lists the ones the caller could actually verify
	// against the backend. A requirement with no verification blocks the plan
	// (see BlockRequiredCapabilityUnverified) — an unenforced promise is not a
	// warning.
	RequiredCapabilities []string
	VerifiedCapabilities []string

	BotName     string
	Platform    string // "slack" | "feishu" | "yuanxin" | "webchat" | ""
	PlatformKey map[string]string

	// Identity references (for IdentityRefs; IDs only).
	UserID      string
	WorkspaceID string
}

// InitMetadata carries the explicit per-request override intent. A zero value
// ("" / nil) means "not declared → fall through to the next precedence level";
// the Resolver only applies top precedence to declared fields.
type InitMetadata struct {
	WorkerType      string
	PermissionMode  string
	AllowedTools    []string
	DisallowedTools []string
	Model           string
}

// Resolver derives an AgentSpec from an Input.
//
// The zero value is ready to use and validates worker types against the live
// worker registry (worker.ValidateType) — the same boundary the REST entry uses
// today. Because that registry is global mutable state, tests (and any caller
// wanting a hermetic/pure resolve) inject ValidateWorkerType with a static
// validator; permission-tier validation is already pure (a static map) and needs
// no injection.
type Resolver struct {
	// ValidateWorkerType validates a non-empty worker type. Nil → defaults to
	// the registry-backed worker.ValidateType.
	ValidateWorkerType func(workerType string) error
}

func (r Resolver) validateWorkerType(workerType string) error {
	if r.ValidateWorkerType != nil {
		return r.ValidateWorkerType(workerType)
	}
	return worker.ValidateType(worker.WorkerType(workerType))
}

// Resolve normalizes the input into an AgentSpec.
//
// Precedence per field: init-metadata explicit > per-bot override > workspace
// override (permission only) > platform config > messaging shared default >
// compile default.
//
// Boundary: a non-empty but unknown worker type / permission tier is rejected
// with an error (the normalized boundary rejection required by #847). Empty
// values are allowed through — the worker registry / bridge remain the ultimate
// boundary, matching current behavior (webchat passes an empty worker_type
// through today; REST defaults it before this layer).
func (r Resolver) Resolve(in Input) (AgentSpec, error) {
	spec, _, err := r.resolveWithAuthority(in)
	return spec, err
}

// resolveWithAuthority is Resolve plus the authority-clamp outcome. Resolve
// itself must stay a pure "what did the precedence chain produce" function so
// its equivalence proof keeps working; the clamp REASON is only interesting to
// the plan, which has to decide between a warning and a blocked plan.
func (r Resolver) resolveWithAuthority(in Input) (AgentSpec, permissionOutcome, error) {
	var spec AgentSpec

	// ── Worker ──────────────────────────────────────────────────────────────
	wt := in.InitMeta.WorkerType
	if wt == "" && isMessagingPlatform(in.Platform) && in.Cfg != nil {
		// Messaging sessions fall back through the documented 5-level chain.
		wt = in.Cfg.ResolveWorkerType(in.Platform, in.BotName)
	}
	if wt != "" {
		if err := r.validateWorkerType(wt); err != nil {
			return AgentSpec{}, permissionOutcome{}, fmt.Errorf("agentspec: worker type: %w", err)
		}
	}
	spec.Worker.Type = wt
	spec.Worker.Model = in.InitMeta.Model
	spec.Worker.ModelSet = in.InitMeta.Model != ""
	spec.Worker.Command = resolveCommand(in, wt)
	// AllowedModels is intentionally NOT injected (finding F1): doing so would
	// change webchat model visibility. The contract field stays nil in first-cut.

	// ── Policy ──────────────────────────────────────────────────────────────
	pm := resolvePermissionMode(in, wt)
	explicitPermission := in.InitMeta.PermissionMode != ""
	if pm != "" {
		if err := worker.ValidatePermissionMode(pm); err != nil {
			return AgentSpec{}, permissionOutcome{}, fmt.Errorf("agentspec: permission mode: %w", err)
		}
	}
	// The authority clamp runs AFTER precedence resolution and AFTER boundary
	// validation, so the tier a plan carries is always inside the ceiling.
	outcome := applyPermissionCeiling(pm, ceilingFor(in), explicitPermission)
	if outcome.BlockedCode != "" {
		// Refuse before the value reaches any caller. Plan resolution turns
		// this into a blocked plan; a direct Resolve caller gets the
		// strictest tier, never the one it asked for.
		pm = worker.PermissionModeReadOnly
	} else {
		pm = outcome.Effective
	}
	spec.Policy.PermissionMode = pm
	spec.Policy.AllowedTools = in.InitMeta.AllowedTools
	spec.Policy.AllowedToolsSet = in.InitMeta.AllowedTools != nil
	spec.Policy.DisallowedTools = in.InitMeta.DisallowedTools
	spec.Policy.DisallowedToolsSet = in.InitMeta.DisallowedTools != nil

	// ── Sandbox (contract; codex semantics baseline) ────────────────────────
	spec.Sandbox.Mode = resolveSandbox(in, wt)

	// ── Budget (contract; no first-cut init source → zero) ──────────────────

	// ── Identity ────────────────────────────────────────────────────────────
	spec.Identity = IdentityRefs{
		UserID:      in.UserID,
		WorkspaceID: in.WorkspaceID,
		BotName:     in.BotName,
		Platform:    in.Platform,
	}

	return spec, outcome, nil
}

// resolvePermissionMode mirrors the bridge's permission inputs for the plan
// projection. Explicit init and workspace values win; a workspace without an
// explicit override receives the configured global ceiling. Platform sessions
// without a workspace retain their worker-specific operator defaults.
func resolvePermissionMode(in Input, workerType string) string {
	if mode := in.InitMeta.PermissionMode; mode != "" {
		return mode
	}
	if mode := in.WorkspacePerm; mode != "" {
		return mode
	}
	if in.Cfg == nil {
		return ""
	}
	if in.WorkspaceID != "" {
		return worker.NormalizePermissionMode(in.Cfg.Worker.DefaultPermissionMode)
	}

	switch workerType {
	case string(worker.TypeClaudeCode):
		if mode := in.Cfg.Worker.ClaudeCode.PermissionMode; mode != "" {
			return mode
		}
		return worker.PermissionModeBypass
	case string(worker.TypeCodexCLI):
		return permissionModeFromCodexConfig(in.Cfg.Worker.CodexCLI.Sandbox, in.Cfg.Worker.CodexCLI.ApprovalMode)
	case string(worker.TypeOpenCodeSrv):
		return worker.PermissionModeBypass
	case string(worker.TypeACP):
		if in.Cfg.Worker.ACP.AutoApprove != nil && !*in.Cfg.Worker.ACP.AutoApprove {
			return worker.PermissionModeWorkspace
		}
		return worker.PermissionModeBypass
	default:
		return ""
	}
}

func permissionModeFromCodexConfig(sandbox, approval string) string {
	switch sandbox {
	case "read-only":
		return worker.PermissionModeReadOnly
	case "workspace-write":
		switch approval {
		case "never":
			return worker.PermissionModeAutoEdit
		case "on-request":
			return worker.PermissionModeWorkspace
		default:
			return worker.PermissionModeReadOnly
		}
	case "danger-full-access":
		if approval == "never" {
			return worker.PermissionModeBypass
		}
		return worker.PermissionModeReadOnly
	default:
		return worker.PermissionModeReadOnly
	}
}

// ceilingFor collects every permission upper bound that applies to this launch.
// The workspace override doubles as a ceiling when it is set: an owner may
// narrow a session below the operator default, never widen past it.
func ceilingFor(in Input) AuthorityCeiling {
	return AuthorityCeiling{
		Workspace: in.WorkspacePerm,
		Session:   in.SessionPerm,
	}
}

// isMessagingPlatform reports whether the platform resolves worker_type via the
// config 5-level fallback. WebChat (and "") are request-driven, not config-driven.
func isMessagingPlatform(platform string) bool {
	switch platform {
	case "slack", "feishu", "yuanxin":
		return true
	}
	return false
}

// resolveCommand returns the configured launch command for the resolved worker
// type (contract-only in first-cut). The ACP per-bot command override is honored
// via PlatformKey, mirroring the existing propagation path.
func resolveCommand(in Input, workerType string) string {
	if workerType == string(worker.TypeACP) {
		if cmd := in.PlatformKey[worker.ACPCommandPlatformKey]; cmd != "" {
			return cmd
		}
	}
	if in.Cfg == nil {
		return ""
	}
	switch workerType {
	case string(worker.TypeClaudeCode):
		return in.Cfg.Worker.ClaudeCode.Command
	case string(worker.TypeCodexCLI):
		return in.Cfg.Worker.CodexCLI.Command
	case string(worker.TypeOpenCodeSrv):
		return in.Cfg.Worker.OpenCodeServer.Command
	case string(worker.TypeACP):
		return in.Cfg.Worker.ACP.Command
	}
	return ""
}

// resolveSandbox returns the normalized sandbox mode (contract-only in first-cut).
// Only codex_cli carries a sandbox field; a per-bot override arrives via
// PlatformKey. Other workers express isolation through the permission tier.
func resolveSandbox(in Input, workerType string) string {
	if workerType != string(worker.TypeCodexCLI) {
		return ""
	}
	if sb := in.PlatformKey[worker.SandboxPlatformKey]; sb != "" {
		return sb
	}
	if in.Cfg != nil {
		return in.Cfg.Worker.CodexCLI.Sandbox
	}
	return ""
}
