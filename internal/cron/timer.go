package cron

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/observability"
)

// timerLoop manages the scheduler's timer-driven tick cycle.
type timerLoop struct {
	scheduler *Scheduler
	timer     *time.Timer
	mu        sync.Mutex
	running   atomic.Int32
}

func newTimerLoop(s *Scheduler) *timerLoop {
	return &timerLoop{scheduler: s}
}

// tryAcquireSlot atomically checks the concurrency cap and reserves a slot.
// Returns false if the cap is reached.
func (tl *timerLoop) tryAcquireSlot(max int) bool {
	for {
		cur := tl.running.Load()
		if int(cur) >= max {
			return false
		}
		if tl.running.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (tl *timerLoop) releaseSlot() {
	tl.running.Add(-1)
}

// arm sets the timer to fire at the given duration (capped at maxTimerInterval).
func (tl *timerLoop) arm(d time.Duration) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	if tl.timer != nil {
		tl.timer.Stop()
	}
	if d <= 0 {
		d = time.Second
	}
	if d > maxTimerInterval {
		d = maxTimerInterval
	}
	tl.timer = time.AfterFunc(d, tl.onTick)
}

func (tl *timerLoop) stop() {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.timer != nil {
		tl.timer.Stop()
	}
}

// maxConsecutiveErrors is the threshold for auto-disabling a job after
// repeated execution failures.
const maxConsecutiveErrors = 10

// maxScheduleErrors is the threshold for auto-disabling a job after
// repeated schedule computation failures.
const maxScheduleErrors = 5

const maxTimerInterval = 60 * time.Second

func (tl *timerLoop) onTick() {
	s := tl.scheduler
	if s.closed.Load() {
		return
	}

	now := time.Now()
	due := s.collectDue(now)
	if len(due) == 0 {
		tl.arm(s.nextTickDuration(now))
		return
	}

	s.log.Debug("cron: tick fired", "due_jobs", len(due), "now", now.Format(time.RFC3339))

	for _, job := range due {
		// At-most-once: advance next_run_at_ms BEFORE executing.
		// For all schedule types including "at": NextRun returns zero time
		// for past "at" schedules, which sets NextRunAtMs to a large negative
		// value. collectDue skips entries with NextRunAtMs <= 0, preventing
		// duplicate execution within the same tick cycle.
		next, err := NextRun(job.Schedule, now)
		if err != nil {
			s.log.Error("cron: compute next run", "job_id", job.ID, "err", err)
			job.State.SchedErrs++
			if job.State.SchedErrs >= maxScheduleErrors {
				s.log.Warn("cron: auto-disabling job after schedule errors", "job_id", job.ID, "errors", job.State.SchedErrs)
				job.Enabled = false
			}
		} else {
			job.State.NextRunAtMs = next.UnixMilli()
			job.State.SchedErrs = 0
		}

		// Persist state before execution.
		// If this fails, memory and disk diverge: the in-memory NextRunAtMs is
		// advanced but disk still has the old value. A crash+restart would cause
		// re-execution (at-least-once instead of at-most-once). We proceed anyway
		// because skipping execution here would also be wrong.
		// Uses background timeout (not s.ctx) so shutdown doesn't cancel the write.
		pctx, pcancel := s.persistTimeout()
		if err := s.store.UpdateState(pctx, job.ID, job.State); err != nil {
			s.log.Error("cron: persist state before execution",
				"job_id", job.ID, "err", err)
		}
		pcancel()
		if !job.Enabled {
			pctx, pcancel = s.persistTimeout()
			if err := s.store.SetEnabled(pctx, job.ID, false); err != nil {
				s.log.Error("cron: persist disabled job", "job_id", job.ID, "err", err)
			}
			pcancel()
			s.mergeJobState(job.ID, job.State, true)
			continue
		}
		s.mergeJobState(job.ID, job.State, false)

		// Execute with concurrency cap.
		if !tl.tryAcquireSlot(s.maxConcurrent) {
			s.log.Warn("cron: concurrency cap reached, skipping job", "job_id", job.ID)
			continue
		}

		// Re-check job still exists after acquiring slot — DeleteJob may have
		// removed it between collectDue and here, wasting a slot on an orphan.
		//
		// NOTE: This is a TOCTOU check — the gap between releasing s.mu here
		// and launching the goroutine means DeleteJob can still win. Consequence
		// is benign: the orphan goroutine executes on a cloned job, and all
		// subsequent mergeJobState/persistState calls silently no-op because
		// the job is no longer in s.jobs. The slot is held until completion but
		// no data corruption occurs.
		s.mu.Lock()
		_, exists := s.jobs[job.ID]
		s.mu.Unlock()
		if !exists {
			tl.releaseSlot()
			continue
		}

		// Fresh clone for the goroutine — mergeJobState updated the shared
		// in-memory entry's State, so we need an independent copy to avoid
		// data races when executeJob mutates state fields without holding s.mu.
		execJob := job.Clone()
		s.wg.Add(1)
		go func(j *CronJob) {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("cron: panic in executeJob",
						"job_id", j.ID, "name", j.Name, "panic", r)
					j.State.RunningAtMs = 0
					j.State.LastStatus = StatusFailed
					j.State.ConsecutiveErrs++
					s.mergeJobState(j.ID, j.State, false)
					s.persistState(j.ID, j.State)
				}
				tl.releaseSlot()
				s.wg.Done()
			}()
			observability.CronFires().Add(s.ctx, 1, metric.WithAttributes(attribute.String("job_name", j.Name)))
			s.executeJob(j, ScheduledTriggerFor(j))
		}(execJob)
	}

	tl.arm(s.nextTickDuration(now))
}

