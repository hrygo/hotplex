package acp

import (
	"context"

	"github.com/hrygo/hotplex/internal/worker"
)

// ReportIsolation implements worker.IsolationReporter (#867).
//
// ACP agents run behind an approval gate: approve=false means file effects
// need a human decision, which is a declared boundary, not verified
// enforcement. approve=true removes even that gate. The network dimension
// stays unknown either way.
func (w *Worker) ReportIsolation(_ context.Context, _ worker.SessionInfo) worker.IsolationReport {
	report := worker.IsolationReport{
		Network:  worker.IsolationUnknown,
		Scope:    worker.IsolationScopeSession,
		Evidence: "auto_approve_gate",
	}
	if w.autoApprove.Load() {
		report.Filesystem = worker.IsolationUnknown
		report.Evidence = "auto_approve_removes_approval_gate"
	} else {
		report.Filesystem = worker.IsolationDeclared
	}
	return report
}

var _ worker.IsolationReporter = (*Worker)(nil)
