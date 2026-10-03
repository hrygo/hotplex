package cron

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

// failingOccurrenceStore wraps a real store and fails Claim, to prove the
// executor refuses to start an Agent without a durable identity.
type failingOccurrenceStore struct {
	inner OccurrenceStore
	err   error
}

func (f *failingOccurrenceStore) Claim(ctx context.Context, occ *Occurrence) (*Occurrence, bool, error) {
	return nil, false, f.err
}

func (f *failingOccurrenceStore) Get(ctx context.Context, key string, gen int64) (*Occurrence, error) {
	return f.inner.Get(ctx, key, gen)
}

func (f *failingOccurrenceStore) GetByID(ctx context.Context, id string) (*Occurrence, error) {
	return f.inner.GetByID(ctx, id)
}

func (f *failingOccurrenceStore) BindSession(ctx context.Context, id, sessionID string) error {
	return f.inner.BindSession(ctx, id, sessionID)
}

func (f *failingOccurrenceStore) UpdateStatus(
	ctx context.Context, id string, status OccurrenceStatus, code string, at time.Time,
) error {
	return f.inner.UpdateStatus(ctx, id, status, code, at)
}

func (f *failingOccurrenceStore) ListByJob(ctx context.Context, jobID string, limit int) ([]*Occurrence, error) {
	return f.inner.ListByJob(ctx, jobID, limit)
}

func newOccurrenceExecutor(
	t *testing.T, store OccurrenceStore, bridge *mockBridge, sm SessionStateChecker,
) *Executor {
	t.Helper()
	return NewExecutor(slog.Default(), bridge, sm, "", store)
}

func testScheduledTrigger(jobID string) TriggerIdentity {
	return TriggerIdentity{
		Kind:          TriggerScheduled,
		JobID:         jobID,
		ScheduleRev:   "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}
}

func TestExecutorExecute_DuplicateTriggerRunsAgentOnce(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	trigger := testScheduledTrigger(job.ID)

	first, err := e.Execute(context.Background(), job, trigger, 5*time.Second)
	require.NoError(t, err)
	require.False(t, first.Duplicate)
	require.NotEmpty(t, first.SessionID)
	require.Equal(t, 1, bridge.startCount, "the first trigger starts one session")

	// A repeated trigger of the same firing must resolve to the recorded run
	// instead of starting a second Agent.
	second, err := e.Execute(context.Background(), job, trigger, 5*time.Second)
	require.NoError(t, err)
	require.True(t, second.Duplicate, "a repeated trigger must be suppressed")
	require.Equal(t, first.OccurrenceID, second.OccurrenceID)
	require.Equal(t, first.SessionID, second.SessionID)
	require.Equal(t, 1, bridge.startCount, "a duplicate trigger must not start a second session")
}

func TestExecutorExecute_ConcurrentDuplicateTriggersStartOneAgent(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	trigger := testScheduledTrigger(job.ID)

	const racers = 6
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		started  int
		execErrs []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.Execute(context.Background(), job, trigger, 5*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				execErrs = append(execErrs, err)
				return
			}
			if !res.Duplicate {
				started++
			}
		}()
	}
	wg.Wait()

	require.Empty(t, execErrs)
	require.Equal(t, 1, started,
		"exactly one concurrent trigger of the same firing may start the Agent")
	require.Equal(t, 1, bridge.startCount)
}

func TestExecutorExecute_WorkerStartFailureKeepsOccurrence(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{startErr: errors.New("bridge down")}
	sm := &mockSessionStateChecker{workers: map[string]worker.Worker{}}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	res, err := e.Execute(context.Background(), job, testScheduledTrigger(job.ID), 5*time.Second)
	require.Error(t, err)
	require.NotEmpty(t, res.OccurrenceID)

	// The trigger was taken and must not be lost, even though no Agent ran.
	occ, err := store.GetByID(context.Background(), res.OccurrenceID)
	require.NoError(t, err, "a failed start must still leave the occurrence on record")
	require.Equal(t, OccurrenceFailed, occ.Status)
	require.Equal(t, "SESSION_START_FAILED", occ.ErrorCode)

	// Because the trigger is durably claimed, a retry must be an explicit new
	// generation rather than a silent second attempt at the same one.
	_, created, err := store.Claim(context.Background(), &Occurrence{
		OccurrenceID: GenerateOccurrenceID(),
		TriggerKey:   occ.TriggerKey,
		Generation:   occ.Generation,
		JobID:        occ.JobID,
		TriggerKind:  occ.TriggerKind,
		Status:       OccurrenceAccepted,
	})
	require.NoError(t, err)
	require.False(t, created, "the same generation must not be reclaimable")
}

