package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/agentconfig"
	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/noop"
	"github.com/hrygo/hotplex/pkg/events"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// forwardOpts configures the forwardEvents goroutine behavior.
type forwardOpts struct {
	ctx         context.Context
	resumed     bool
	workDir     string
	retryDepth  int
	lastInput   string
	lastReplay  worker.InputReplay
	workerRunID string
}

// workerLaunchParams holds the parameters for createAndLaunchWorker.
type workerLaunchParams struct {
	ctx                context.Context
	wt                 worker.WorkerType
	workerInfo         worker.SessionInfo
	platform           string
	scope              agentconfig.RuntimeScopeKind
	botID              string
	botName            string
	forwardOpts        *forwardOpts
	injectExclude      []string          // per-session agent config files to skip; nil = use platform default
	workspaceOverrides map[string]string // WebChat per-workspace config overrides (spec ②); nil = Message Channel track → Load
	// facts carries the session-level authority facts the runtime plan needs
	// but worker.SessionInfo deliberately does not carry: a Worker adapter has
	// no use for a workspace id or a captured ceiling, and adding them would
	// widen the Worker contract for a gateway-only concern.
	facts launchFacts
}

// launchFacts are the session-scoped inputs to the runtime plan that do not
// travel on worker.SessionInfo.
type launchFacts struct {
	workspaceID string
	// sessionCeiling is the permission tier captured when the session started.
	// It is immutable for the life of the session, so a later config edit
	// cannot widen what this session may do.
	sessionCeiling string
	platformKey    map[string]string
}

// workerStartFunc is called after AttachWorker and injectAgentConfig.
// On startFn failure, the worker is automatically detached.
type workerStartFunc func(ctx context.Context, w worker.Worker, info worker.SessionInfo) error

// workerAttachErrFunc is called when AttachWorker fails for caller-specific cleanup.
type workerAttachErrFunc func(w worker.Worker, err error)

// createAndLaunchWorker creates a worker, attaches it, injects config, calls startFn,
// and launches the forwardEvents goroutine. Returns the worker for post-launch use.
// On startFn failure, the worker is detached before returning.
func (b *Bridge) createAndLaunchWorker(params workerLaunchParams, startFn workerStartFunc, attachErrFn workerAttachErrFunc) (worker.Worker, error) {
	sid := params.workerInfo.SessionID
	if params.forwardOpts == nil {
		params.forwardOpts = &forwardOpts{}
	}
	if params.forwardOpts.ctx == nil {
		params.forwardOpts.ctx = params.ctx
	}
	if params.forwardOpts.workerRunID == "" {
		params.forwardOpts.workerRunID = "run_" + uuid.NewString()
	}

	start := time.Now()
	defer func() {
		observability.WorkerCreationDuration().Record(params.ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("worker_type", string(params.wt))))
	}()

	w, err := b.wf.NewWorker(params.wt)
	if err != nil {
		return nil, fmt.Errorf("bridge: create worker: %w", err)
	}

	if noopw, ok := w.(*noop.Worker); ok {
		noopw.SetConn(noop.NewConn(sid, params.workerInfo.UserID))
	}

	// The plan is resolved ONCE here, at the single chokepoint every launch
	// passes through, and bound to the run below. Resolving it later — from a
	// diagnostic, or after a config hot-reload — would describe a config the
	// run never saw.
	//
	// It is resolved BEFORE AttachWorker on purpose: in authoritative mode a
	// blocked plan must refuse the launch, and refusing after attaching would
	// leave a Worker registered against a session that never runs one.
	plan := b.prepareLaunchPlan(params)
	if plan.Mode == config.RuntimePlanModeAuthoritative {
		if plan.Blocked() {
			_ = w.Terminate(context.Background())
			if attachErrFn != nil {
				attachErrFn(w, fmt.Errorf("%w: %s", ErrPlanLaunchRefused,
					strings.Join(plan.BlockedCodes, ",")))
			}
			return nil, fmt.Errorf("%w: %s", ErrPlanLaunchRefused,
				strings.Join(plan.BlockedCodes, ","))
		}
		if err := b.sharedRuntime.checkAndRecord(params.wt, processScopedProfile(plan)); err != nil {
			_ = w.Terminate(context.Background())
			if attachErrFn != nil {
				attachErrFn(w, err)
			}
			return nil, err
		}
		// From here the plan, not the legacy parameters, decides what starts.
		params.workerInfo = applyAuthoritativePlan(plan.Plan.AgentSpec, params.workerInfo)
		plan.Applied = true
	}

	// Isolation is asked of the WORKER, never inferred from what we requested.
	// A configuration that requires a boundary the Worker cannot prove gets a
	// refusal, not a launch with weaker guarantees than were asked for.
	plan.Isolation = worker.ReportIsolation(params.ctx, w, params.workerInfo)
	// The generic default report is scope-agnostic, so correct it here where
	// the shared-process knowledge actually lives. For these Workers every
	// session sits on the same process and therefore the same boundary; a
	// per-session scope would be a claim about an environment that does not
	// exist.
	if _, shared := sharedProcessWorkers[params.wt]; shared {
		plan.Isolation.Scope = worker.IsolationScopeProcess
	}
	if cfg := b.currentConfig(); cfg != nil {
		if missing := isolationShortfall(cfg.Worker.RequireIsolation, plan.Isolation); len(missing) > 0 {
			reason := isolationRefusal(missing, plan.Isolation)
			_ = w.Terminate(context.Background())
			if attachErrFn != nil {
				attachErrFn(w, fmt.Errorf("%w: %s", ErrPlanLaunchRefused, reason))
			}
			return nil, fmt.Errorf("%w: %s", ErrPlanLaunchRefused, reason)
		}
	}

	if err := b.sm.AttachWorker(params.ctx, sid, w); err != nil {
		if attachErrFn != nil {
			attachErrFn(w, err)
		}
		return nil, fmt.Errorf("bridge: attach worker: %w", err)
	}

	facts := buildRuntimeFacts(w, params.workerInfo, params.platform, params.scope)
	b.injectAgentConfig(&params.workerInfo, facts, params.platform, params.botName, params.botID, params.injectExclude, params.workspaceOverrides)

	if err := startFn(params.ctx, w, params.workerInfo); err != nil {
		b.sm.DetachWorker(sid)
		return nil, err
	}
	if err := b.capturePermissionCeiling(params.ctx, sid, w); err != nil {
		_ = w.Terminate(context.Background())
		b.sm.DetachWorker(sid)
		if attachErrFn != nil {
			attachErrFn(w, err)
		}
		return nil, err
	}
	runBinding := b.bindWorkerRun(sid, w, params.forwardOpts.workerRunID, plan)

	// A new Worker (or a resumed/replaced one) is now the session's command
	// authority: drop the session's cached catalog so the next assembly picks
	// up the fresh Worker's command set (spec §5.2, §8.7). This single
	// chokepoint covers StartSession, ResumeSession, StartFreshWorker, and
	// crash-recovery fresh starts.
	if b.catalogInvalidate != nil {
		b.catalogInvalidate(sid)
	}

	// Best-effort async persist so WorkerSessionID survives gateway restart
	// even if no turn events arrive (SIGTERM before first Prompt). The
	// correctness guarantee comes from forwardEvents' first-event safety-net.
	// Tracked by fwdWg so graceful shutdown waits for the DB write to complete.
	b.fwdWg.Add(1)
	go func() {
		defer b.fwdWg.Done()
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("bridge: panic in persistWorkerSessionID", "session_id", sid, "panic", r)
			}
		}()
		b.persistWorkerSessionIDInternal(params.ctx, w, sid, false)
	}()

	b.fwdWg.Add(1)
	go func() {
		defer b.fwdWg.Done()
		defer close(runBinding.lifecycle.done)
		defer b.clearWorkerRun(sid, w, runBinding.id)
		b.launchForwarderLocked(runBinding, sid, *params.forwardOpts)
	}()

	return w, nil
}

