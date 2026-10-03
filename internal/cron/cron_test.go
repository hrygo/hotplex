package cron

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	return &Scheduler{
		log:            slog.Default(),
		store:          store,
		executor:       NewExecutor(slog.Default(), bridge, sm, "", nil, &mockSystemDispatcher{sm: sm}),
		maxConcurrent:  3,
		maxJobs:        50,
		defaultTimeout: 5 * time.Minute,
		jobs:           map[string]*CronJob{},
		ctx:            context.Background(),
		tickLoop:       newTimerLoop(&Scheduler{}),
	}
}

// testRecurringJob returns a CronJob with valid lifecycle fields for recurring schedules.
func testRecurringJob(name, message string) *CronJob {
	return &CronJob{
		Name:      name,
		Schedule:  CronSchedule{Kind: ScheduleEvery, EveryMs: 60_000},
		Payload:   CronPayload{Kind: PayloadIsolatedSession, Message: message},
		OwnerID:   "user1",
		BotID:     "bot1",
		MaxRuns:   100,
		ExpiresAt: "2099-01-01T00:00:00Z",
	}
}

func TestScheduler_CreateJob(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	job := testRecurringJob("test-create", "hello world")

	require.NoError(t, s.CreateJob(ctx, job))
	require.NotEmpty(t, job.ID)
	require.True(t, job.Enabled)
	require.NotZero(t, job.CreatedAtMs)
	require.NotZero(t, job.State.NextRunAtMs)

	// Should be in the in-memory index.
	got, err := s.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "test-create", got.Name)
}

func TestScheduler_CreateJob_MaxJobsLimit(t *testing.T) {
	s := newTestScheduler(t)
	s.maxJobs = 2
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		require.NoError(t, s.CreateJob(ctx, testRecurringJob("job"+string(rune('a'+i)), "test")))
	}

	err := s.CreateJob(ctx, testRecurringJob("overflow", "test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "max jobs limit")
}

func TestScheduler_CreateJob_Validation(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	tests := []struct {
		name string
		job  *CronJob
	}{
		{"missing name", &CronJob{Schedule: CronSchedule{Kind: ScheduleEvery, EveryMs: 60_000}, Payload: CronPayload{Message: "hi"}, MaxRuns: 10, ExpiresAt: "2099-01-01T00:00:00Z"}},
		{"empty prompt", &CronJob{Name: "x", Schedule: CronSchedule{Kind: ScheduleEvery, EveryMs: 60_000}, Payload: CronPayload{Message: ""}, MaxRuns: 10, ExpiresAt: "2099-01-01T00:00:00Z"}},
		{"invalid schedule", &CronJob{Name: "x", Schedule: CronSchedule{Kind: "bad"}, Payload: CronPayload{Message: "hi"}, MaxRuns: 10, ExpiresAt: "2099-01-01T00:00:00Z"}},
		{"missing lifecycle", &CronJob{Name: "x", Schedule: CronSchedule{Kind: ScheduleEvery, EveryMs: 60_000}, Payload: CronPayload{Message: "hi"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, s.CreateJob(ctx, tt.job))
		})
	}
}

func TestScheduler_UpdateJob(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	job := testRecurringJob("test-update", "original")
	require.NoError(t, s.CreateJob(ctx, job))

	job.Payload.Message = "updated"
	require.NoError(t, s.UpdateJob(ctx, job))

	got, err := s.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "updated", got.Payload.Message)
}

func TestScheduler_DeleteJob(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	job := testRecurringJob("test-delete", "bye")
	require.NoError(t, s.CreateJob(ctx, job))
	require.NoError(t, s.DeleteJob(ctx, job.ID))

	_, err := s.GetJob(ctx, job.ID)
	require.Error(t, err)
}

func TestScheduler_ListJobs(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateJob(ctx, testRecurringJob("list-job", "test")))
	}

	jobs, err := s.ListJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 3)
}

