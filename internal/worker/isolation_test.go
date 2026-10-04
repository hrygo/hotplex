package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// noIsolationWorker implements Worker but NOT IsolationReporter.
type noIsolationWorker struct{ Worker }

// emptyReporter implements the optional interface but declines to answer.
type emptyReporter struct {
	noIsolationWorker
	called bool
}

func (e *emptyReporter) ReportIsolation(context.Context, SessionInfo) IsolationReport {
	e.called = true
	return IsolationReport{}
}

// chattyReporter answers with real evidence.
type chattyReporter struct {
	noIsolationWorker
	report IsolationReport
}

func (c *chattyReporter) ReportIsolation(context.Context, SessionInfo) IsolationReport {
	return c.report
}

// TestReportIsolation_DefaultsToUnknown is the whole point of E2: a Worker
// that cannot prove anything says so, rather than the gateway inferring
// safety from what it requested.
func TestReportIsolation_DefaultsToUnknown(t *testing.T) {
	t.Parallel()
	got := ReportIsolation(context.Background(), &noIsolationWorker{}, SessionInfo{})
	require.Equal(t, IsolationUnknown, got.Filesystem)
	require.Equal(t, IsolationUnknown, got.Network)
	require.Equal(t, "no_backend_evidence", got.Evidence)
}

func TestReportIsolation_EmptyReportIsTreatedAsUnknown(t *testing.T) {
	t.Parallel()
	// An empty report is a declined question, not a clean bill of health.
	r := &emptyReporter{}
	got := ReportIsolation(context.Background(), r, SessionInfo{})
	require.True(t, r.called, "the optional interface must actually be consulted")
	require.Equal(t, IsolationUnknown, got.Filesystem)
	require.Equal(t, IsolationUnknown, got.Network)
}

func TestReportIsolation_UsesTheWorkerEvidence(t *testing.T) {
	t.Parallel()
	want := IsolationReport{
		Filesystem: IsolationEnforced,
		Network:    IsolationUnavailable,
		Scope:      IsolationScopeProcess,
		Evidence:   "backend_probe",
	}
	got := ReportIsolation(context.Background(), &chattyReporter{report: want}, SessionInfo{})
	require.Equal(t, want, got)
}

func TestIsolationStateLookup(t *testing.T) {
	t.Parallel()
	r := IsolationReport{Filesystem: IsolationPartial, Network: IsolationEnforced}
	require.Equal(t, IsolationPartial, r.State(IsolationFilesystem))
	require.Equal(t, IsolationEnforced, r.State(IsolationNetwork))
	require.Equal(t, IsolationUnknown, r.State(IsolationDimension("something-else")),
		"an unrecognised dimension is unknown, never assumed safe")
}

func TestUnknownIsolationReport_IsNotASafetyClaim(t *testing.T) {
	t.Parallel()
	r := UnknownIsolationReport()
	require.NotEqual(t, IsolationEnforced, r.Filesystem)
	require.NotEqual(t, IsolationEnforced, r.Network)
}
