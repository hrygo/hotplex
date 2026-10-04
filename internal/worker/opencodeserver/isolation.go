package opencodeserver

import (
	"context"

	"github.com/hrygo/hotplex/internal/worker"
)

// ReportIsolation implements worker.IsolationReporter (#867).
//
// The OCS session carries a permission mode + tier handed to the server.
// Same honesty rule as the other adapters: a restrictive tier is a declared
// boundary, not verified enforcement; the network dimension is unknown.
func (w *Worker) ReportIsolation(_ context.Context, _ worker.SessionInfo) worker.IsolationReport {
	ceiling, ok := w.permissionCeiling.Mode()
	report := worker.IsolationReport{
		Network:  worker.IsolationUnknown,
		Scope:    worker.IsolationScopeSession,
		Evidence: "permission_tier_passed_to_server",
	}
	if !ok {
		report.Filesystem = worker.IsolationUnknown
		report.Evidence = "permission_ceiling_unset"
		return report
	}
	switch ceiling {
	case worker.PermissionModeReadOnly, worker.PermissionModeWorkspace:
		report.Filesystem = worker.IsolationDeclared
	default:
		report.Filesystem = worker.IsolationUnknown
		report.Evidence = "permission_tier_unrecognised_or_unbounded"
	}
	return report
}

var _ worker.IsolationReporter = (*Worker)(nil)