func TestScheduler_GetJob_NotFound(t *testing.T) {
	s := newTestScheduler(t)
	_, err := s.GetJob(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestScheduler_TriggerJob(t *testing.T) {
	s := newTestScheduler(t)

	job := testRecurringJob("trigger-test", "manual")

	// TriggerJob should not block (starts goroutine).
	require.NoError(t, s.TriggerJob(context.Background(), job, TriggerRequest{}))
	// Give goroutine a moment to start.
	time.Sleep(10 * time.Millisecond)
}

func TestScheduler_CollectDue(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	now := time.Now()

	// Due job (next_run in the past).
	dueJob := testRecurringJob("due", "due")
	require.NoError(t, s.CreateJob(ctx, dueJob))
	// CreateJob overwrites next_run — fix it to be in the past.
	s.mu.Lock()
	s.jobs[dueJob.ID].State.NextRunAtMs = now.Add(-1 * time.Minute).UnixMilli()
	s.mu.Unlock()

	// Future job.
	futureJob := testRecurringJob("future", "future")
	require.NoError(t, s.CreateJob(ctx, futureJob))
	s.mu.Lock()
	s.jobs[futureJob.ID].State.NextRunAtMs = now.Add(1 * time.Hour).UnixMilli()
	s.mu.Unlock()

	// Disabled job (past due but disabled).
	disabledJob := testRecurringJob("disabled", "disabled")
	require.NoError(t, s.CreateJob(ctx, disabledJob))
	s.mu.Lock()
	s.jobs[disabledJob.ID].Enabled = false
	s.jobs[disabledJob.ID].State.NextRunAtMs = now.Add(-1 * time.Minute).UnixMilli()
	s.mu.Unlock()

	// Running job (past due but currently executing).
	runningJob := testRecurringJob("running", "running")
	require.NoError(t, s.CreateJob(ctx, runningJob))
	s.mu.Lock()
	s.jobs[runningJob.ID].State.NextRunAtMs = now.Add(-1 * time.Minute).UnixMilli()
	s.jobs[runningJob.ID].State.RunningAtMs = now.UnixMilli()
	s.mu.Unlock()

	due := s.collectDue(now)
	require.Len(t, due, 1)
	require.Equal(t, "due", due[0].Name)
}

func TestScheduler_NextTickDuration(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	now := time.Now()

	// No jobs → max interval.
	d := s.nextTickDuration(now)
	require.Equal(t, maxTimerInterval, d)

	// Add a job due in 30s.
	job := testRecurringJob("near", "near")
	job.Schedule.EveryMs = 120_000
	require.NoError(t, s.CreateJob(ctx, job))

	// Override the next_run to 30s from now.
	s.mu.Lock()
	s.jobs[job.ID].State.NextRunAtMs = now.Add(30 * time.Second).UnixMilli()
	s.mu.Unlock()

	d = s.nextTickDuration(now)
	require.True(t, d > 20*time.Second && d <= 30*time.Second)
}

func TestScheduler_UpdateConfig(t *testing.T) {
	s := newTestScheduler(t)
	require.Equal(t, 3, s.maxConcurrent)
	require.Equal(t, 50, s.maxJobs)

	s.UpdateConfig(10, 100)
	require.Equal(t, 10, s.maxConcurrent)
	require.Equal(t, 100, s.maxJobs)

	// Zero values should not change.
	s.UpdateConfig(0, 0)
	require.Equal(t, 10, s.maxConcurrent)
	require.Equal(t, 100, s.maxJobs)
}

func TestScheduler_WithinGracePeriod(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	tests := []struct {
		name      string
		everyMs   int64
		missedAgo time.Duration
		within    bool
	}{
		{"just missed 1m job", 60_000, 10 * time.Second, true},
		{"missed 1m job by 31s", 60_000, 31 * time.Second, false},   // grace = 30s
		{"missed 1h job by 20m", 3_600_000, 20 * time.Minute, true}, // grace = 30min
		{"missed 1h job by 40m", 3_600_000, 40 * time.Minute, false},
		{"missed 5h job by 15m", 18_000_000, 15 * time.Minute, true},  // grace capped at 30min, within
		{"missed 5h job by 45m", 18_000_000, 45 * time.Minute, false}, // grace capped at 30min, outside
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &CronJob{
				Schedule: CronSchedule{Kind: ScheduleEvery, EveryMs: tt.everyMs},
				State: CronJobState{
					NextRunAtMs: now.Add(-tt.missedAgo).UnixMilli(),
				},
			}
			got := s.withinGracePeriod(job, now)
			require.Equal(t, tt.within, got)
		})
	}
}

