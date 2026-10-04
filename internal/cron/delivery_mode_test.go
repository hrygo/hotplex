package cron

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func slackJob(mode DeliveryMode) *CronJob {
	job := testJob()
	job.ID = "cron_slack"
	job.Platform = "slack"
	job.PlatformKey = map[string]string{"channel_id": "C123"}
	job.DeliveryMode = mode
	return job
}

func TestResolveDeliveryMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   DeliveryMode
		want DeliveryMode
	}{
		{name: "absent stays legacy", in: "", want: DeliveryModeLegacyCLI},
		{name: "explicit legacy", in: DeliveryModeLegacyCLI, want: DeliveryModeLegacyCLI},
		{name: "gateway", in: DeliveryModeGateway, want: DeliveryModeGateway},
		{
			name: "unrecognised never silently gains an owner",
			in:   DeliveryMode("something_else"),
			want: DeliveryModeLegacyCLI,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, ResolveDeliveryMode(tc.in))
		})
	}
}

// TestExecutorGatewayMode_SuppressesTheCLIDeliveryInstruction is the
// double-delivery guard: in gateway mode the Agent must not also be told to
// send the result itself.
func TestExecutorGatewayMode_SuppressesTheCLIDeliveryInstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mode         DeliveryMode
		wantInstruct bool
	}{
		{name: "legacy keeps the CLI instruction", mode: DeliveryModeLegacyCLI, wantInstruct: true},
		{name: "gateway drops it", mode: DeliveryModeGateway, wantInstruct: false},
		{name: "absent mode keeps today's behaviour", mode: "", wantInstruct: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newOccurrenceStore(t)
			bridge := &mockBridge{}
			sm := &mockSessionStateChecker{
				defaultSession: &session.SessionInfo{State: "terminated"},
				defaultWorker:  &mockWorker{},
			}
			dispatcher := &mockSystemDispatcher{sm: sm}
			e := NewExecutor(discardLogger(), bridge, sm, "", store, dispatcher)

			job := slackJob(tc.mode)
			_, err := e.Execute(context.Background(), job,
				testScheduledTrigger(job.ID), 5*time.Second)
			require.NoError(t, err)

			hasInstruction := strings.Contains(dispatcher.lastReq.Content, "hotplex slack send-message")
			require.Equal(t, tc.wantInstruct, hasInstruction,
				"exactly one owner may be told to send the result")
		})
	}
}

// TestExecutorRecordsTheOwnerOnTheOccurrence pins that the owner is a fact
// about the firing, not about the job's current setting.
func TestExecutorRecordsTheOwnerOnTheOccurrence(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := NewExecutor(discardLogger(), bridge, sm, "", store, &mockSystemDispatcher{sm: sm})

	job := slackJob(DeliveryModeGateway)
	result, err := e.Execute(context.Background(), job,
		testScheduledTrigger(job.ID), 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, DeliveryModeGateway, result.DeliveryMode)

	stored, err := store.GetByID(context.Background(), result.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, DeliveryModeGateway, stored.DeliveryMode)

	// Switching the job's mode afterwards must not rewrite this run's owner:
	// the next firing of the same trigger still resolves to the recorded fact.
	job.DeliveryMode = DeliveryModeLegacyCLI
	again, err := e.Execute(context.Background(), job,
		testScheduledTrigger(job.ID), 5*time.Second)
	require.NoError(t, err)
	require.True(t, again.Duplicate)
	require.Equal(t, DeliveryModeGateway, again.DeliveryMode)
}

func TestOccurrenceClaimPersistsDeliveryMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newOccurrenceStore(t)

	occ, err := NewOccurrence(TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}, DeliveryModeGateway, time.UnixMilli(1_700_000_000_000))
	require.NoError(t, err)

	created, wasCreated, err := store.Claim(ctx, occ)
	require.NoError(t, err)
	require.True(t, wasCreated)
	require.Equal(t, DeliveryModeGateway, created.DeliveryMode)

	readBack, err := store.GetByID(ctx, occ.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, DeliveryModeGateway, readBack.DeliveryMode)
}