func TestExecutorExecute_TimeoutRecordsUnknownNotRetryableFailure(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "running"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	res, err := e.Execute(context.Background(), job, testScheduledTrigger(job.ID), 100*time.Millisecond)
	require.Error(t, err)

	occ, err := store.GetByID(context.Background(), res.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, OccurrenceUnknown, occ.Status,
		"a timeout is not proof the Agent had no effect, so it must not read as a safe failure")
	require.Equal(t, "EXECUTION_TIMEOUT", occ.ErrorCode)
}

func TestExecutorExecute_BindsSessionAndCompletesOccurrence(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	res, err := e.Execute(context.Background(), job, testScheduledTrigger(job.ID), 5*time.Second)
	require.NoError(t, err)

	occ, err := store.GetByID(context.Background(), res.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, OccurrenceCompleted, occ.Status)
	require.Equal(t, res.SessionID, occ.SessionID,
		"the occurrence must record the session so a duplicate can return it")
	require.NotNil(t, occ.StartedAtMs)
	require.NotNil(t, occ.FinishedAtMs)
}

func TestExecutorExecute_ClaimFailureRefusesToStartAgent(t *testing.T) {
	t.Parallel()

	store := &failingOccurrenceStore{inner: newOccurrenceStore(t), err: errors.New("db down")}
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	job := testJob()
	_, err := e.Execute(context.Background(), job, testScheduledTrigger(job.ID), 5*time.Second)
	require.Error(t, err)
	require.Equal(t, 0, bridge.startCount,
		"without a durable identity the executor must not start an Agent it cannot deduplicate")
}

func TestExecutorExecute_IncompleteTriggerIdentityRefusesToRun(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newOccurrenceExecutor(t, store, bridge, sm)

	// A scheduled firing with no instant has no stable key; it must fail loudly
	// rather than fall back to a timestamp that defeats deduplication.
	_, err := e.Execute(context.Background(), testJob(),
		TriggerIdentity{Kind: TriggerScheduled, JobID: "cron_test"}, 5*time.Second)
	require.ErrorIs(t, err, ErrTriggerIdentityIncomplete)
	require.Equal(t, 0, bridge.startCount)
}

func TestRequestedTriggerFor(t *testing.T) {
	t.Parallel()

	webhookJob := testJob()
	webhookJob.PlatformKey = map[string]string{"trigger": "webhook", "pr_number": "42"}

	t.Run("webhook with a verified event id deduplicates on it", func(t *testing.T) {
		t.Parallel()
		identity := RequestedTriggerFor(webhookJob, "nonce-1", "evt-123")
		require.Equal(t, TriggerWebhook, identity.Kind)
		key, err := identity.Key()
		require.NoError(t, err)
		require.Equal(t, "webhook|cron_test|evt-123", key)
	})

	t.Run("webhook without a verified event id does not claim dedup", func(t *testing.T) {
		t.Parallel()
		identity := RequestedTriggerFor(webhookJob, "nonce-1", "")
		require.Equal(t, TriggerManual, identity.Kind,
			"without a stable source ID the firing must be per-request, not deduplicated")
		key, err := identity.Key()
		require.NoError(t, err)
		require.Equal(t, "manual|cron_test|nonce-1", key)
	})

	t.Run("plain manual trigger uses its request nonce", func(t *testing.T) {
		t.Parallel()
		identity := RequestedTriggerFor(testJob(), "nonce-9", "")
		require.Equal(t, TriggerManual, identity.Kind)
		key, err := identity.Key()
		require.NoError(t, err)
		require.Equal(t, "manual|cron_test|nonce-9", key)
	})
}
