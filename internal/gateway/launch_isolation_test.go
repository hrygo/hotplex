package gateway

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// TestIsolationShortfall_BarIsEnforcedOnly pins the standard E2 refuses to
// launch a Worker that cannot prove an isolation dimension the configuration
// requires. Only "enforced" is strong enough; anything weaker — declared,
// observed, partial, unavailable, unknown — is a weaker guarantee than was
// asked for.
func TestIsolationShortfall_BarIsEnforcedOnly(t *testing.T) {
	t.Parallel()
	req := config.IsolationRequirementConfig{Filesystem: true, Network: true}

	for _, tc := range []struct {
		name        string
		report      worker.IsolationReport
		wantMissing []worker.IsolationDimension
	}{
		{
			name: "fully enforced satisfies the requirement",
			report: worker.IsolationReport{
				Filesystem: worker.IsolationEnforced,
				Network:    worker.IsolationEnforced,
			},
		},
		{
			name: "declared is not enforced",
			report: worker.IsolationReport{
				Filesystem: worker.IsolationDeclared,
				Network:    worker.IsolationEnforced,
			},
			wantMissing: []worker.IsolationDimension{worker.IsolationFilesystem},
		},
		{
			name: "partial is not enforced",
			report: worker.IsolationReport{
				Filesystem: worker.IsolationPartial,
				Network:    worker.IsolationPartial,
			},
			wantMissing: []worker.IsolationDimension{
				worker.IsolationFilesystem, worker.IsolationNetwork,
			},
		},
		{
			name: "unknown is not enforced",
			report: worker.IsolationReport{
				Filesystem: worker.IsolationUnknown,
				Network:    worker.IsolationUnknown,
			},
			wantMissing: []worker.IsolationDimension{
				worker.IsolationFilesystem, worker.IsolationNetwork,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isolationShortfall(req, tc.report)
			if tc.wantMissing == nil {
				require.Empty(t, got)
				return
			}
			require.ElementsMatch(t, tc.wantMissing, got)
			// The refusal reason must name dimensions and states only, never
			// the backend-controlled evidence string.
			reason := isolationRefusal(got, tc.report)
			require.NotContains(t, reason, "no_backend_evidence")
		})
	}
}

func TestIsolationShortfall_NoRequirementMeansNoRefusal(t *testing.T) {
	t.Parallel()
	unknown := worker.UnknownIsolationReport()
	require.Empty(t, isolationShortfall(config.IsolationRequirementConfig{}, unknown))
	require.Empty(t, isolationShortfall(
		config.IsolationRequirementConfig{Network: true},
		worker.IsolationReport{
			Network:    worker.IsolationEnforced,
			Filesystem: worker.IsolationUnknown,
		}),
		"requiring only the network must not also demand filesystem enforcement")
}

// TestRequiredIsolationRefusesLaunch proves the requirement is enforced at
// launch time, not merely reported after the fact.
func TestRequiredIsolationRefusesLaunch(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-isolation"
	w := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, Platform: "slack",
	}, nil).Once()
	sm.On("GetWorker", sessionID).Return(nil).Maybe()

	cfg := config.Default()
	cfg.Worker.RequireIsolation = config.IsolationRequirementConfig{Filesystem: true}
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: newTestHub(t), SM: sm})
	b.SetConfigProvider(func() *config.Config { return cfg })
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{w}})

	_, err := b.StartFreshWorker(context.Background(), sessionID)
	require.ErrorIs(t, err, ErrPlanLaunchRefused)
	require.ErrorContains(t, err, "filesystem=unknown")
	require.True(t, w.terminated.Load(), "a Worker refused on isolation must be torn down")
}

func TestNoIsolationRequirementLaunchesNormally(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-no-isolation-req"
	eventsCh := make(chan *events.Envelope)
	w := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: eventsCh},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, Platform: "slack",
	}, nil).Once()
	sm.On("AttachWorker", sessionID, w).Return(nil).Once()
	sm.On("GetWorker", sessionID).Return(w).Maybe()
	sm.On("DetachWorkerIf", sessionID, w).Return(true).Maybe()

	// claude_code has no IsolationReporter, so its report is UNKNOWN. With no
	// requirement configured that must NOT block an ordinary launch — strict
	// env alone is enough to run without an OS isolation backend.
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: newTestHub(t), SM: sm})
	b.SetConfigProvider(func() *config.Config { return config.Default() })
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{w}})

	_, err := b.StartFreshWorker(context.Background(), sessionID)
	require.NoError(t, err, "an unknown isolation report must not block an ordinary launch")

	_, ok := b.LaunchPlanFor(sessionID)
	require.True(t, ok)
}