// recordingEffectDelivery captures what the gateway-owned path was asked to
// deliver.
type recordingEffectDelivery struct {
	requests []EffectDeliveryRequest
	err      error
}

func (r *recordingEffectDelivery) DeliverEffect(
	_ context.Context, req EffectDeliveryRequest,
) error {
	r.requests = append(r.requests, req)
	return r.err
}

func newRoutingScheduler(effectDelivery EffectDelivery, legacyCalls *int) *Scheduler {
	return &Scheduler{
		log:            discardLogger(),
		ctx:            context.Background(),
		effectDelivery: effectDelivery,
		delivery: &Delivery{
			log:     discardLogger(),
			extract: func(context.Context, string) (string, error) { return "answer", nil },
			deliverFn: func(context.Context, string, map[string]string, string) error {
				*legacyCalls++
				return nil
			},
		},
	}
}

func TestSchedulerRoutesGatewayModeToTheEffectOwner(t *testing.T) {
	t.Parallel()

	effectDelivery := &recordingEffectDelivery{}
	legacyCalls := 0
	s := newRoutingScheduler(effectDelivery, &legacyCalls)

	job := slackJob(DeliveryModeGateway)
	s.routeDelivery(job, ExecuteResult{
		SessionID:    "sess-1",
		OccurrenceID: "occ-1",
		ExecutionID:  "exec-1",
		DeliveryMode: DeliveryModeGateway,
	})

	require.Len(t, effectDelivery.requests, 1)
	require.Equal(t, "occ-1", effectDelivery.requests[0].OccurrenceID)
	require.Equal(t, "exec-1", effectDelivery.requests[0].ExecutionID)
	require.Equal(t, "C123", effectDelivery.requests[0].PlatformKey["channel_id"])
	require.Zero(t, legacyCalls, "the legacy path must not also deliver a gateway-mode run")
}

func TestSchedulerKeepsLegacyModeOnItsOriginalPath(t *testing.T) {
	t.Parallel()

	effectDelivery := &recordingEffectDelivery{}
	legacyCalls := 0
	s := newRoutingScheduler(effectDelivery, &legacyCalls)

	job := slackJob(DeliveryModeLegacyCLI)
	// No CLI delivery target configured, so the gateway delivery path handles
	// it — exactly as it does today.
	job.PlatformKey = map[string]string{}
	s.routeDelivery(job, ExecuteResult{
		SessionID:    "sess-1",
		OccurrenceID: "occ-1",
		ExecutionID:  "exec-1",
		DeliveryMode: DeliveryModeLegacyCLI,
	})

	require.Equal(t, 1, legacyCalls)
	require.Empty(t, effectDelivery.requests,
		"a legacy run must never be handed to the gateway owner")
}

// TestSchedulerGatewayModeWithoutOwnerRefusesLoudly: a gateway-mode job whose
// owner is missing must not quietly fall back to a path that cannot record an
// intent.
func TestSchedulerGatewayModeWithoutOwnerRefusesLoudly(t *testing.T) {
	t.Parallel()

	legacyCalls := 0
	s := newRoutingScheduler(nil, &legacyCalls)

	s.routeDelivery(slackJob(DeliveryModeGateway), ExecuteResult{
		SessionID:    "sess-1",
		OccurrenceID: "occ-1",
		ExecutionID:  "exec-1",
		DeliveryMode: DeliveryModeGateway,
	})

	require.Zero(t, legacyCalls, "no silent downgrade to the legacy path")
}

func TestSchedulerSilentJobIsNeverDelivered(t *testing.T) {
	t.Parallel()

	effectDelivery := &recordingEffectDelivery{}
	legacyCalls := 0
	s := newRoutingScheduler(effectDelivery, &legacyCalls)

	job := slackJob(DeliveryModeGateway)
	job.Silent = true
	s.routeDelivery(job, ExecuteResult{
		SessionID:    "sess-1",
		OccurrenceID: "occ-1",
		ExecutionID:  "exec-1",
		DeliveryMode: DeliveryModeGateway,
	})

	require.Empty(t, effectDelivery.requests)
	require.Zero(t, legacyCalls)
}
