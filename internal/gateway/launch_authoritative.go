package gateway

// Authoritative launch (#946 plan unit D3).
//
// D2 made the plan the single description of a launch while leaving the legacy
// parameters in charge. D3 flips that for the entry×Worker combinations named
// in the rollout allowlists — and only those.
//
// Two properties matter more than the feature itself:
//
//  1. A blocked plan stops the launch. There is no "log it and fall back to the
//     legacy path" branch: swallowing the error and launching anyway would make
//     the plan decorative, which is exactly the failure mode this whole plan
//     exists to remove.
//
//  2. Shared-process Workers cannot be given per-session isolation by
//     pretending. opencode-server and codex CLI run one process for many
//     sessions, so an env or isolation profile is PROCESS-scoped. The first
//     slice refuses an incompatible profile rather than silently inheriting the
//     running process's, and never restarts a shared process to make room.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/worker"
)

// ErrPlanLaunchRefused is returned when an authoritative plan cannot be
// launched as specified. It is distinct from a Worker start failure: nothing
// was attempted.
var ErrPlanLaunchRefused = fmt.Errorf("gateway: launch refused by runtime plan")

// sharedProcessWorkers are the Worker types that run ONE process for many
// sessions. Anything process-scoped in a plan (env profile, isolation mode) is
// therefore not per-session for these.
var sharedProcessWorkers = map[worker.WorkerType]struct{}{
	worker.TypeOpenCodeSrv: {},
	worker.TypeCodexCLI:    {},
}

// sharedRuntimeGuard remembers the process-scoped profile each shared Worker
// was launched with, so a later session that needs a different one is refused
// instead of silently inheriting the running process.
//
// First slice: the profile is recorded for the lifetime of the gateway process
// and never released. Refusing more than strictly necessary is the fail-closed
// direction; releasing on last-release would need hooks into the Worker
// process manager, which is E's territory.
type sharedRuntimeGuard struct {
	mu       sync.Mutex
	profiles map[worker.WorkerType]string
}

func newSharedRuntimeGuard() *sharedRuntimeGuard {
	return &sharedRuntimeGuard{profiles: make(map[worker.WorkerType]string)}
}

// processScopedProfile returns the digest of everything about a plan that a
// SHARED process cannot vary per session. It deliberately excludes per-session
// decisions (tools, permission tier, worker session identity) so the guard
// refuses only for genuine process-level conflicts.
func processScopedProfile(p launchPlan) string {
	return strings.Join([]string{
		p.Plan.ConfigHash,
		p.Plan.EnvProfile,
		p.Plan.AgentSpec.Sandbox.Mode,
	}, "|")
}

// checkAndRecord validates a shared-process launch against the profile already
// in use, recording it on first use.
func (g *sharedRuntimeGuard) checkAndRecord(wt worker.WorkerType, profile string) error {
	if _, shared := sharedProcessWorkers[wt]; !shared {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	existing, seen := g.profiles[wt]
	if !seen {
		g.profiles[wt] = profile
		return nil
	}
	if existing == profile {
		return nil
	}
	return fmt.Errorf("%w: %s worker already runs with an incompatible process profile",
		ErrPlanLaunchRefused, wt)
}

// applyAuthoritativePlan projects the plan onto the parameters that will be
// launched, so the Worker is actually started from the plan rather than from
// parameters that merely happen to agree with it.
//
// Only AgentSpec-owned fields are overlaid; everything else on SessionInfo is
// passed through untouched (the mapper's ownership table).
func applyAuthoritativePlan(spec agentspec.AgentSpec, info worker.SessionInfo) worker.SessionInfo {
	return agentspec.MapToSessionInfo(spec, info)
}
