package agentspec

// Authority-aware resolution: what a plan is ALLOWED to ask for.
//
// The precedence chain in resolve.go answers "which value won". It does not
// answer the question that actually matters at launch: "is that value inside
// what this session was authorized to do". A request that arrives with a high
// permission tier is still a request — the resolver used to hand it straight
// through, so a client could name a tier no operator or workspace ever
// granted.
//
// The rule implemented here is deliberately asymmetric:
//
//   - a tier that came from CONFIG (worker/platform defaults) is silently
//     clamped down to the ceiling, because the ceiling is the operator's
//     decision and the config default is merely a guess;
//   - a tier the caller EXPLICITLY asked for and cannot have is BLOCKED,
//     because quietly downgrading an explicit request is how a user ends up
//     believing they got something they did not get.
//
// Neither path can produce a plan more permissive than the ceiling.

import (
	"github.com/hrygo/hotplex/internal/worker"
)

const (
	// BlockPermissionExceedsCeiling: an explicitly requested permission tier is
	// above every ceiling that applies to this session. Fail-closed: the
	// request is refused, never downgraded on the caller's behalf.
	BlockPermissionExceedsCeiling = "permission_exceeds_ceiling"

	// BlockRequiredCapabilityUnverified: the plan requires a capability the
	// caller could not verify against the backend. A required capability that
	// nobody can prove is not a warning — it is an unenforced promise.
	BlockRequiredCapabilityUnverified = "required_capability_unverified"

	// WarnPermissionClampedToCeiling: a config-derived tier was above the
	// ceiling and got clamped down. Compat keeps running, but the divergence
	// is visible.
	WarnPermissionClampedToCeiling = "permission_clamped_to_ceiling"

	// WarnPermissionCeilingUnenforced: a ceiling exists but the resolved tier
	// is empty, so the tier is decided by the Worker default and the ceiling
	// is not enforced by this plan at all.
	WarnPermissionCeilingUnenforced = "permission_ceiling_unenforced"
)

// AuthorityCeiling is the set of permission upper bounds that apply to a
// launch. Every non-empty field is an independent upper bound; the effective
// ceiling is the tightest of them.
type AuthorityCeiling struct {
	// Workspace is the workspace-level ceiling (owner/admin granted).
	Workspace string
	// Session is the tier captured when the session started. It is immutable
	// for the life of the session, so a later config edit cannot widen it.
	Session string
}

// IsZero reports whether no ceiling applies at all.
func (c AuthorityCeiling) IsZero() bool {
	return c.Workspace == "" && c.Session == ""
}

// Effective returns the tightest ceiling, or "" when none applies. An
// unparseable ceiling is skipped rather than trusted: it cannot lower the
// result, and the caller separately validates tiers it resolves itself.
//
// Every field is an UPPER bound, so the answer is the MINIMUM rank, not the
// maximum. Taking the maximum would let a wide workspace ceiling erase a
// narrow session ceiling — the exact escalation the session capture exists
// to prevent.
func (c AuthorityCeiling) Effective() string {
	best := ""
	bestRank := -1
	for _, mode := range []string{c.Workspace, c.Session} {
		rank := PermissionRank(mode)
		if rank < 0 {
			continue
		}
		if bestRank < 0 || rank < bestRank {
			best, bestRank = mode, rank
		}
	}
	return best
}

// PermissionRank orders the four permission tiers by permissiveness. An
// unknown or empty tier ranks -1 so it can never tighten a ceiling and never
// counts as "more permissive" — validation of the tier itself is a separate
// concern handled by the resolver boundary.
func PermissionRank(mode string) int {
	switch mode {
	case worker.PermissionModeReadOnly:
		return 0
	case worker.PermissionModeWorkspace:
		return 1
	case worker.PermissionModeAutoEdit:
		return 2
	case worker.PermissionModeBypass:
		return 3
	default:
		return -1
	}
}

// permissionOutcome is the result of applying a ceiling to a desired tier.
type permissionOutcome struct {
	// Effective is the tier the plan may actually carry.
	Effective string
	// BlockedCode is non-empty when the request must be refused.
	BlockedCode string
	// WarningCode is non-empty when compat continues with a visible warning.
	WarningCode string
}

// applyPermissionCeiling resolves desired against the ceiling.
//
// explicitRequest marks the tier as caller-declared (init metadata). Only an
// explicit request can block; a config default is clamped with a warning.
func applyPermissionCeiling(desired string, ceiling AuthorityCeiling, explicitRequest bool) permissionOutcome {
	if ceiling.IsZero() || desired == "" {
		out := permissionOutcome{Effective: desired}
		if desired == "" && !ceiling.IsZero() {
			// The ceiling exists but nothing pinned a tier, so the Worker
			// default decides and the ceiling is not enforced here.
			out.WarningCode = WarnPermissionCeilingUnenforced
		}
		return out
	}

	limit := ceiling.Effective()
	desiredRank, limitRank := PermissionRank(desired), PermissionRank(limit)
	if limitRank < 0 {
		// An unparseable ceiling grants nothing; fall back to the strictest
		// tier rather than treating an unreadable ceiling as "no limit".
		limit, limitRank = worker.PermissionModeReadOnly, 0
	}
	if desiredRank >= 0 && desiredRank <= limitRank {
		return permissionOutcome{Effective: desired}
	}
	if explicitRequest {
		return permissionOutcome{BlockedCode: BlockPermissionExceedsCeiling}
	}
	return permissionOutcome{
		Effective:   limit,
		WarningCode: WarnPermissionClampedToCeiling,
	}
}

// missingRequiredCapabilities returns the required capabilities the caller
// could not verify, preserving the declared order. The result is nil when
// every requirement is verified.
func missingRequiredCapabilities(required, verified []string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(verified))
	for _, id := range verified {
		have[id] = struct{}{}
	}
	var missing []string
	for _, id := range required {
		if id == "" {
			continue
		}
		if _, ok := have[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	return missing
}

// authorityWarningMessage returns a bounded, non-secret explanation for an
// authority warning code. The two codes mean opposite things — one says the
// ceiling tightened the result, the other says the ceiling was never applied
// at all — so they must not share a message.
func authorityWarningMessage(code string) string {
	if code == WarnPermissionCeilingUnenforced {
		return "a permission ceiling applies but no tier was resolved; " +
			"the worker default decides and this plan does not enforce the ceiling"
	}
	return "config-derived permission tier was above the ceiling and was clamped down"
}