func TestScheduler_JobTimeout(t *testing.T) {
	s := newTestScheduler(t)
	require.Equal(t, 5*time.Minute, s.defaultTimeout)

	// Job with explicit timeout.
	job := &CronJob{TimeoutSec: 120}
	require.Equal(t, 2*time.Minute, s.jobTimeout(job))

	// Job without explicit timeout → default.
	job2 := &CronJob{TimeoutSec: 0}
	require.Equal(t, 5*time.Minute, s.jobTimeout(job2))
}

func TestScheduler_StartShutdown(t *testing.T) {
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}

	// Pre-seed a job in the DB.
	job := helperJob("lifecycle-test")
	require.NoError(t, store.Create(context.Background(), job))

	s := New(Deps{
		Log:        slog.Default(),
		Store:      store,
		Bridge:     bridge,
		SessionMgr: sm,
		Cfg:        Config{Enabled: true, MaxConcurrentRuns: 3, MaxJobs: 50},
	})

	ctx := context.Background()
	require.NoError(t, s.Start(ctx))

	// Verify the job was loaded from DB.
	got, err := s.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "lifecycle-test", got.Name)
	require.True(t, got.Enabled)

	// Shutdown should complete without timeout.
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.Shutdown(shutdownCtx)
}

func TestScheduler_ScheduleRetry(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	job := helperJob("retry-test")
	job.State.ConsecutiveErrs = 2
	require.NoError(t, s.store.Create(ctx, job))
	s.jobs[job.ID] = job

	before := time.Now()
	s.scheduleRetry(ctx, job)

	require.Equal(t, 1, job.State.RetryCount)
	require.True(t, job.State.NextRunAtMs > before.UnixMilli())

	// Duration should be backoff(2) = 1 minute (index 1 after off-by-one fix).
	require.WithinDuration(t, before.Add(1*time.Minute), time.UnixMilli(job.State.NextRunAtMs), 2*time.Second)
}

func TestScheduler_MergeJobState(t *testing.T) {
	s := newTestScheduler(t)

	job := testRecurringJob("merge-test", "hello")
	require.NoError(t, s.CreateJob(context.Background(), job))

	t.Run("merges state without overwriting Enabled", func(t *testing.T) {
		// Simulate external disable via CLI.
		s.mu.Lock()
		s.jobs[job.ID].Enabled = false
		s.mu.Unlock()

		// mergeJobState should update State but keep Enabled=false.
		newState := CronJobState{
			NextRunAtMs: time.Now().Add(1 * time.Hour).UnixMilli(),
			RunCount:    42,
		}
		s.mergeJobState(job.ID, newState, false)

		s.mu.Lock()
		got := s.jobs[job.ID]
		s.mu.Unlock()
		require.False(t, got.Enabled, "Enabled should remain false")
		require.Equal(t, 42, got.State.RunCount, "State should be merged")
	})

	t.Run("disable flag overrides Enabled", func(t *testing.T) {
		job2 := testRecurringJob("merge-disable", "world")
		require.NoError(t, s.CreateJob(context.Background(), job2))

		s.mu.Lock()
		require.True(t, s.jobs[job2.ID].Enabled)
		s.mu.Unlock()

		s.mergeJobState(job2.ID, CronJobState{RunCount: 5}, true)

		s.mu.Lock()
		got := s.jobs[job2.ID]
		s.mu.Unlock()
		require.False(t, got.Enabled, "disable=true should set Enabled=false")
		require.Equal(t, 5, got.State.RunCount)
	})

	t.Run("no-op for deleted job", func(t *testing.T) {
		// Should not panic.
		s.mergeJobState("nonexistent", CronJobState{RunCount: 99}, true)
	})
}