// capturePermissionCeiling persists the first true effective permission tier
// before the Worker becomes observable through forwarding. A concurrent
// first-writer mismatch fences this Worker instead of allowing it to run above
// the already-established session ceiling.
func (lifecycle *workerRunLifecycle) setTurnTimeoutHandler(handler func(uint64)) {
	if lifecycle == nil {
		return
	}
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	lifecycle.turnTimeoutHandler = handler
	if handler != nil && !lifecycle.turnTimeoutDeadline.IsZero() {
		lifecycle.scheduleTurnTimeoutLocked(lifecycle.turnTimeoutGeneration, lifecycle.turnTimeoutDeadline)
	}
}

func (lifecycle *workerRunLifecycle) armTurnTimeout(deadline time.Time) uint64 {
	if lifecycle == nil || deadline.IsZero() {
		return 0
	}
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	lifecycle.turnTimeoutGeneration++
	lifecycle.turnTimeoutDeadline = deadline
	if lifecycle.turnTimeoutTimer != nil {
		lifecycle.turnTimeoutTimer.Stop()
		lifecycle.turnTimeoutTimer = nil
	}
	if lifecycle.turnTimeoutHandler != nil {
		lifecycle.scheduleTurnTimeoutLocked(lifecycle.turnTimeoutGeneration, deadline)
	}
	return lifecycle.turnTimeoutGeneration
}

