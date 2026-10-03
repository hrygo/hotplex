package worker

// Isolation capability reporting (#946 plan unit E2).
//
// The rule this file exists to enforce: an isolation claim must come from
// evidence the backend actually produced, never from what we asked for.
//
// It is tempting to report "the plan said read-only, so filesystem isolation
// is enforced". That is two different claims. The first is what we requested;
// the second is what the machine did. Only the second is worth anything to an
// operator deciding whether a session is safe, so the states below keep them
// apart, and the default is UNKNOWN rather than a convenient yes.
//
// Reporting a narrower ENVIRONMENT is not on this scale either — see
// base.BuildEnv. Environment narrowing is hygiene; isolation is a boundary.

import "context"

// IsolationDimension names what is being reported on.
type IsolationDimension string

const (
	// IsolationFilesystem covers filesystem reach.
	IsolationFilesystem IsolationDimension = "filesystem"
	// IsolationNetwork covers outbound network reach.
	IsolationNetwork IsolationDimension = "network"
)

// IsolationState is the evidence-bounded status of one dimension.
type IsolationState string

const (
	// IsolationDeclared: we requested a boundary and passed it to the
	// backend. Nothing confirms the backend honoured it.
	IsolationDeclared IsolationState = "declared"
	// IsolationObserved: the backend reported a boundary back, but the report
	// is the backend describing itself rather than an independent check.
	IsolationObserved IsolationState = "observed"
	// IsolationEnforced: verified evidence that the boundary is in force.
	IsolationEnforced IsolationState = "enforced"
	// IsolationPartial: some of the dimension is bounded; the rest is not.
	IsolationPartial IsolationState = "partial"
	// IsolationUnavailable: the backend cannot provide this dimension at all.
	IsolationUnavailable IsolationState = "unavailable"
	// IsolationUnknown: nothing is known. This is the honest default and the
	// only safe one for a Worker with no evidence to offer.
	IsolationUnknown IsolationState = "unknown"
)

// IsolationReport is the bounded, secret-free projection of one Worker's
// isolation capabilities. Evidence is a short enum-like phrase naming WHAT
// proved the claim — never a path, command or host detail.
type IsolationReport struct {
	Filesystem IsolationState `json:"filesystem"`
	Network    IsolationState `json:"network"`
	// Scope is "process" for Workers running one shared process across
	// sessions. A per-session reading would be fiction there: every session
	// on the process sees the same boundary.
	Scope string `json:"scope,omitempty"`
	// Evidence names what backs the states above.
	Evidence string `json:"evidence,omitempty"`
}

// Isolation scopes.
const (
	IsolationScopeSession = "session"
	IsolationScopeProcess = "process"
)

// UnknownIsolationReport is what a Worker reports when it has no evidence to
// offer. It is a real answer, not a placeholder: "we do not know" is the
// correct thing to say about a boundary nobody has verified.
func UnknownIsolationReport() IsolationReport {
	return IsolationReport{
		Filesystem: IsolationUnknown,
		Network:    IsolationUnknown,
		Scope:      IsolationScopeSession,
		Evidence:   "no_backend_evidence",
	}
}

// State returns the report's state for one dimension.
func (r IsolationReport) State(d IsolationDimension) IsolationState {
	switch d {
	case IsolationFilesystem:
		return r.Filesystem
	case IsolationNetwork:
		return r.Network
	}
	// An unrecognised dimension is unknown, never assumed safe.
	return IsolationUnknown
}

// IsolationReporter is an OPTIONAL Worker capability, discovered by type
// assertion. It is deliberately not part of the Worker interface: forcing
// every adapter to answer a question it cannot answer would push invented
// "enforced" claims into the type system, which is worse than an honest
// UNKNOWN from the default.
type IsolationReporter interface {
	ReportIsolation(ctx context.Context, info SessionInfo) IsolationReport
}

// ReportIsolation returns w's isolation report, falling back to the honest
// unknown report when the Worker does not implement the optional interface.
func ReportIsolation(ctx context.Context, w Worker, info SessionInfo) IsolationReport {
	if r, ok := w.(IsolationReporter); ok {
		if rep := r.ReportIsolation(ctx, info); rep.Filesystem != "" || rep.Network != "" {
			return rep
		}
		// A reporter that returns an empty report has, in effect, declined to
		// answer. Treat that as unknown rather than reading empty as "fine".
	}
	return UnknownIsolationReport()
}