// executeJob runs a single job and updates its state.
// The job must be a clone (not a shared map pointer).
// Uses mergeJobState to avoid overwriting concurrent state changes (e.g. CLI disable).
func (s *Scheduler) executeJob(job *CronJob, trigger TriggerIdentity) {
	// Resolve workdir with system fallback: explicit > platform-specific > worker default.
	if job.WorkDir == "" && s.resolveWorkDir != nil {
		if resolved := s.resolveWorkDir(job); resolved != "" {
			job.WorkDir = resolved
		}
	}

	// Dispatch attached_session to dedicated handler.
	if job.Payload.Kind == PayloadAttachedSession {
		s.executeAttached(job)
		return
	}

	s.beginExecution(job)

	timeout := s.jobTimeout(job)
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	start := time.Now()
	result, err := s.executor.Execute(ctx, job, trigger, timeout)
	observability.CronDuration().Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("job_name", job.Name)))

	// LastRunID stays a display pointer to the latest run; it is not the run
	// fact itself — the occurrence is.
	job.State.LastRunID = result.SessionID
	if s.handlePostExecution(job, start.UnixMilli(), err, errorType(err)) {
		return
	}
	if result.Duplicate {
		// The trigger was already claimed by an earlier run. Nothing new was
		// executed and nothing new may be delivered.
		s.applyLifecycle(job)
		return
	}

	// Deliver results for successful isolated_session runs.
	if s.delivery != nil && !job.Silent && !HasCLIDelivery(job) {
		s.delivery.Deliver(s.ctx, job, result.SessionID)
	}
	s.applyLifecycle(job)
}

// executeAttached handles attached_session payload jobs.
// Fire-and-forget: the result stays in the target session.
func (s *Scheduler) executeAttached(job *CronJob) {
	if s.attachedHandler == nil {
		s.log.Warn("cron: attached_session execution skipped, no attached router",
			"job_id", job.ID, "name", job.Name)
		observability.CronAttached().Add(s.ctx, 1, metric.WithAttributes(attribute.String("result", "no_router")))
		job.State.LastStatus = StatusFailed
		s.persistState(job.ID, job.State)
		return
	}

	s.beginExecution(job)

	timeout := s.jobTimeout(job)
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	err := s.attachedHandler.Execute(ctx, job)

	if s.handlePostExecution(job, time.Now().UnixMilli(), err, "attached") {
		return
	}
	s.applyLifecycle(job)
}

// handlePostExecution handles error/disable/retry logic after execution.
// Returns true if the caller should stop (disabled, retrying, or errored).
func (s *Scheduler) handlePostExecution(job *CronJob, startedAtMs int64, err error, errType string) bool {
	shouldDisable := s.finishExecution(job, startedAtMs, err, errType)
	s.mergeJobState(job.ID, job.State, false)

	if shouldDisable {
		s.persistAndDisable(job.ID, job.State)
		return true
	}

	if err != nil {
		if job.Schedule.Kind == ScheduleAt && isTemporaryError(err) && job.State.RetryCount < maxRetries(job) {
			pctx, pcancel := s.persistTimeout()
			s.scheduleRetry(pctx, job)
			pcancel()
			return true
		}
		s.persistState(job.ID, job.State)
		return true
	}

	return false
}

// beginExecution marks a job as running and persists state.
func (s *Scheduler) beginExecution(job *CronJob) {
	now := time.Now().UnixMilli()
	job.State.RunningAtMs = now
	s.persistState(job.ID, job.State)
	s.mergeJobState(job.ID, job.State, false)
}