func (lifecycle *workerRunLifecycle) scheduleTurnTimeoutLocked(generation uint64, deadline time.Time) {
	if lifecycle.turnTimeoutTimer != nil {
		lifecycle.turnTimeoutTimer.Stop()
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	lifecycle.turnTimeoutTimer = time.AfterFunc(delay, func() {
		lifecycle.turnTimeoutMu.Lock()
		if lifecycle.turnTimeoutGeneration != generation || lifecycle.turnTimeoutDeadline.IsZero() {
			lifecycle.turnTimeoutMu.Unlock()
			return
		}
		handler := lifecycle.turnTimeoutHandler
		lifecycle.turnTimeoutTimer = nil
		lifecycle.turnTimeoutMu.Unlock()
		if handler != nil {
			handler(generation)
		}
	})
}

func (lifecycle *workerRunLifecycle) turnTimeoutCurrent(generation uint64) bool {
	if lifecycle == nil {
		return false
	}
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	return generation != 0 &&
		generation == lifecycle.turnTimeoutGeneration &&
		!lifecycle.turnTimeoutDeadline.IsZero()
}

func (lifecycle *workerRunLifecycle) stopTurnTimeout() {
	if lifecycle == nil {
		return
	}
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	lifecycle.turnTimeoutGeneration++
	lifecycle.turnTimeoutDeadline = time.Time{}
	if lifecycle.turnTimeoutTimer != nil {
		lifecycle.turnTimeoutTimer.Stop()
		lifecycle.turnTimeoutTimer = nil
	}
}

func (lifecycle *workerRunLifecycle) clearTurnTimeoutHandler() {
	if lifecycle == nil {
		return
	}
	lifecycle.turnTimeoutMu.Lock()
	defer lifecycle.turnTimeoutMu.Unlock()
	lifecycle.turnTimeoutGeneration++
	lifecycle.turnTimeoutDeadline = time.Time{}
	lifecycle.turnTimeoutHandler = nil
	if lifecycle.turnTimeoutTimer != nil {
		lifecycle.turnTimeoutTimer.Stop()
		lifecycle.turnTimeoutTimer = nil
	}
}

func (b *Bridge) capturePermissionCeiling(ctx context.Context, sessionID string, w worker.Worker) error {
	reporter, ok := w.(worker.PermissionCeilingReporter)
	if !ok {
		return nil
	}
	ceiling, initialized := reporter.PermissionCeiling()
	if !initialized {
		return fmt.Errorf("bridge: worker permission ceiling: %w", worker.ErrPermissionCeilingUnset)
	}
	stored, err := b.sm.SetPermissionCeilingIfEmpty(ctx, sessionID, ceiling)
	if err != nil {
		return fmt.Errorf("bridge: persist permission ceiling: %w", err)
	}
	var persisted worker.PermissionCeiling
	if err := persisted.Capture(stored); err != nil {
		return fmt.Errorf("bridge: invalid persisted permission ceiling: %w", err)
	}
	// The store-level check only fences the WIDER-than-store direction: a
	// replacement Worker that reports a ceiling above the persisted value is
	// rejected. A stricter Worker (below the stored ceiling) is allowed because
	// the Worker's own local PermissionCeiling is the authoritative runtime
	// fence — it already blocks escalation at the Worker. The persisted value
	// is metadata for resume/restart, not a second enforcement point.
	if _, err := persisted.Check(ceiling); err != nil {
		return fmt.Errorf("bridge: concurrent permission ceiling mismatch: %w", err)
	}
	return nil
}

// forwarderBinding is the immutable ownership contract captured synchronously
// before a forwardEvents goroutine is spawned. Freezing the Conn and reset
// generation at launch — instead of reading w.Conn() from inside the spawned
// goroutine — prevents a stale forwarder from binding to a replacement Conn
// after /reset and splitting the event stream with the new forwarder
// (Turn-Integrity spec RC-1 / Fix A, invariant I-1/I-2).
type forwarderBinding struct {
	worker    worker.Worker
	conn      worker.SessionConn // frozen event source; forwardEvents reads ONLY this
	lifecycle *workerRunLifecycle
	resetGen  int64 // frozen reset generation for stale-exit detection
}

// launchForwarderLocked is the single entry point for spawning a forwardEvents
// goroutine. It MUST be called from the launching (synchronous) goroutine so
// the Conn is captured before any concurrent ResetContext can replace it.
// All four launch paths — fresh Start, Resume, /reset ConnReplaced, and crash
// recovery — route through here (Fix A).
func (b *Bridge) launchForwarderLocked(binding workerRunBinding, sessionID string, opts forwardOpts) {
	w := binding.worker
	var resetGen int64
	if rg, ok := w.(worker.ResetGenerationer); ok {
		resetGen = rg.LoadResetGeneration()
	}
	fb := forwarderBinding{
		worker:    w,
		conn:      binding.lifecycle.conn,
		lifecycle: binding.lifecycle,
		resetGen:  resetGen,
	}
	b.forwardEvents(fb, sessionID, opts)
}

// bindWorkerRun binds a launched Worker to its run identity AND to the plan
// that described the launch (#946 D2).
//
// Keeping the launch fingerprint on the run is what lets a diagnostic answer
// "which plan was this run launched under" from history, instead of re-resolving
// the plan against the CURRENT config and presenting the result as if it were
// a historical fact.
func (b *Bridge) bindWorkerRun(sessionID string, w worker.Worker, runID string, plan launchPlan) workerRunBinding {
	if runID == "" {
		runID = "run_" + uuid.NewString()
	}
	// plan is copied so the binding owns an immutable snapshot: the caller may
	// keep mutating its own copy without changing what the run is recorded as
	// having launched under.
	planSnapshot := plan
	binding := workerRunBinding{
		worker:     w,
		id:         runID,
		lifecycle:  newWorkerRunLifecycle(w.Conn()),
		launchPlan: &planSnapshot,
	}
	b.workerRuns.Store(sessionID, binding)
	return binding
}

// LaunchPlanFor returns the plan the session's CURRENT run was launched under.
//
// Diagnostics must prefer this over re-resolving the plan against the live
// config: a re-resolution answers "what would launch now", which is a different
// question from "what did this run launch under", and presenting the first as
// the second is how a historical fact turns into a plausible fiction.
func (b *Bridge) LaunchPlanFor(sessionID string) (agentspec.EffectiveRuntimePlan, bool) {
	value, ok := b.workerRuns.Load(sessionID)
	if !ok {
		return agentspec.EffectiveRuntimePlan{}, false
	}
	binding, ok := value.(workerRunBinding)
	if !ok || binding.launchPlan == nil {
		return agentspec.EffectiveRuntimePlan{}, false
	}
	return binding.launchPlan.Plan, true
}

// LaunchPlanApplied reports whether the recorded run was actually started from
// its plan, as opposed to shadow-comparing against it. Without this, a
// matching plan and an applied plan look identical from outside.
func (b *Bridge) LaunchPlanApplied(sessionID string) bool {
	value, ok := b.workerRuns.Load(sessionID)
	if !ok {
		return false
	}
	binding, ok := value.(workerRunBinding)
	return ok && binding.launchPlan != nil && binding.launchPlan.Applied
}

// RecordTurnStart stamps the current turn's start time on the session
// accumulator. Called by the input path immediately before delivering the
// user's input to the worker. Safe to call when the accumulator does not yet
// exist (it is created on-demand). Turn-Integrity Fix D.
func (b *Bridge) RecordTurnStart(sessionID string) {
	_, _, _, _ = b.RecordTurnStartForRun(sessionID, "")
}

// RecordTurnStartForRun records the input-path start stamp and arms the
// current worker run's absolute timeout. The returned values are the exact
// deadline snapshot to persist with the durable execution record.
func (b *Bridge) RecordTurnStartForRun(sessionID, expectedRunID string) (startedAt, deadlineAt int64, policyRevision string, err error) {
	started := time.Now()
	acc := b.getOrInitAccum(sessionID, "", started)
	if expectedRunID != "" {
		binding, ok := b.currentWorkerRunBinding(sessionID, expectedRunID)
		if !ok || binding.lifecycle == nil {
			return 0, 0, "", errWorkerRunChanged
		}
		acc.recordTurnStart(started)
		startedAt = started.UnixMilli()
		if b.turnTimeout > 0 {
			deadline := started.Add(b.turnTimeout)
			binding.lifecycle.armTurnTimeout(deadline)
			return startedAt, deadline.UnixMilli(), turnTimeoutRevision(b.turnTimeout), nil
		}
		return startedAt, 0, "", nil
	}

	acc.recordTurnStart(started)
	startedAt = started.UnixMilli()
	if b.turnTimeout <= 0 {
		return startedAt, 0, "", nil
	}
	if binding, ok := b.currentWorkerRunBinding(sessionID, ""); ok && binding.lifecycle != nil {
		deadline := started.Add(b.turnTimeout)
		binding.lifecycle.armTurnTimeout(deadline)
		return startedAt, deadline.UnixMilli(), turnTimeoutRevision(b.turnTimeout), nil
	}
	return startedAt, 0, "", nil
}

func turnTimeoutRevision(timeout time.Duration) string {
	return fmt.Sprintf("worker.turn_timeout=%s", timeout)
}

func (b *Bridge) RecordTurnDeadlineForRun(sessionID, expectedRunID string, startedAt, deadlineAt int64) error {
	if startedAt <= 0 || deadlineAt <= startedAt {
		return errors.New("bridge: valid persisted turn deadline is required")
	}
	binding, ok := b.currentWorkerRunBinding(sessionID, expectedRunID)
	if !ok || binding.lifecycle == nil {
		return errWorkerRunChanged
	}
	b.getOrInitAccum(sessionID, "", time.UnixMilli(startedAt)).recordTurnStart(time.UnixMilli(startedAt))
	binding.lifecycle.armTurnTimeout(time.UnixMilli(deadlineAt))
	return nil
}

// ClearTurnStart clears the current turn's start stamp. Called by the input
// path when delivery to the worker failed, so the subsequent Done does not
// bill a turn that never started. Turn-Integrity Fix D.
func (b *Bridge) ClearTurnStart(sessionID string) {
	b.ClearTurnStartForRun(sessionID, "")
}

func (b *Bridge) ClearTurnStartForRun(sessionID, expectedRunID string) {
	acc := b.getOrInitAccum(sessionID, "", time.Now())
	acc.clearTurnStart()
	if binding, ok := b.currentWorkerRunBinding(sessionID, expectedRunID); ok && binding.lifecycle != nil {
		binding.lifecycle.stopTurnTimeout()
	}
}

// consumeTurnStartMs reads and clears the current turn's start stamp. Called by
// the forwarder on Done to compute a duration that excludes inter-turn idle.
// Returns 0 when no start was recorded (crash recovery / replay); the caller
// falls back to the first worker event time.
func (b *Bridge) consumeTurnStartMs(sessionID string) int64 {
	acc := b.getOrInitAccum(sessionID, "", time.Now())
	return acc.consumeTurnStartMs()
}

// clearWorkerRun conditionally removes a binding. Matching both Worker and run
// ID prevents an old forwarder (including an in-place reset on the same Worker)
// from deleting the replacement binding.
func (b *Bridge) clearWorkerRun(sessionID string, expectedWorker worker.Worker, expectedRunID string) {
	value, ok := b.workerRuns.Load(sessionID)
	if !ok {
		return
	}
	binding, ok := value.(workerRunBinding)
	if !ok || binding.worker != expectedWorker || (expectedRunID != "" && binding.id != expectedRunID) {
		return
	}
	b.workerRuns.CompareAndDelete(sessionID, value)
}

func (b *Bridge) suspendWorkerRun(sessionID string, expectedWorker worker.Worker) (workerRunBinding, bool) {
	for {
		value, ok := b.workerRuns.Load(sessionID)
		if !ok {
			return workerRunBinding{}, false
		}
		binding, ok := value.(workerRunBinding)
		if !ok || binding.worker != expectedWorker {
			return workerRunBinding{}, false
		}
		if b.workerRuns.CompareAndDelete(sessionID, value) {
			return binding, true
		}
	}
}

func (b *Bridge) restoreWorkerRun(sessionID string, binding workerRunBinding) {
	if binding.worker == nil || binding.id == "" || b.sm == nil || b.sm.GetWorker(sessionID) != binding.worker {
		return
	}
	b.workerRuns.LoadOrStore(sessionID, binding)
}

func (b *Bridge) persistWorkerSessionIDEnsure(ctx context.Context, w worker.Worker, sessionID string) {
	b.persistWorkerSessionIDInternal(ctx, w, sessionID, true)
}

func (b *Bridge) persistWorkerSessionIDInternal(_ context.Context, w worker.Worker, sessionID string, force bool) {
	// Use Background to fully detach from the request context. The async DB
	// write outlives the request, so inheriting trace baggage would mislead
	// trace analysis (spans from a completed request's children).
	ctx := context.Background()
	handler, ok := w.(worker.WorkerSessionIDHandler)
	if !ok {
		return
	}
	workerSID := handler.GetWorkerSessionID()
	if workerSID == "" {
		return
	}
	var err error
	if force {
		err = b.sm.EnsureWorkerSessionID(ctx, sessionID, workerSID)
	} else {
		err = b.sm.UpdateWorkerSessionID(ctx, sessionID, workerSID)
	}
	if err != nil {
		b.log.Warn("bridge: failed to persist worker session ID", "session_id", sessionID, "worker_session_id", workerSID, "err", err)
	} else {
		b.log.Debug("bridge: persisted worker session ID", "session_id", sessionID, "worker_session_id", workerSID)
	}
}

// fallbackParams carries the context needed by attemptResumeFallback.
type fallbackParams struct {
	sessionID     string
	workDir       string
	exitCode      int
	retryDepth    int
	workerType    worker.WorkerType
	lastInput     string
	lastReplay    worker.InputReplay
	crashedWorker worker.Worker
	sessPlatform  string
	sessOwner     string
	accGeneration int64
	accModelName  string
}

// attemptResumeFallback handles a crashed worker with a two-step strategy:
//  1. retryDepth < 1: Retry resume once to preserve conversation history (transient failures).
//  2. retryDepth >= 1: Fall back to fresh start — conversation data is permanently lost.
//
// Applies to both fresh and resumed sessions. Workers that cannot resume
// will gracefully fall back to fresh Start() via ErrFellBackToFreshStart.
//
// Returns true if a new forwardEvents goroutine took over.
func (b *Bridge) attemptResumeFallback(p fallbackParams) bool {
	b.log.Warn("bridge: worker crashed shortly after resume",
		"session_id", p.sessionID, "worker_type", p.workerType, "exit_code", p.exitCode, "retry_depth", p.retryDepth)

	// Crash-loop protection: if this session has crashed crashLoopMax times within
	// crashLoopWindow, stop retrying to prevent thread exhaustion that can kill
	// the entire gateway process (pthread_create failed → SIGABRT).
	if b.recordCrashLoop(p.sessionID) {
		b.log.Error("bridge: crash loop detected, aborting retry to protect gateway",
			"session_id", p.sessionID, "worker_type", p.workerType, "max", crashLoopMax, "window", crashLoopWindow)
		b.sendError(p.sessionID, events.ErrCodeWorkerCrash, "Worker crashed %d times in %v. Stopping retry to protect gateway stability.", crashLoopMax, crashLoopWindow)
		return false
	}

	// Clean up the crashed worker first.
	b.cleanupCrashedWorker(p.sessionID, p.crashedWorker)

	// Step 1: Retry resume once for transient failures (e.g., file lock, timing).
	if p.retryDepth == 0 {
		if err := b.resumeWithOpts(context.Background(), p.sessionID, p.workDir, forwardOpts{resumed: true, workDir: p.workDir, retryDepth: p.retryDepth + 1, lastInput: p.lastInput, lastReplay: p.lastReplay}); err != nil {
			if errors.Is(err, ErrResumeSequenceUnavailable) {
				b.log.Warn("bridge: resume retry blocked because sequence hydration is unavailable; preserving session context",
					"session_id", p.sessionID, "worker_type", p.workerType, "err", err)
				return false
			}
			if errors.Is(err, worker.ErrResumeCheckFailed) {
				b.log.Warn("bridge: resume verification failed during crash recovery; preserving session context",
					"session_id", p.sessionID, "worker_type", p.workerType, "err", err)
				b.sendError(p.sessionID, events.ErrCodeInternalError, "Unable to verify the previous worker session. Please retry later.")
				return false
			}
			b.log.Warn("bridge: resume retry failed synchronously, falling back to fresh start", "session_id", p.sessionID, "worker_type", p.workerType, "err", err)
		} else {
			b.log.Info("bridge: resume retry succeeded", "session_id", p.sessionID, "worker_type", p.workerType)
			b.sendError(p.sessionID, events.ErrCodeResumeRetry, "Worker crashed after resume (exit %d), retried resume to preserve conversation.", p.exitCode)
			return true
		}
	}

	// Step 2: Resume retry also failed or retryDepth exhausted — start fresh worker.
	b.log.Info("bridge: starting fresh worker after failed resume", "session_id", p.sessionID, "worker_type", p.workerType)

	si, err := b.sm.Get(context.Background(), p.sessionID)
	if err != nil {
		b.log.Error("bridge: session not found for fresh start fallback", "session_id", p.sessionID, "err", err)
		return false
	}

	if si.State == events.StateTerminated {
		if err := b.sm.Transition(context.Background(), p.sessionID, events.StateRunning); err != nil {
			b.log.Error("bridge: pre-attach transition for fresh start", "session_id", p.sessionID, "err", err)
			return false
		}
		si.State = events.StateRunning
	}

	workerInfo := b.prepareWorkerInfo(si.ID, si.UserID, p.workDir, si)

	w, err := b.createAndLaunchWorker(workerLaunchParams{
		ctx:                context.Background(),
		wt:                 si.WorkerType,
		workerInfo:         workerInfo,
		platform:           si.Platform,
		scope:              runtimeScopeForSession(si.WorkspaceID, si.BotID, si.BotName),
		botID:              si.BotID,
		botName:            si.BotName,
		forwardOpts:        &forwardOpts{workDir: p.workDir},
		injectExclude:      nil, // resolved by injectAgentConfig
		workspaceOverrides: b.resolveWorkspaceOverrides(context.Background(), si.WorkspaceID),
		facts:              launchFactsFor(si),
	},
		func(ctx context.Context, w worker.Worker, info worker.SessionInfo) error {
			if si.State != events.StateRunning {
				if err := b.sm.Transition(ctx, p.sessionID, events.StateRunning); err != nil {
					b.log.Warn("bridge: transition to running for fresh start", "session_id", p.sessionID, "err", err)
				}
			}
			if err := w.Start(ctx, info); err != nil {
				b.log.Warn("bridge: fresh worker start failed", "session_id", p.sessionID, "err", err)
				return err
			}
			return nil
		},
		func(_ worker.Worker, err error) {
			b.log.Error("bridge: attach worker for fresh start", "session_id", p.sessionID, "err", err)
		},
	)
	if err != nil {
		return false
	}

	// Re-deliver the original input that was lost when the first worker crashed.
	b.captureSyntheticEvent(syntheticTurnParams{
		SessionID:  p.sessionID,
		Reason:     "fresh_start",
		Message:    "Session restarted with context reset after worker crash",
		Source:     eventstore.SourceFreshStart,
		Platform:   p.sessPlatform,
		Owner:      p.sessOwner,
		Model:      p.accModelName,
		Generation: p.accGeneration,
		TurnNum:    0, // fresh start resets turn count
	})
	if p.lastInput != "" || p.lastReplay.Content != "" || p.lastReplay.Skill != nil {
		replay := p.lastReplay
		if replay.Content == "" && replay.Skill == nil {
			replay.Content = p.lastInput
		}
		b.log.Info("bridge: re-delivering input to fresh worker", "session_id", p.sessionID, "content_len", len(replay.Content), "skill", replay.Skill != nil)
		if err := b.deliverInputReplayForSession(context.Background(), p.sessionID, w, replay); err != nil {
			b.log.Warn("bridge: input re-delivery failed", "session_id", p.sessionID, "err", err)
		}
	}

	b.log.Info("bridge: fresh worker started after resume failure", "session_id", p.sessionID, "worker_type", p.workerType)
	notifyMsg := buildNotifyEnvelope(p.sessionID,
		"🔄 会话已重新启动，上下文已重置。",
		0)
	_ = b.hub.SendToSession(context.Background(), notifyMsg)
	return true
}

// cleanupCrashedWorker detaches the dead worker and transitions the session to TERMINATED
// so the next message triggers orphan resume instead of silently dropping input.
// Uses CAS via crashedWorker to avoid detaching a worker that was already replaced
// by a concurrent ResumeSession or attemptResumeFallback.
func (b *Bridge) cleanupCrashedWorker(sessionID string, crashedWorker worker.Worker) {
	acc := b.getOrInitAccum(sessionID, "", time.Now())
	wt := worker.TypeUnknown
	if crashedWorker != nil {
		wt = crashedWorker.Type()
	}
	b.log.Debug("bridge: cleaning up crashed worker",
		"session_id", sessionID, "worker_type", wt, "turn_count", acc.TurnCount.Load())
	if b.sm == nil {
		return
	}
	if crashedWorker != nil {
		if !b.sm.DetachWorkerIf(sessionID, crashedWorker) {
			b.log.Debug("bridge: crashed worker already replaced, skipping cleanup",
				"session_id", sessionID, "worker_type", wt)
			return
		}
	} else {
		b.sm.DetachWorker(sessionID)
	}
	if crashedWorker != nil {
		b.clearWorkerRun(sessionID, crashedWorker, "")
	}
	if err := b.sm.Transition(context.Background(), sessionID, events.StateTerminated); err != nil {
		b.log.Debug("bridge: transition to terminated after worker exit", "session_id", sessionID, "err", err)
	}
	b.accumMu.Lock()
	delete(b.accum, sessionID)
	b.accumMu.Unlock()
	b.compressCache.Delete(sessionID)

	b.crashTrackerMu.Lock()
	delete(b.crashTracker, sessionID)
	b.crashTrackerMu.Unlock()
}

// resolveInjectExclude returns the inject_exclude list for a platform, falling
// back from the per-session value to the platform/global default in the atomic
// config map. Used by injectAgentConfig and crash recovery paths.
func (b *Bridge) resolveInjectExclude(platform string, perSession []string) []string {
	if perSession != nil {
		return perSession
	}
	if m, ok := b.agentConfigExclude.Load().(map[string][]string); ok {
		if excl, found := m[platform]; found {
			return slices.Clone(excl)
		}
		if excl, found := m[""]; found {
			return slices.Clone(excl)
		}
	}
	return nil
}

// resolveWorkspaceOverrides fetches a workspace's agent-config overrides and parses
// them. Returns nil for empty workspaceID (Message Channel track) or nil wsStore, and
// degrades to nil (team defaults) on any fetch/parse error — never blocks worker launch.
// ctx propagates request-scoped cancellation/deadline to the workspace DB query (the
// store layer uses standard QueryRowContext without otelsql instrumentation, so ctx
// does not auto-generate an OTel DB span). See design spec §7.3.
func (b *Bridge) resolveWorkspaceOverrides(ctx context.Context, workspaceID string) map[string]string {
	if workspaceID == "" || b.wsStore == nil {
		return nil
	}
	ws, err := b.wsStore.GetWorkspaceByID(ctx, workspaceID)
	if err != nil {
		b.warnOverrideDegrade(workspaceID, "fetch workspace overrides failed, degrading to team defaults", err)
		return nil
	}
	overrides, err := agentconfig.ValidateOverrides(ws.AgentConfigOverrides)
	if err != nil {
		b.warnOverrideDegrade(workspaceID, "parse workspace overrides failed, degrading to team defaults", err)
		return nil
	}
	// Valid overrides (or empty): clear any prior warning flag so a future
	// regression is warned again (#749).
	b.warnedOverrides.Delete(workspaceID)
	return overrides
}

// resolveWorkspacePermissionMode returns the effective permission mode tier for a
// session (#789, r3 #804). The returned tier is a CEILING on permissiveness — each
// worker clamps its operator config to never exceed it (codex: most-restrictive of
// session tier vs cfg.Sandbox/ApprovalMode; ACP: auto_approve gated off for tiers
// stricter than bypass), so an injected default can tighten but never escalate.
// Precedence:
//
//   - Sessions WITHOUT a workspace (platform/cron: Slack/Feishu/cron-driven) → "".
//     Each worker applies its OWN default/config (codex honors cfg.Sandbox, ACP
//     honors cfg.AutoApprove; CC/OCS map "" to their own bypass). Injecting a global
//     default here would override an operator's restricted codex.sandbox /
//     acp.auto_approve (#789 P1).
//   - Workspace with an explicit override → that tier wins.
//   - Workspace with NO explicit override, or a fetch error → the bridge's global
//     default (config.worker.default_permission_mode, seeded "workspace" in r3).
//     Returning the default (not "") on fetch error is fail-closed: for CC/OCS ""
//     maps to bypass, so an empty degrade would re-open the r2 P1 escalation on a
//     transient DB outage.
//
// ctx note: GetWorkspaceByID uses shutdownCtx rather than a request-scoped ctx because
// buildWorkerInfo/prepareWorkerInfo intentionally carry no ctx (see #714); the query is
// fast and degrades harmlessly, so request-scoped cancellation isn't propagated here
// (unlike resolveWorkspaceOverrides, whose call sites carry a ctx). #789 review UNCERTAIN.
func (b *Bridge) resolveWorkspacePermissionMode(workspaceID string) string {
	if workspaceID == "" || b.wsStore == nil {
		return ""
	}
	defaultMode, _ := b.defaultPermissionMode.Load().(string)
	ws, err := b.wsStore.GetWorkspaceByID(b.shutdownCtx, workspaceID)
	if err != nil {
		// Fail-closed (#804 r3 review): degrade to the bridge default (workspace)
		// rather than "". For CC/OCS "" maps to bypass, so an empty degrade would
		// silently escalate a workspace session to full bypass during a DB blip.
		b.warnOverrideDegrade(workspaceID, "fetch workspace permission_mode failed, degrading to bridge default", err)
		return defaultMode
	}
	// fetch succeeded: clear any prior degrade flag so a future regression warns again
	// (#749, mirrors resolveWorkspaceOverrides 成功路径的 Delete).
	b.warnedOverrides.Delete(workspaceID)
	if ws.PermissionMode != "" {
		return ws.PermissionMode // explicit workspace override wins
	}
	return defaultMode
}

// warnOverrideDegrade logs a degrading warning at most once per workspaceID per
// process lifetime, preventing log spam under high-crash session loops (#749).
// The warning is re-armed when the workspace later resolves successfully.
func (b *Bridge) warnOverrideDegrade(workspaceID, msg string, err error) {
	if _, loaded := b.warnedOverrides.LoadOrStore(workspaceID, struct{}{}); loaded {
		return
	}
	b.log.Warn("bridge: "+msg, "workspace_id", workspaceID, "err", err)
}

// injectAgentConfig loads agent config files and injects the unified system
// prompt into session info. A no-op when config dir is empty or agent config
// is not configured.
// injectExclude lists file base names to skip; when nil, falls back to the
// platform-level default from the atomic config map.
//
// workspaceOverrides selects the WebChat track (LoadForWorkspace) when non-nil;
// nil selects the Message Channel track (Load with botName). WebChat sessions
// don't select a bot, so botName is intentionally not passed to LoadForWorkspace.
//
// Parameter order note: botName (YAML config name, e.g. "my-bot") comes before
// botID (platform runtime ID, e.g. "U12345") because botName is the primary key
// for agent-config path resolution, while botID is only used for logging.
func (b *Bridge) injectAgentConfig(info *worker.SessionInfo, facts agentconfig.RuntimeFacts, platform, botName, botID string, injectExclude []string, workspaceOverrides map[string]string) {
	if b.agentConfigDir == "" {
		return
	}
	// botName is the YAML config name for agent-config path resolution.
	// When empty (single-bot mode / webchat / API), bot-level lookup is skipped
	// and resolution falls through to platform-level automatically.
	injectExclude = b.resolveInjectExclude(platform, injectExclude)
	if unknown := agentconfig.ValidateExcludeList(injectExclude); len(unknown) > 0 {
		b.log.Warn("bridge: inject_exclude contains unknown config files",
			"unknown", unknown, "valid", agentconfig.KnownFiles())
	}
	b.log.Debug("bridge: loading agent config", "dir", b.agentConfigDir, "platform", platform, "bot_name", botName, "bot_id", botID, "exclude", injectExclude, "workspace_overrides", workspaceOverrides != nil)
	var configs *agentconfig.AgentConfigs
	var err error
	if workspaceOverrides != nil {
		configs, err = agentconfig.LoadForWorkspace(b.agentConfigDir, platform, workspaceOverrides, injectExclude...)
	} else {
		configs, err = agentconfig.Load(b.agentConfigDir, platform, botName, injectExclude...)
	}
	if err != nil {
		if errors.Is(err, agentconfig.ErrInvalidBotName) {
			b.log.Error("bridge: agent config rejected",
				"dir", b.agentConfigDir, "platform", platform, "bot_name", botName, "bot_id", botID, "err", err)
		} else {
			b.log.Warn("bridge: agent config load failed",
				"dir", b.agentConfigDir, "platform", platform, "bot_name", botName, "bot_id", botID, "err", err)
		}
		return
	}
	if prompt := agentconfig.BuildSystemPromptWithRuntime(configs, facts); prompt != "" {
		info.SystemPrompt = prompt
		b.log.Info("bridge: agent config injected", "prompt_len", len(prompt), "platform", platform, "bot_name", botName, "bot_id", botID)
	} else {
		b.log.Debug("bridge: agent config loaded but prompt empty", "platform", platform, "bot_name", botName, "bot_id", botID)
	}
}

// recordCrashLoop tracks consecutive crashes per session. Returns true if the
// session has exceeded crashLoopMax crashes within crashLoopWindow, indicating
// a crash loop that should abort retries to protect gateway stability.
func (b *Bridge) recordCrashLoop(sessionID string) bool {
	b.crashTrackerMu.Lock()
	defer b.crashTrackerMu.Unlock()

	h, ok := b.crashTracker[sessionID]
	if !ok || time.Since(h.firstSeen) > crashLoopWindow {
		h = &crashHistory{firstSeen: time.Now()}
		b.crashTracker[sessionID] = h
	}
	h.count++

	return h.count > crashLoopMax
}

// resetCrashLoop clears the crash history for a session on successful completion.
func (b *Bridge) resetCrashLoop(sessionID string) {
	b.crashTrackerMu.Lock()
	defer b.crashTrackerMu.Unlock()
	delete(b.crashTracker, sessionID)
}
