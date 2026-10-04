package claudecode

import (
	"context"

	"github.com/hrygo/hotplex/internal/worker"
)

// ReportIsolation implements worker.IsolationReporter (#867).
//
// What Claude Code actually gives us: a permission mode that bounds which
// file operations the CLI will attempt. That is a declared boundary handed
// to the backend, not a verified enforcement — and it says nothing about
// the network. So read-only/workspace report declared, everything else
// reports unknown, and network always stays unknown.
func (w *Worker) ReportIsolation(_ context.Context, session worker.SessionInfo) worker.IsolationReport {
	mode := w.effectivePermissionMode(session)
	report := worker.IsolationReport{
		Network:  worker.IsolationUnknown,
		Scope:    worker.IsolationScopeSession,
		Evidence: "permission_mode_passed_to_cli",
	}
	switch mode {
	case worker.PermissionModeReadOnly, worker.PermissionModeWorkspace:
		report.Filesystem = worker.IsolationDeclared
	default:
		report.Filesystem = worker.IsolationUnknown
		report.Evidence = "permission_mode_unrecognised_or_unbounded"
	}
	return report
}

var _ worker.IsolationReporter = (*Worker)(nil)