// finishExecution records the outcome of a job execution.
// It handles error bookkeeping (consecutive errors) and success bookkeeping.
// Returns true if the job should be auto-disabled due to consecutive failures.
// The caller is responsible for persisting disable and checking retry logic.
func (s *Scheduler) finishExecution(job *CronJob, startedAtMs int64, err error, errType string) bool {
	job.State.RunningAtMs = 0
	job.State.LastRunAtMs = startedAtMs

	if err != nil {
		s.log.Error("cron: job execution failed",
			"job_id", job.ID, "name", job.Name, "error_type", errType, "err", err)
		job.State.LastStatus = StatusFailed
		job.State.ConsecutiveErrs++
		s.log.Info("cron: error state recorded",
			"job_id", job.ID, "name", job.Name,
			"last_status", job.State.LastStatus, "consecutive_errors", job.State.ConsecutiveErrs)
		observability.CronErrors().Add(s.ctx, 1, metric.WithAttributes(attribute.String("job_name", job.Name), attribute.String("error_type", errType)))

		if job.State.ConsecutiveErrs >= maxConsecutiveErrors {
			s.log.Warn("cron: auto-disabling job after consecutive failures",
				"job_id", job.ID, "name", job.Name, "consecutive_errors", job.State.ConsecutiveErrs)
			return true
		}
		return false
	}

	job.State.LastStatus = StatusSuccess
	job.State.ConsecutiveErrs = 0
	job.State.RunCount++
	resetRetry(job)
	return false
}

// applyLifecycle handles post-execution lifecycle: one-shot delete/disable, recurring max_runs/expires_at.
func (s *Scheduler) applyLifecycle(job *CronJob) {
	// One-shot: disable or delete after run (success or permanent error).
	if job.Schedule.Kind == ScheduleAt {
		if job.DeleteAfterRun {
			ctx, cancel := s.persistTimeout()
			defer cancel()
			if err := s.store.Delete(ctx, job.ID); err != nil {
				s.log.Error("cron: delete one-shot job", "job_id", job.ID, "err", err)
			}
			s.mu.Lock()
			delete(s.jobs, job.ID)
			s.mu.Unlock()
			return
		}
		s.persistAndDisable(job.ID, job.State)
		return
	}

	// Recurring lifecycle: check max_runs and expires_at.
	shouldDisable := false
	if job.MaxRuns > 0 && job.State.RunCount >= job.MaxRuns {
		s.log.Info("cron: job reached max_runs, disabling",
			"job_id", job.ID, "name", job.Name, "run_count", job.State.RunCount, "max_runs", job.MaxRuns)
		shouldDisable = true
	} else if job.ExpiresAt != "" {
		if t, perr := time.Parse(time.RFC3339, job.ExpiresAt); perr == nil && time.Now().After(t) {
			s.log.Info("cron: job expired, disabling",
				"job_id", job.ID, "name", job.Name, "expires_at", job.ExpiresAt)
			shouldDisable = true
		}
	}

	s.persistState(job.ID, job.State)
	if shouldDisable {
		ctx, cancel := s.persistTimeout()
		defer cancel()
		if err := s.store.SetEnabled(ctx, job.ID, false); err != nil {
			s.log.Error("cron: persist disable", "job_id", job.ID, "err", err)
		}
	}
	s.mergeJobState(job.ID, job.State, shouldDisable)
}

// persistAndDisable persists state and disables a job in both store and memory.
// Used for one-shot jobs and auto-disabled jobs after consecutive failures.
func (s *Scheduler) persistAndDisable(jobID string, state CronJobState) {
	s.persistState(jobID, state)
	ctx, cancel := s.persistTimeout()
	defer cancel()
	if err := s.store.SetEnabled(ctx, jobID, false); err != nil {
		s.log.Error("cron: persist disable", "job_id", jobID, "err", err)
	}
	s.mergeJobState(jobID, state, true)
}

// persistTimeout returns a context with a 5-second timeout for store operations.
// Used by persist helpers that run outside the scheduler's main context
// (e.g. final state writes during shutdown, onTick persist calls).
func (s *Scheduler) persistTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// persistState saves job state to the store, using a background context
// so final state is not lost during scheduler shutdown.
func (s *Scheduler) persistState(jobID string, state CronJobState) {
	ctx, cancel := s.persistTimeout()
	defer cancel()
	if err := s.store.UpdateState(ctx, jobID, state); err != nil {
		if errors.Is(err, ErrJobNotFound) {
			s.log.Debug("cron: persist state skipped, job deleted", "job_id", jobID)
			return
		}
		s.log.Error("cron: persist state", "job_id", jobID, "err", err)
	}
}

// jobTimeout returns the timeout for a job, falling back to the scheduler default.
func (s *Scheduler) jobTimeout(job *CronJob) time.Duration {
	if job.TimeoutSec > 0 {
		return time.Duration(job.TimeoutSec) * time.Second
	}
	return s.defaultTimeout
}
