package codexcli

// Isolation reporting for the codex app-server Worker (#946 E2).
//
// The honest answer here is DECLARED, never ENFORCED. The gateway passes a
// sandbox mode to the codex CLI, and the CLI is the thing that would enforce
// it — but the gateway has no independent check that the boundary is in
// force. Reporting "enforced" would be the gateway describing its own request
// as if it were the machine's behaviour, which is exactly the claim this
// reporter exists to avoid.
//
// SCOPE is "process": one app-server serves every codex_cli session, so every
// session on it sees the same boundary. A per-session reading would be a
// statement about a session that has no independent environment at all.

import (
	"context"

	"github.com/hrygo/hotplex/internal/worker"
)

// ReportIsolation implements worker.IsolationReporter.
func (w *AppServerWorker) ReportIsolation(
	_ context.Context, info worker.SessionInfo,
) worker.IsolationReport {
	mode := info.Sandbox
	if mode == "" {
		mode = w.manager.cfg.Sandbox
	}

	// The codex sandbox vocabulary bounds the filesystem and says nothing
	// about the network, so the network dimension stays unknown rather than
	// inheriting an assumption from the filesystem answer.
	report := worker.IsolationReport{
		Network:  worker.IsolationUnknown,
		Scope:    worker.IsolationScopeProcess,
		Evidence: "sandbox_mode_passed_to_cli",
	}

	switch mode {
	case "read-only", "workspace-write":
		// We asked for a boundary and handed it to the CLI. That is the whole
		// of what we know.
		report.Filesystem = worker.IsolationDeclared
	case "danger-full-access":
		// The requested mode IS the absence of a filesystem boundary. Saying
		// "declared" here would dress up no restriction as a declared one.
		report.Filesystem = worker.IsolationUnavailable
		report.Evidence = "sandbox_mode_requests_no_boundary"
	default:
		// An unrecognised mode is not evidence of anything.
		report.Filesystem = worker.IsolationUnknown
		report.Evidence = "sandbox_mode_unrecognised"
	}
	return report
}

var _ worker.IsolationReporter = (*AppServerWorker)(nil)
