package cron

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

func TestErrorType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "execution"},
		{"timeout", errors.New("context timeout exceeded"), "timeout"},
		{"deadline", errors.New("deadline exceeded"), "timeout"},
		{"rate limit", errors.New("rate limit hit"), "rate_limit"},
		{"429", errors.New("HTTP 429"), "rate_limit"},
		{"500", errors.New("500 internal server error"), "server_error"},
		{"502", errors.New("502 bad gateway"), "server_error"},
		{"503", errors.New("503 unavailable"), "server_error"},
		{"504", errors.New("504 service unavailable"), "server_error"},
		{"connection refused", errors.New("dial tcp: connection refused"), "network"},
		{"temporary", errors.New("temporary failure"), "network"},
		{"execution", errors.New("worker not found"), "execution"},
		{"other", errors.New("something else"), "execution"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errorType(tt.err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTimerLoop_ArmStop(t *testing.T) {
	s := &Scheduler{log: slog.Default(), ctx: context.Background(), jobs: map[string]*CronJob{}}
	tl := newTimerLoop(s)

	// Arm and stop should not panic.
	tl.arm(1 * time.Second)
	tl.stop()

	// Re-arm after stop.
	tl.arm(2 * time.Second)
	tl.stop()
}

func TestTimerLoop_ArmCapsMaxInterval(t *testing.T) {
	s := &Scheduler{log: slog.Default(), ctx: context.Background(), jobs: map[string]*CronJob{}}
	tl := newTimerLoop(s)

	// Durations > maxTimerInterval should be capped.
	tl.arm(5 * time.Minute)
	tl.stop()
}

func TestTimerLoop_ArmZeroDuration(t *testing.T) {
	s := &Scheduler{log: slog.Default(), ctx: context.Background(), jobs: map[string]*CronJob{}}
	tl := newTimerLoop(s)

	// Zero/negative should be set to 1 second.
	tl.arm(0)
	tl.stop()

	tl.arm(-1 * time.Second)
	tl.stop()
}

func TestOnTick_NoDueJobs(t *testing.T) {
	store := newTestStore(t)
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	// No due jobs → should just re-arm without error.
	s.tickLoop.onTick()
}

func TestOnTick_AdvanceNextRunBeforeExecution(t *testing.T) {
	// Verifies at-most-once: next_run is advanced before execution for recurring schedules.
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		executor:      NewExecutor(slog.Default(), bridge, sm, "", nil),
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	now := time.Now()
	job := helperJob("at-most-once")
	job.State.NextRunAtMs = now.Add(-1 * time.Second).UnixMilli() // past due
	require.NoError(t, store.Create(context.Background(), job))
	s.jobs[job.ID] = job

	s.tickLoop.onTick()
	s.closed.Store(true) // prevent re-armed timer from firing
	s.tickLoop.stop()

	// Check that the job's next_run was advanced in the store.
	got, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.True(t, got.State.NextRunAtMs > now.UnixMilli(), "next_run should be advanced past now")
}

func TestOnTick_AtSchedule_AdvancesToPreventDupes(t *testing.T) {
	// Verifies that "at" schedules advance NextRunAtMs to prevent
	// duplicate execution across tick cycles. NextRun returns zero time
	// for past "at" schedules, which sets NextRunAtMs to a large negative
	// value. collectDue skips entries with NextRunAtMs <= 0.
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		executor:      NewExecutor(slog.Default(), bridge, sm, "", nil),
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	job := helperJob("at-advance")
	job.Schedule = CronSchedule{Kind: ScheduleAt, At: past}
	job.State.NextRunAtMs = time.Now().Add(-1 * time.Second).UnixMilli()
	require.NoError(t, store.Create(context.Background(), job))
	s.jobs[job.ID] = job

	s.tickLoop.onTick()
	s.closed.Store(true)
	s.tickLoop.stop()
	s.wg.Wait() // wait for executeJob goroutine to finish before reading state

	// next_run should have been advanced to zero time (negative UnixMilli),
	// preventing collectDue from picking it up again.
	got, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.True(t, got.State.NextRunAtMs <= 0,
		"at schedule should advance next_run to zero time (got %d)", got.State.NextRunAtMs)
}

func TestOnTick_AutoDisableAfterScheduleErrors(t *testing.T) {
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		executor:      NewExecutor(slog.Default(), bridge, sm, "", nil),
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	job := &CronJob{
		ID:       "cron_bad_sched",
		Name:     "bad-schedule",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "unknown"}, // will cause schedule error
		Payload:  CronPayload{Kind: PayloadIsolatedSession, Message: "test"},
		State: CronJobState{
			NextRunAtMs: time.Now().Add(-1 * time.Second).UnixMilli(),
			SchedErrs:   maxScheduleErrors - 1, // one more error → auto-disable
		},
	}
	// Put directly in memory — "unknown" kind violates DB CHECK constraint.
	s.jobs[job.ID] = job

	s.tickLoop.onTick()

	// collectDue returns clones, so read updated state from the map.
	got := s.jobs[job.ID]
	require.False(t, got.Enabled, "job should be auto-disabled after consecutive schedule errors")
}

func TestContainsAny(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s    string
		subs []string
		want bool
	}{
		{"hello world", []string{"world"}, true},
		{"hello world", []string{"xyz"}, false},
		{"rate limit exceeded", []string{"rate limit", "429"}, true},
		{"", []string{"a"}, false},
		{"abc", []string{""}, true}, // empty substring matches trivially
	}

	for _, tt := range tests {
		t.Run(tt.s[:min(20, len(tt.s))], func(t *testing.T) {
			t.Parallel()
			got := containsAny(tt.s, tt.subs...)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestMaxTimerInterval(t *testing.T) {
	require.Equal(t, 60*time.Second, maxTimerInterval)
}

func TestFinishExecution_ClearsRunningAtMs(t *testing.T) {
	// Regression: finishExecution must sync RunningAtMs=0 back to in-memory
	// via mergeJobState. Otherwise collectDue skips the job forever (line 411).
	store := newTestStore(t)
	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		executor:      NewExecutor(slog.Default(), bridge, sm, "", nil),
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	now := time.Now()
	job := helperJob("running-clear")
	job.State.NextRunAtMs = now.Add(-1 * time.Second).UnixMilli()
	require.NoError(t, store.Create(context.Background(), job))
	s.jobs[job.ID] = job

	// Fire tick → executes job in goroutine → will timeout (no worker).
	s.tickLoop.onTick()
	s.closed.Store(true)
	s.tickLoop.stop()
	s.wg.Wait()

	// After execution finishes, in-memory RunningAtMs must be 0.
	got := s.jobs[job.ID]
	require.Equal(t, int64(0), got.State.RunningAtMs,
		"RunningAtMs must be cleared after execution so collectDue does not skip the job")
	require.Equal(t, StatusFailed, got.State.LastStatus)
	require.Equal(t, 1, got.State.ConsecutiveErrs)
}

func TestGenerateJobID(t *testing.T) {
	id := GenerateJobID()
	require.True(t, strings.HasPrefix(id, "cron_"))
	require.Len(t, id, 5+36) // "cron_" (5) + UUID (36) = 41
}

// panicBridge panics on StartSession to test panic recovery in onTick.
type panicBridge struct{}

func (panicBridge) StartSession(_ context.Context, _ worker.SessionStartParams) error {
	panic("test panic in bridge")
}

func TestOnTick_PanicRecovery(t *testing.T) {
	store := newTestStore(t)
	sm := &mockSessionStateChecker{
		sessions: map[string]*session.SessionInfo{},
		workers:  map[string]worker.Worker{},
	}
	s := &Scheduler{
		log:           slog.Default(),
		store:         store,
		executor:      NewExecutor(slog.Default(), panicBridge{}, sm, "", nil),
		maxConcurrent: 3,
		ctx:           context.Background(),
		jobs:          map[string]*CronJob{},
		tickLoop:      newTimerLoop(&Scheduler{}),
	}
	s.tickLoop.scheduler = s

	now := time.Now()
	job := helperJob("panic-recovery")
	job.State.NextRunAtMs = now.Add(-1 * time.Second).UnixMilli()
	require.NoError(t, store.Create(context.Background(), job))
	s.jobs[job.ID] = job

	s.tickLoop.onTick()
	s.closed.Store(true)
	s.tickLoop.stop()
	s.wg.Wait()

	// After panic recovery, the job must not be stuck in running state.
	got := s.jobs[job.ID]
	require.Equal(t, int64(0), got.State.RunningAtMs,
		"RunningAtMs must be cleared after panic recovery")
	require.Equal(t, StatusFailed, got.State.LastStatus)
	require.Equal(t, 1, got.State.ConsecutiveErrs)
}
