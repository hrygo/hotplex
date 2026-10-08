package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/proc"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// waitKillTimeout is the maximum time to wait for Worker.Wait() to return
// after calling Kill(). If Wait() still hasn't returned, forwardEvents
// abandons the wait to prevent permanent goroutine blocking.
const waitKillTimeout = 5 * time.Second

// forwardContext carries per-session mutable state for the event forwarding loop.
// Ownership: exclusively owned by the single forwardEvents goroutine per session.
// No concurrent access — all fields are read/written from that goroutine only,
// except turnTimerFired and terminalSent, which use atomic.Bool for timer and
// terminal-fence callback safety.
type forwardContext struct {
	sessionID     string
	workerType    worker.WorkerType
	workerRunID   string
	sessPlatform  string
	sessOwner     string
	workDir       string
	ctx           context.Context
	startTime     time.Time
	turnStartTime time.Time
	firstEvent    bool
	doneReceived  bool
	// terminalSent fences duplicate Done/Error events from a worker stream and
	// lets the exit path suppress a second synthetic error. It is atomic because
	// the turn-timeout callback and the forwarder goroutine can race.
	terminalSent atomic.Bool
	turnText     strings.Builder
	// hasRealText tracks worker-produced assistant text independently from
	// synthetic fallback text appended to turnText for history.
	hasRealText    bool
	lastError      *events.ErrorData
	pendingError   *events.Envelope
	turnTimerFired atomic.Bool
	lifecycle      *workerRunLifecycle
	// flog carries trace_id from the forwardEvents OTel span. Helpers must
	// use fc.flog (not b.log) to keep log lines correlatable with the span.
	// Nil-safe via bridge.flogOf; tests build forwardContext without it.
	flog *slog.Logger
}

func (fc *forwardContext) claimTerminal() bool {
	if fc == nil || !fc.terminalSent.CompareAndSwap(false, true) {
		return false
	}
	if fc.lifecycle != nil {
		fc.lifecycle.terminalCommitted.Store(true)
	}
	return true
}

func (fc *forwardContext) reopenTerminal() {
	if fc == nil {
		return
	}
	fc.terminalSent.Store(false)
	if fc.lifecycle != nil {
		fc.lifecycle.terminalCommitted.Store(false)
	}
}

// flogOf returns fc.flog or b.log when fc is nil or fc.flog is unset.
func (b *Bridge) flogOf(fc *forwardContext) *slog.Logger {
	if fc != nil && fc.flog != nil {
		return fc.flog
	}
	return b.log
}

func (b *Bridge) populateForwardSessionContext(span trace.Span, fc *forwardContext, sessionID string, workerType worker.WorkerType) {
	if b.sm == nil || (b.collector == nil && !span.IsRecording()) {
		return
	}
	si, err := b.sm.Get(context.Background(), sessionID)
	if err != nil {
		return
	}
	fc.sessPlatform = si.Platform
	fc.sessOwner = si.OwnerID
	if fc.sessOwner == "" {
		fc.sessOwner = si.UserID
	}

	// Correlate the forward span by agent identity independently of event-store
	// collection. Tracing and event persistence are separate optional features.
	eff := si.EffectiveIdentity()
	spanAttrs := []attribute.KeyValue{
		attribute.String(agentspec.MetadataKeyAgentID, eff.AgentID),
		attribute.String(agentspec.MetadataKeyWorkerType, string(workerType)),
	}
	if eff.UserID != "" {
		spanAttrs = append(spanAttrs, attribute.String(agentspec.MetadataKeyUserID, eff.UserID))
	}
	if eff.WorkspaceID != "" {
		spanAttrs = append(spanAttrs, attribute.String(agentspec.MetadataKeyWorkspaceID, eff.WorkspaceID))
	}
	if eff.Platform != "" {
		spanAttrs = append(spanAttrs, attribute.String(agentspec.MetadataKeyPlatform, eff.Platform))
	}
	if eff.Anonymous {
		spanAttrs = append(spanAttrs, attribute.Bool("anonymous", true))
	}
	span.SetAttributes(spanAttrs...)
}

// forwardEvents proxies worker events to the hub with seq assignment.
// EVT-004: if msgStore is configured, it appends to the event log on done events.
// AEP-020: after the recv channel closes, calls Worker.Wait() to determine exit
// code and sets DoneData.Success accordingly (non-zero exit = crash = success=false).
// bgCtx returns a background context scoped to the gateway lifecycle.
// Falls back to context.Background() when shutdownCtx is nil (unit tests).
func (b *Bridge) bgCtx() context.Context {
	if b.shutdownCtx != nil {
		return b.shutdownCtx
	}
	return context.Background()
}

func (b *Bridge) forwardEvents(fb forwarderBinding, sessionID string, opts forwardOpts) {
	parent := opts.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, span := observability.Tracer().Start(parent, "bridge.forward_events")
	defer span.End()
	w := fb.worker
	workerType := w.Type()
	flog := b.log.With(observability.KeyTraceID, observability.TraceID(ctx))
	var fc *forwardContext
	defer func() {
		if r := recover(); r != nil {
			observability.GatewayForwarderPanics().Add(context.Background(), 1,
				metric.WithAttributes(attribute.String("worker_type", string(workerType))))
			flog.Error("bridge: panic in forwardEvents", "session_id", sessionID,
				"worker_type", workerType, "worker_run_id", opts.workerRunID,
				"panic", r, "stack", string(debug.Stack()))
			// A panic after stream consumption must still produce one visible
			// terminal. Without this guard the client is left indefinitely in a
			// thinking state; a later worker cleanup has no forwarder to notify it.
			if fc == nil {
				return
			}
			releaseEvent, admitted := fc.lifecycle.beginEvent()
			if !admitted {
				return
			}
			defer releaseEvent()
			if b.hub != nil && !w.IsStopped() && fc.claimTerminal() {
				b.sendError(sessionID, events.ErrCodeWorkerCrash, "worker event stream interrupted before terminal")
			}
		}
	}()

	flog.Debug("bridge: forwardEvents goroutine started", "session_id", sessionID,
		"worker_type", workerType, "resumed", opts.resumed,
		"has_frozen_conn", fb.conn != nil, "forwarder_reset_gen", fb.resetGen,
		"worker_run_id", opts.workerRunID)

	// reset generation was frozen at launch (Fix A). A stale forwarder whose
	// worker was since reset detects the mismatch in handleWorkerExit.
	myResetGen := fb.resetGen

	fc = &forwardContext{
		sessionID:     sessionID,
		workerType:    workerType,
		workerRunID:   opts.workerRunID,
		workDir:       opts.workDir,
		ctx:           ctx,
		startTime:     time.Now(),
		turnStartTime: time.Now(),
		firstEvent:    true,
		lifecycle:     fb.lifecycle,
		flog:          flog,
	}

	b.populateForwardSessionContext(span, fc, sessionID, workerType)

	acc := b.getOrInitAccum(sessionID, opts.workDir, fc.startTime)
	if acc.Generation.Load() == 0 && acc.genInitialized.CompareAndSwap(false, true) {
		gen := int64(1)
		if b.turnsQuerier != nil {
			// Use shutdownCtx as parent: these are short DB reads (<3s) to
			// restore turn counters. opts.ctx is scoped to the inbound request
			// and may expire on slow worker startups. shutdownCtx is cancelled
			// on gateway shutdown, ensuring DB queries don't block graceful stop.
			genCtx, genCancel := context.WithTimeout(b.bgCtx(), 3*time.Second)
			latest, _ := b.turnsQuerier.LatestGeneration(genCtx, sessionID)
			genCancel()
			if latest > 0 {
				gen = latest
			}
		}
		acc.Generation.Store(gen)
	}
	if acc.TurnCount.Load() == 0 && b.turnsQuerier != nil {
		// bgCtx() — same rationale as LatestGeneration above.
		tnCtx, tnCancel := context.WithTimeout(b.bgCtx(), 3*time.Second)
		tn, err := b.turnsQuerier.LatestTurnNum(tnCtx, sessionID, acc.Generation.Load())
		tnCancel()
		if err != nil {
			flog.Warn("turns: restore turn num", "err", err)
		}
		if tn > 0 {
			acc.TurnCount.Store(int32(tn))
		}
	}

	fc.lifecycle.setTurnTimeoutHandler(func(generation uint64) {
		releaseEvent, admitted := fc.lifecycle.beginEvent()
		if !admitted {
			return
		}
		defer releaseEvent()
		if !fc.lifecycle.turnTimeoutCurrent(generation) {
			return
		}
		if !fc.turnTimerFired.CompareAndSwap(false, true) {
			return
		}
		if !fc.claimTerminal() {
			return
		}
		fc.lifecycle.stopTurnTimeout()
		acc := b.getOrInitAccum(sessionID, fc.workDir, fc.startTime)
		acc.clearTurnStart()
		b.finishTurnTTFT(sessionID, "timeout")
		fc.turnText.Reset()
		fc.hasRealText = false
		flog.Warn("bridge: turn timeout exceeded, terminating worker",
			"session_id", sessionID, "worker_type", workerType, "turn_timeout", b.turnTimeout)
		if err := w.Terminate(context.Background()); err != nil {
			flog.Warn("bridge: terminate worker after turn timeout", "session_id", sessionID, "err", err)
		}
		b.finishTurnTimeout(sessionID, fc, syntheticTurnParams{
			SessionID:  sessionID,
			Reason:     "turn_timeout",
			Message:    fmt.Sprintf("Turn exceeded %v time limit", b.turnTimeout),
			Source:     eventstore.SourceTimeout,
			Platform:   fc.sessPlatform,
			Owner:      fc.sessOwner,
			Model:      acc.ModelName,
			Generation: acc.Generation.Load(),
			TurnNum:    int(acc.TurnCount.Load()),
		})
	})
	defer fc.lifecycle.clearTurnTimeoutHandler()

	// Event source: the frozen Conn captured at launch (Fix A). forwardEvents
	// MUST NOT call w.Conn() here — doing so would re-read the mutable field and
	// could bind a stale forwarder to a replacement Conn after /reset, splitting
	// the event stream. The nil fallback is purely defensive: real workers
	// always publish a Conn before launch, and the (synchronous) launch path
	// already captured the best value available at that instant.
	recvSource := fb.conn
	if recvSource == nil {
		flog.Error("bridge: frozen conn is nil at forwardEvents start, falling back to live Conn()",
			"session_id", sessionID, "worker_type", workerType)
		recvSource = w.Conn()
	}
	recvCh := recvSource.Recv()
	for env := range recvCh {
		b.processForwardedEvent(env, w, opts, fc)
	}
	// Wait() can release the live Worker.Conn. Read from the connection that
	// supplied this forwarder's events before that cleanup so crash recovery
	// re-delivers the correct turn input and never observes a replacement/reset
	// connection. In the defensive nil-frozen-Conn path this also preserves the
	// input from the live fallback that actually supplied the event stream.
	lastReplay := snapshotInputReplay(recvSource)
	fc.lifecycle.withEvent(func() {
		if !fc.doneReceived {
			b.finishTurnTTFT(sessionID, "worker_exit")
		}

		// Flush buffered error that never reached a retry decision point.
		if fc.pendingError != nil && fc.claimTerminal() {
			releaseSeq := func() {}
			canFlush := true
			if b.collector != nil {
				var ok bool
				releaseSeq, ok = b.hub.BeginSeqOperation(sessionID)
				if !ok {
					canFlush = false
				}
			}
			if canFlush {
				if err := b.hub.SendToSession(context.Background(), fc.pendingError); err != nil {
					flog.Warn("bridge: flush pending error on exit failed", "session_id", sessionID, "err", err)
				}
				b.captureEvent(sessionID, fc.pendingError.Seq, fc.pendingError.Event.Type, fc.pendingError.Event.Data, flog)
				releaseSeq()
			}
			fc.pendingError = nil
		}
	})
	fc.pendingError = nil

	b.handleWorkerExit(w, workerExitParams{
		sessionID:      sessionID,
		workerType:     workerType,
		opts:           opts,
		startTime:      fc.startTime,
		doneReceived:   fc.doneReceived,
		turnText:       fc.turnText.String(),
		turnTextLen:    fc.turnText.Len(),
		terminalSent:   fc.terminalSent.Load(),
		turnTimerFired: fc.turnTimerFired.Load(),
		sessPlatform:   fc.sessPlatform,
		sessOwner:      fc.sessOwner,
		resetGen:       myResetGen,
		lastInput:      lastReplay.Content,
		lastReplay:     lastReplay,
		lifecycle:      fc.lifecycle,
		flog:           flog,
	})
}

func snapshotLastInput(conn worker.SessionConn) string {
	return snapshotInputReplay(conn).Content
}

func snapshotInputReplay(conn worker.SessionConn) worker.InputReplay {
	if replayer, ok := conn.(worker.InputReplayRecoverer); ok {
		replay := replayer.LastInputReplay()
		replay.Content = sanitizeLastInput(replay.Content)
		if replay.Skill != nil {
			invocation := *replay.Skill
			replay.Skill = &invocation
		}
		return replay
	}
	ir, ok := conn.(worker.InputRecoverer)
	if !ok {
		return worker.InputReplay{}
	}
	return worker.InputReplay{Content: sanitizeLastInput(ir.LastInput())}
}

// processForwardedEvent handles a single worker event in the forwarding loop.
func (b *Bridge) processForwardedEvent(env *events.Envelope, w worker.Worker, opts forwardOpts, fc *forwardContext) {
	releaseEvent, ok := fc.lifecycle.beginEvent()
	if !ok {
		return
	}
	defer releaseEvent()

	sessionID := fc.sessionID
	workerType := fc.workerType

	// Refresh worker LastIO on activity (issue #815, spec Fix C). Without this,
	// a long turn (>ExecutionTimeout) is wrongly reaped by the zombie scanner
	// because LastIO stays frozen at the input time — codex only bumped LastIO
	// in Input()/startNewThread, not during the turn. Done (turn terminator) is
	// excluded. SetLastIO lives on base.BaseWorker (not the Worker interface),
	// hence the type assertion; all real workers embed BaseWorker and satisfy it.
	if env.Event.Type != events.Done {
		if lu, ok := w.(interface{ SetLastIO(time.Time) }); ok {
			lu.SetLastIO(time.Now())
		}
	}

	// Handle internal reset events from in-place-reset workers (OCS, ACP).
	if env.Event.Type == events.KindInternalReset {
		b.handleInternalReset(env, sessionID, fc)
		return
	}

	// Pin sequence generation for the complete durable forwarding operation,
	// including turn writes performed before SendToSession assigns a seq.
	releaseSeq := func() {}
	if b.collector != nil && b.hub != nil {
		var ok bool
		releaseSeq, ok = b.hub.BeginSeqOperation(sessionID)
		if !ok {
			return
		}
	}
	defer releaseSeq()

	env = events.Clone(env)
	env.SessionID = sessionID
	env.OwnerID = fc.sessOwner
	// When eventstore is enabled, Hub SeqGen is the sole authority for seq
	// allocation. Worker-local counters (ACP/Codex mappers) restart from 1 on
	// every Worker launch and collide with persisted events on resume (issue
	// #879 regression). Always override regardless of what the Worker set.
	// When eventstore is disabled, only assign if the Worker left seq=0 (OCS
	// pattern); a Worker-provided seq is used as-is to preserve ordering in
	// non-durable deployments.
	//
	// Durable forwarding pre-allocates the seq (needed for eventstore capture)
	// and holds the per-session publish-order lock across allocation → capture
	// → enqueue, so a concurrent handler-allocated event (e.g. a stop's
	// synthetic done) cannot interleave and deliver out-of-order seqs to the
	// client (contract test C04-double-stop). Without the store, seq=0 events
	// are allocated inside SendToSession under the same lock.
	var releaseOrder func()
	if b.collector != nil {
		mu := b.hub.SeqOrderLock(sessionID)
		mu.Lock()
		releaseOrder = mu.Unlock
	}
	defer func() {
		if releaseOrder != nil {
			releaseOrder()
		}
	}()

	// A Done is the one worker event whose own handling enqueues another
	// client-visible event — the runtime execution fact — BEFORE the Done
	// itself reaches the wire. Stamping it here would hand it a LOWER seq than
	// the event that overtakes it, so a client enforcing monotonic seq drops
	// the terminal Done and the turn never finishes. Its seq is therefore
	// assigned at enqueue time instead, under this same held lease.
	deferDoneSeq := b.collector != nil && env.Event.Type == events.Done
	if b.collector != nil && !deferDoneSeq {
		env.Seq = b.hub.NextSeqHeld(sessionID)
	}

	// Buffer error events for potential LLM retry.
	if env.Event.Type == events.Error {
		b.flogOf(fc).Warn("bridge: received error from worker", "session_id", sessionID, "worker_type", workerType, "data", env.Event.Data)
		if ed, ok := env.Event.Data.(events.ErrorData); ok {
			fc.lastError = &ed
		}
		// When LLM retry is active, buffer the error and suppress forwarding.
		// The error is flushed only if retry decides NOT to retry (after Done).
		if b.retryCtrl != nil {
			cloned := events.Clone(env)
			cloned.SessionID = sessionID
			fc.pendingError = cloned
			return
		}
	}
	if fc.terminalSent.Load() && !fc.turnTimerFired.Load() && isTurnStartEvent(env.Event.Type) {
		// Some workers begin the next turn with content rather than an explicit
		// state(running) event. A post-terminal content event is therefore the
		// next turn boundary; reopen the terminal fence before processing it.
		fc.doneReceived = false
		fc.reopenTerminal()
	}

	// A worker stream may emit a duplicate Done/Error (or emit Done after a
	// stop has already produced the synthetic terminal). Claim the terminal
	// before any stats/runtime side effects so exactly one terminal is visible.
	// Retryable errors return above as pending and claim only when flushed.
	if env.Event.Type == events.Done || env.Event.Type == events.Error {
		if w.IsStopped() || !fc.claimTerminal() {
			return
		}
	}

	if fc.firstEvent {
		// Safety-net: persist again on first event in case the post-launch
		// persist was incomplete. Scenarios: (1) GetWorkerSessionID() had not
		// returned a value at launch time; (2) guard re-persist in transitionState
		// failed; (3) gateway crashed between launch and first event.
		b.persistWorkerSessionIDEnsure(opts.ctx, w, sessionID)
		fc.turnStartTime = time.Now()
		fc.firstEvent = false
	}

	// New turn detection: state(running) marks the beginning of a new turn.
	// Reset doneReceived so crash detection applies to this turn (prevents
	// stale true from a previous turn masking a crash in the current turn).
	if env.Event.Type == events.State {
		fc.doneReceived = false
		if stateData, ok := env.Event.Data.(events.StateData); ok && stateData.State == events.StateRunning {
			fc.turnStartTime = time.Now()
			fc.reopenTerminal()
		} else if m, ok := env.Event.Data.(map[string]any); ok && m["state"] == string(events.StateRunning) {
			fc.turnStartTime = time.Now()
			fc.reopenTerminal()
		}
	}

	if fc.turnTimerFired.Load() {
		return
	}

	// Permission dedup: suppress repeated cards for a recently denied
	// owner+fingerprint. On hit, deliver a local denial to the worker instead
	// of forwarding the request to the client. See Permission-Deny-Dedup-Spec.
	if env.Event.Type == events.PermissionRequest {
		if b.suppressPermissionRequest(opts.ctx, env, w) {
			return
		}
		if prd, ok := env.Event.Data.(events.PermissionRequestData); ok {
			b.emitPermissionRequestAudit(fc, &prd)
		} else if prdPtr, ok := env.Event.Data.(*events.PermissionRequestData); ok && prdPtr != nil {
			b.emitPermissionRequestAudit(fc, prdPtr)
		}
	}

	deltaContent, reasoningContent := b.extractTurnContent(env, fc)
	if env.Event.Type == events.Reasoning && reasoningContent != "" {
		b.recordTurnFirstOutput(sessionID, false)
	}
	if (env.Event.Type == events.MessageDelta || env.Event.Type == events.Message) && extractMessageContent(env) != "" {
		b.recordTurnFirstOutput(sessionID, true)
	}

	// Stats accumulation.
	b.accumulateStats(env, w, opts, fc)

	terminalStatus := ""
	doneData, isDone := asDoneData(env.Event.Data)
	shouldRetry := false
	retryAttempt := 0
	deferRuntimeFinish := env.Event.Type == events.Done && isDone && !doneData.Success && fc.pendingError != nil
	// The durable terminal write happens before the Done is sent (so the
	// client is never told "completed" while the record is non-terminal),
	// but the terminal runtime FACT is delivered after the Done (AEP order:
	// done → runtime.execution.{completed,failed}). D01.
	var runtimeFact *pendingRuntimeFact
	// Done processing: mark received.
	if env.Event.Type == events.Done {
		fc.doneReceived = true
		b.resetCrashLoop(sessionID)
		b.maybeTransitionIdleAfterDone(sessionID, fc)
		if !deferRuntimeFinish {
			runtimeFact = b.finishRuntimeOnDone(sessionID, fc, env)
		}
		terminalStatus = "completed"
		if isDone && !doneData.Success {
			terminalStatus = "failed"
		}
		if isDone && !doneData.Success && b.retryCtrl != nil && (!opts.resumed || fc.hasRealText) {
			shouldRetry, retryAttempt = b.retryCtrl.ShouldRetry(context.TODO(), sessionID, fc.lastError)
		}
	}

	if shouldRetry {
		if deferRuntimeFinish {
			// A retry continues the same accepted input with a fresh worker
			// attempt: the durable runtime finish belongs to the attempt that
			// finally terminates, not to this superseded Done. Persisting
			// here would mark the execution terminal while the retry is
			// still running, so no fact is produced either.
			_ = b.finishRuntimeOnDone(sessionID, fc, env)
		}
		fc.pendingError = nil
		fc.reopenTerminal()
		// Pre-register cancel channel before launching goroutine to close
		// the race window where CancelRetry can't find the channel.
		cancelCh := make(chan struct{})
		b.retryCancelMu.Lock()
		b.retryCancel[sessionID] = cancelCh
		b.retryCancelMu.Unlock()
		// Run autoRetry asynchronously so forwardEvents continues reading
		// from recvCh. This prevents the goroutine from blocking during
		// the backoff period — if the worker crashes, the for-range loop
		// detects recvCh closure immediately instead of waiting for the
		// backoff timer to expire. The goroutine uses shutdownCtx so it
		// cancels promptly during bridge shutdown.
		go b.autoRetry(b.shutdownCtx, w, sessionID, retryAttempt, cancelCh, fc.lifecycle)
		fc.turnText.Reset()
		fc.hasRealText = false
		if b.collector != nil {
			b.collector.ResetSession(sessionID)
		}
		fc.lastError = nil
		return // continue — retry produces new events on recv
	}

	if env.Event.Type == events.Done || env.Event.Type == events.Error {
		fc.lifecycle.stopTurnTimeout()
	}

	if env.Event.Type == events.Done && isDone && !doneData.Success && fc.pendingError != nil {
		// A failed turn with a worker Error has one user-visible terminal: the
		// Error. The Done already owns the bridge fence, so sending it first
		// would otherwise make flushPendingError discard the real error.
		b.sendPendingError(fc)
		// The failed Done is never sent to the client (the Error is the one
		// user-visible terminal), but it is still persisted, so it needs its seq
		// before capture — allocated here, after finishRuntimeOnDone had its turn.
		if deferDoneSeq {
			env.Seq = b.hub.NextSeqHeld(sessionID)
		}
		b.captureForwardedEvent(env, deltaContent, reasoningContent, fc)
		// The Error is the user-visible terminal, so the failed runtime fact
		// follows it — never the hidden Done. finishRuntimeOnDone persists
		// the failed runtime and releases the gate; the fact is emitted
		// after the Error reaches the wire.
		runtimeFact = b.finishRuntimeOnDone(sessionID, fc, env)
		b.emitRuntimeFactOnDone(sessionID, runtimeFact)
		b.finishTurnTTFT(sessionID, terminalStatus)
		fc.turnText.Reset()
		fc.hasRealText = false
		fc.lastError = nil
		return
	}

	// Allocate a deferred Done seq immediately before enqueueing, so the seq a
	// client observes always increases in the order frames reach it.
	if deferDoneSeq {
		env.Seq = b.hub.NextSeqHeld(sessionID)
	}
	if err := b.hub.SendToSession(context.Background(), env); err != nil {
		b.flogOf(fc).Warn("bridge: forward event failed", "err", err, "session_id", sessionID, "worker_type", workerType, "event_type", env.Event.Type)
	}

	b.captureForwardedEvent(env, deltaContent, reasoningContent, fc)

	// Flush buffered error on non-Done events.
	b.flushPendingError(fc, true)

	if env.Event.Type == events.Done && b.retryCtrl != nil && (!isDone || doneData.Success) {
		fc.pendingError = nil
		b.retryCtrl.RecordSuccess(sessionID)
		fc.lastError = nil
	}

	if env.Event.Type == events.Done {
		// A retry continues the same accepted input, so only finish TTFT after
		// the retry decision confirms this is the terminal worker attempt.
		b.finishTurnTTFT(sessionID, terminalStatus)
		fc.turnText.Reset()
		fc.hasRealText = false
		// Do NOT reset fc.turnStartTime here. The previous code did
		// `fc.turnStartTime = time.Now()` at Done, which started the NEXT turn's
		// clock at this Done — so inter-turn idle was billed to the next turn.
		// Turn start is now recorded by the input path (Fix D) and read on Done.
		// Deliver the persisted terminal runtime fact AFTER the Done, per
		// the AEP contract (done → runtime.execution.{completed,failed}).
		b.emitRuntimeFactOnDone(sessionID, runtimeFact)
	}

}

func isTurnStartEvent(kind events.Kind) bool {
	switch kind {
	case events.Message, events.MessageStart, events.MessageDelta, events.Reasoning,
		events.ToolCall, events.PermissionRequest, events.QuestionRequest,
		events.ElicitationRequest:
		return true
	default:
		return false
	}
}

// finishRuntimeOnDone correlates the terminal runtime status when a Done event
// arrives. It queries the active execution for the session, persists the
// terminal state with FinishRuntime, and releases the active gate (replay +
// dispatch of the queued durable head). It does NOT emit the terminal runtime
// fact: AEP requires runtime.execution.{completed,failed} AFTER done
// (docs/reference/aep-protocol.md), so the fact is emitted by
// emitRuntimeFactOnDone after the Done itself is on the wire (D01). The
// durable write still happens first, so a client is never told the execution
// completed while the record is in a non-terminal state. When the durable
// write fails, nothing is emitted and the repairer retries; the pending fact
// is re-emitted by HandleRepairSuccess. Spec 2026-07-14 lines 259-275.
func (b *Bridge) finishRuntimeOnDone(sessionID string, fc *forwardContext, env *events.Envelope) *pendingRuntimeFact {
	if b.executionStore == nil {
		return nil
	}

	// OpenBySession covers pending/running/unknown so a late Done can still
	// converge an execution that lease recovery already marked unknown.
	rec, err := b.executionStore.OpenBySession(context.Background(), sessionID)
	if err != nil {
		return nil
	}

	success := true
	if dd, ok := env.Event.Data.(events.DoneData); ok {
		success = dd.Success
	} else if m, ok := env.Event.Data.(map[string]any); ok {
		if s, _ := m["success"].(bool); !s {
			success = false
		}
	}

	var rtStatus execution.RuntimeStatus
	var eventKind events.Kind
	if success {
		rtStatus = execution.RuntimeCompleted
		eventKind = events.RuntimeExecutionCompleted
	} else {
		rtStatus = execution.RuntimeFailed
		eventKind = events.RuntimeExecutionFailed
	}

	// Correlate with the emitting forwarder's immutable attach run. A stale
	// forwarder must never finish the newest open execution for the session.
	if err := b.executionStore.FinishRuntime(context.Background(), rec.ExecutionID, fc.workerRunID, rtStatus, ""); err != nil {
		b.flogOf(fc).Debug("bridge: finish runtime on done", "err", err,
			"session_id", sessionID, observability.KeyExecutionID, rec.ExecutionID, "status", rtStatus)
		if errors.Is(err, execution.ErrRunMismatch) {
			return nil
		}
		if b.repairer != nil {
			b.repairer.Enqueue(execution.RepairIntent{
				ExecutionID: rec.ExecutionID,
				SessionID:   sessionID,
				WorkerRunID: fc.workerRunID,
				Kind:        execution.RepairRuntime,
				Status:      string(rtStatus),
			})
		}
		// Do not emit a terminal runtime event when the durable write failed:
		// the client must not be told the execution completed while the record
		// is still in a non-terminal state.
		return nil
	}

	// Replay buffered supplements now that the active gate is released. This
	// method runs UNDER processForwardedEvent's held seq lease (see comment at
	// the NextSeqHeld call below about self-deadlock if we re-enter
	// BeginSeqOperation), so the replay MUST run async — DeliverReplay →
	// deliverToWorker → acceptInputExecution re-enters the seq barrier in its
	// own context. cloneForReplay sets seq=0 so the hub reassigns it.
	b.replayPending(sessionID)
	// The active gate is released, so the session's durable queue may promote
	// its head. Both are async for the same seq-lease reason.
	b.dispatchQueued(context.Background(), sessionID)

	// A late Done proves the worker actually executed the input. If the
	// delivery was recorded as failed (e.g. a native skill invocation that
	// timed out client-side while the server kept processing), converge it to
	// unknown so the record does not claim failure while the runtime completed.
	if rec.Status == execution.StatusFailed && rtStatus == execution.RuntimeCompleted {
		if cerr := b.executionStore.ConvergeDeliveryFailed(context.Background(), rec.ExecutionID, fc.workerRunID); cerr != nil {
			b.flogOf(fc).Debug("bridge: converge failed delivery after late done", "err", cerr,
				"session_id", sessionID, observability.KeyExecutionID, rec.ExecutionID)
		}
	}

	observability.ExecutionRuntimeOutcome().Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("runtime_status", string(rtStatus))))
	if rec.StartedAt != nil {
		observability.ExecutionRuntimeDuration().Record(context.Background(),
			time.Since(time.UnixMilli(*rec.StartedAt)).Seconds())
	}

	return &pendingRuntimeFact{
		executionID: rec.ExecutionID,
		status:      string(rtStatus),
		kind:        eventKind,
		ownerID:     fc.sessOwner,
	}
}

// pendingRuntimeFact is the terminal runtime fact whose durable write has
// succeeded but whose client delivery is deferred until after the Done itself
// is on the wire (AEP: done → runtime.execution.{completed,failed}).
type pendingRuntimeFact struct {
	executionID string
	status      string
	kind        events.Kind
	ownerID     string
}

// emitRuntimeFactOnDone delivers a previously persisted terminal runtime fact.
// It runs after the Done envelope has been sent, so the client observes the
// contract order. The seq is allocated at enqueue time under the caller's
// held lease, keeping arrival order and seq order identical.
func (b *Bridge) emitRuntimeFactOnDone(sessionID string, fact *pendingRuntimeFact) {
	if fact == nil {
		return
	}
	var seq int64
	preallocated := b.collector != nil
	if preallocated {
		// processForwardedEvent already holds the session's sequence lease.
		// Re-entering BeginSeqOperation through SendToSession can self-deadlock
		// when a ReleaseSeq writer is queued behind the outer read lock.
		seq = b.hub.NextSeqHeld(sessionID)
	}
	if preallocated && seq == 0 {
		return
	}
	rtEnv := events.NewEnvelope(aep.NewID(), sessionID, seq, fact.kind, events.RuntimeExecutionData{
		ExecutionID: fact.executionID,
		Status:      fact.status,
	})
	rtEnv.OwnerID = fact.ownerID
	_ = b.hub.SendToSession(context.Background(), rtEnv)
}

// finishTurnTimeout closes the durable runtime execution before publishing its
// terminal events. A timeout is an ordinary failed execution: releasing the
// active gate lets the queued head proceed after the Worker has been stopped.
// The error and runtime fact are sent in sequence order; the synthetic Done is
// persisted for history but remains hidden from the client.
func (b *Bridge) finishTurnTimeout(sessionID string, fc *forwardContext, turn syntheticTurnParams) {
	if b.hub == nil {
		return
	}

	if b.collector != nil {
		releaseSeq, ok := b.hub.BeginSeqOperation(sessionID)
		if !ok {
			return
		}
		defer releaseSeq()
		order := b.hub.SeqOrderLock(sessionID)
		order.Lock()
		defer order.Unlock()
	}

	done := events.NewEnvelope(aep.NewID(), sessionID, 0, events.Done, events.DoneData{
		Success: false,
		Reason:  turn.Reason,
	})
	done.OwnerID = fc.sessOwner
	runtimeFact := b.finishRuntimeOnDone(sessionID, fc, done)

	errEnv := events.NewEnvelope(aep.NewID(), sessionID, 0, events.Error, events.ErrorData{
		Code:    events.ErrCodeTurnTimeout,
		Message: fmt.Sprintf("Turn exceeded %v time limit and was terminated.", b.turnTimeout),
	})
	errEnv.OwnerID = fc.sessOwner
	if b.collector != nil {
		errEnv.Seq = b.hub.NextSeqHeld(sessionID)
	}
	if err := b.hub.SendToSession(context.Background(), errEnv); err != nil {
		b.flogOf(fc).Warn("bridge: send turn timeout error", "session_id", sessionID, "err", err)
	}
	b.captureEvent(sessionID, errEnv.Seq, errEnv.Event.Type, errEnv.Event.Data, b.flogOf(fc))

	b.captureSyntheticEventHeld(turn)
	b.emitRuntimeFactOnDone(sessionID, runtimeFact)
}

// extractTurnContent extracts message/reasoning content for turn tracking.
func (b *Bridge) extractTurnContent(env *events.Envelope, fc *forwardContext) (deltaContent, reasoningContent string) {
	switch env.Event.Type {
	case events.MessageDelta, events.Message:
		if content := extractMessageContent(env); content != "" {
			fc.turnText.WriteString(content)
			fc.hasRealText = true
			if env.Event.Type == events.MessageDelta {
				deltaContent = content
			}
		}
	case events.Reasoning:
		reasoningContent = extractReasoningContent(env)
	}
	return
}

func (b *Bridge) handleInternalReset(env *events.Envelope, sessionID string, fc *forwardContext) {
	var data events.InternalResetData
	switch d := env.Event.Data.(type) {
	case events.InternalResetData:
		data = d
	case map[string]any:
		gen, exists := d["generation"]
		if !exists {
			return
		}
		switch v := gen.(type) {
		case int64:
			data = events.InternalResetData{Generation: v}
		case float64:
			data = events.InternalResetData{Generation: int64(v)}
		case json.Number:
			if n, err := v.Int64(); err == nil {
				data = events.InternalResetData{Generation: n}
			} else {
				return
			}
		default:
			return
		}
	default:
		return
	}
	acc := b.getOrInitAccum(sessionID, fc.workDir, fc.startTime)
	if acc.AppliedResetGen.Load() < data.Generation {
		acc.Generation.Add(1)
		acc.AppliedResetGen.Store(data.Generation)
	}
	acc.TurnCount.Store(0)
	fc.turnText.Reset()
	fc.hasRealText = false
}

// accumulateStats tracks tool calls and per-turn stats on done events.
func (b *Bridge) accumulateStats(env *events.Envelope, w worker.Worker, opts forwardOpts, fc *forwardContext) {
	sessionID := fc.sessionID

	switch env.Event.Type {
	case events.ToolCall:
		acc := b.getOrInitAccum(sessionID, fc.workDir, fc.startTime)
		acc.ToolCallCount.Add(1)
		if tc, ok := asToolCallData(env.Event.Data); ok {
			if acc.ToolNames == nil {
				acc.ToolNames = make(map[string]int)
			}
			acc.ToolNames[tc.Name]++
			// tool.call audit (issue #833 P2, spec §5.2). Emit after stats
			// accumulation so a slow/nil collector never blocks forwarding.
			// Outcome is success at this point — tool failure correlation via
			// the later ToolResult event is deferred to P3 (spec §5.2 table
			// lists failure as a possible outcome, but matching result→call by
			// ID at emit-time is speculative and error-prone without buffering).
			b.emitToolCallAudit(fc, &tc)
		}
	case events.Done:
		acc := b.getOrInitAccum(sessionID, opts.workDir, fc.startTime)
		success := false
		if dd, ok := asDoneData(env.Event.Data); ok {
			acc.mergePerTurnStats(dd)
			success = dd.Success
		}
		acc.TurnCount.Add(1)
		// Turn duration: prefer the input-path start stamp so the timer covers
		// only this turn's processing and excludes inter-turn idle (Fix D). On
		// miss (crash recovery / replay with no recorded start), fall back to
		// the first worker event time and leave a debug breadcrumb.
		startMs := b.consumeTurnStartMs(sessionID)
		if startMs > 0 {
			acc.TurnDurationMs = time.Now().UnixMilli() - startMs
		} else {
			acc.TurnDurationMs = time.Since(fc.turnStartTime).Milliseconds()
			b.flogOf(fc).Debug("bridge: turn start stamp missing on done, using first-event fallback",
				"session_id", sessionID, "worker_type", fc.workerType, "fallback_since", time.Since(fc.turnStartTime).Round(time.Millisecond))
		}
		acc.computePerTurnDeltas()

		if cr, ok := w.(worker.ControlRequester); ok {
			fetchContextUsage(cr, acc)
		}

		b.injectSessionStats(env, acc)
		b.maybeSendDoneFallback(sessionID, acc, fc, success)
		b.captureAssistantTurn(sessionID, env.Seq, acc, fc.turnText.String(),
			fc.sessOwner, fc.sessPlatform, env.Timestamp)
		acc.resetPerTurn()
		if b.flogOf(fc).Enabled(context.Background(), slog.LevelDebug) {
			b.flogOf(fc).Debug("bridge: turn completed",
				"session_id", sessionID, "worker_type", fc.workerType, "turn", acc.TurnCount.Load(),
				"duration", time.Since(fc.turnStartTime).Round(time.Millisecond),
				"text_len", fc.turnText.Len(), "tools", acc.ToolCallCount.Load())
		}
	}
}

// maybeSendDoneFallback classifies a successful turn's user-visible result and
// synthesizes a Message event when the worker produced no assistant text. Three
// states (Turn-Integrity spec Fix C, invariant I-5/I-9):
//  1. assistant text > 0 → real content delivered; no fallback.
//  2. text == 0 && tool_count > 0 → legitimate tool-only turn; emit one
//     "✅ 已完成 · 🔧 …" fallback on feishu/slack (webchat renders tools itself).
//  3. text == 0 && tool_count == 0 → empty-success integrity failure; emit an
//     explicit retryable terminal on ALL platforms (incl. webchat) so no card
//     is left showing a placeholder and no session ends silently.
//
// Classification uses THIS turn's real delivery ledger only — never placeholder,
// status copy, the previous turn, or a wrong forwarder's partial state.
//
// Must run BEFORE captureAssistantTurn: it backfills fc.turnText with the
// fallback text so the turns table records it (history/replay shows the
// fallback instead of an empty assistant turn). Also persists to the events
// table via captureEvent so WS event replay surfaces it too.
func (b *Bridge) maybeSendDoneFallback(sessionID string, acc *sessionAccumulator, fc *forwardContext, success bool) {
	if !success {
		return
	}
	if fc.turnText.Len() > 0 {
		return // state 1: real assistant content
	}
	toolCount := acc.ToolCallCount.Load()
	if toolCount == 0 {
		// state 3: empty-success integrity failure. Emit a retryable terminal
		// on every platform. WebChat is NOT skipped here — an empty assistant
		// turn leaves a blank reply, which this message replaces.
		b.emitDoneFallbackMessage(sessionID, fc, messaging.FormatEmptySuccess(), true)
		if b.collector != nil {
			observability.WorkerEmptySuccess().Add(context.Background(), 1,
				metric.WithAttributes(attribute.String("worker_type", string(fc.workerType))),
				metric.WithAttributes(attribute.String("platform", fc.sessPlatform)))
		}
		return
	}
	// state 2: tool-only turn. WebChat renders the tool list independently.
	if fc.sessPlatform == platformWebChat {
		return
	}
	d := messaging.TurnSummaryData{
		ToolCallCount:  int(toolCount),
		ToolNames:      acc.ToolNames,
		TurnDurationMs: acc.TurnDurationMs,
	}
	b.emitDoneFallbackMessage(sessionID, fc, messaging.FormatDoneFallback(d), false)
}

// emitDoneFallbackMessage sends a synthesized user-facing Message, backfills
// fc.turnText so the turns table records it, and persists it for replay.
// emptySuccess flags the message for the empty-success metric path.
func (b *Bridge) emitDoneFallbackMessage(sessionID string, fc *forwardContext, text string, emptySuccess bool) {
	fc.turnText.WriteString(text)
	messageData := events.MessageData{Content: text}
	var seq int64
	if b.collector != nil && b.hub != nil {
		seq = b.nextSeqAndCaptureHeld(sessionID, events.Message, messageData, eventstore.SourceNormal, nil)
	} else {
		seq = b.nextSeqAndCapture(sessionID, events.Message, messageData, eventstore.SourceNormal, nil)
	}
	env := events.NewEnvelope(
		aep.NewID(), sessionID, seq,
		events.Message,
		messageData,
	)
	if err := b.hub.SendToSession(context.Background(), env); err != nil {
		tag := "done_fallback"
		if emptySuccess {
			tag = "empty_success"
		}
		b.flogOf(fc).Warn("bridge: done fallback send failed", "session_id", sessionID, "kind", tag, "err", err)
	}
}

// maybeTransitionIdleAfterDone transitions a messaging session to IDLE after
// the turn completes (Fix A, issue #815). Without this, messaging sessions
// remain RUNNING and are reaped by the 30m zombie ExecutionTimeout, forcing
// codex into a fresh-start that loses ephemeral-thread context.
//
// webchat is excluded: it transitions to IDLE on WS close (conn.go:146), and
// transitioning here would break Fast Reconnect (which requires State==Running
// per session.md). Empty platform (collector disabled / unidentified) is skipped
// conservatively to avoid IDLE-ing sessions we can't classify.
func (b *Bridge) maybeTransitionIdleAfterDone(sessionID string, fc *forwardContext) {
	if b.sm == nil || fc.sessPlatform == "" || fc.sessPlatform == platformWebChat {
		return
	}
	if err := b.sm.Transition(context.Background(), sessionID, events.StateIdle); err != nil {
		b.flogOf(fc).Debug("bridge: post-done transition to idle rejected",
			"session_id", sessionID, "platform", fc.sessPlatform, "err", err)
	}
}

// captureForwardedEvent persists the event for replay.
func (b *Bridge) captureForwardedEvent(env *events.Envelope, deltaContent, reasoningContent string, fc *forwardContext) {
	sessionID := fc.sessionID
	if deltaContent != "" && b.collector != nil {
		b.collector.CaptureDeltaString(sessionID, env.Seq, deltaContent)
	} else if reasoningContent != "" && b.collector != nil {
		b.collector.CaptureReasoningString(sessionID, env.Seq, reasoningContent)
	} else if env.Event.Type != events.MessageDelta && env.Event.Type != events.Reasoning {
		b.captureEvent(sessionID, env.Seq, env.Event.Type, env.Event.Data, b.flogOf(fc))
	}
}

// flushPendingError sends the buffered error event to the client.
// skipOnDone controls whether to suppress the flush when the current event is Done.
// Failed Done events with a buffered error are resolved explicitly before the
// Done can be sent, so this helper remains for non-terminal stream events.
func (b *Bridge) flushPendingError(fc *forwardContext, skipOnDone bool) {
	if fc.pendingError == nil {
		return
	}
	if skipOnDone && fc.doneReceived {
		return
	}
	if !fc.claimTerminal() {
		fc.pendingError = nil
		return
	}
	b.sendPendingError(fc)
}

// sendPendingError delivers a buffered worker error without claiming the
// terminal fence. Callers use it when another terminal event already claimed
// the fence but must remain hidden from the client.
func (b *Bridge) sendPendingError(fc *forwardContext) {
	if fc.pendingError == nil {
		return
	}
	if err := b.hub.SendToSession(context.Background(), fc.pendingError); err != nil {
		b.flogOf(fc).Warn("bridge: forward buffered error failed", "err", err, "session_id", fc.sessionID, "worker_type", fc.workerType)
	}
	b.captureEvent(fc.sessionID, fc.pendingError.Seq, fc.pendingError.Event.Type, fc.pendingError.Event.Data, b.flogOf(fc))
	fc.pendingError = nil
}

// workerExitParams carries the context needed by handleWorkerExit.
type workerExitParams struct {
	sessionID      string
	workerType     worker.WorkerType
	opts           forwardOpts
	startTime      time.Time
	doneReceived   bool
	terminalSent   bool
	turnText       string
	turnTextLen    int
	turnTimerFired bool
	sessPlatform   string
	sessOwner      string
	// resetGen is the resetGeneration captured when forwardEvents started.
	// If the current generation differs, another reset replaced this goroutine's
	// worker and we must NOT cleanupCrashedWorker (would detach the NEW worker).
	resetGen int64
	// flog carries trace_id from the originating forwardEvents span so exit-path
	// logs stay correlatable with the run that produced them.
	flog *slog.Logger
	// lastReplay was captured from the frozen forwarder connection before Wait()
	// releases the worker's mutable live connection.
	lastInput  string
	lastReplay worker.InputReplay
	lifecycle  *workerRunLifecycle
}

// rawExitCodeFields extracts the raw OS exit code from workers that implement
// worker.RawExitCoder, formatted as unsigned decimal and hex. Returns ok=false
// for workers whose Wait() already returns the raw code (no separate value to
// report). Used in crash diagnostics to distinguish a normalized exit code
// (OCS maps every crash to 1) from the underlying OS code (e.g. 0xC0000142).
func (b *Bridge) rawExitCodeFields(w worker.Worker) (dec, hex string, ok bool) {
	rc, isRaw := w.(worker.RawExitCoder)
	if !isRaw {
		return "", "", false
	}
	code, has := rc.RawExitCode()
	if !has {
		return "", "", false
	}
	dec, hex = proc.FormatExitCode(code)
	return dec, hex, true
}

// handleWorkerExit processes worker exit after the recv channel closes.
// It determines the exit code, attempts crash recovery, sends error events,
// and performs cleanup.
func (b *Bridge) handleWorkerExit(w worker.Worker, p workerExitParams) {
	workerType := p.workerType
	lg := p.flog
	if lg == nil {
		lg = b.log
	}

	// P0 guard: if the worker was reset (generation changed), a NEW forwardEvents
	// goroutine is already managing the replacement worker. This OLD goroutine must
	// exit silently — cleanupCrashedWorker would detach the NEW worker and delete
	// the accumulator, breaking the session.
	if rg, ok := w.(worker.ResetGenerationer); ok && rg.LoadResetGeneration() != p.resetGen {
		lg.Info("bridge: worker exit from stale forwardEvents after reset, skipping cleanup",
			"session_id", p.sessionID, "worker_type", workerType,
			"forwarder_reset_gen", p.resetGen, "current_reset_gen", rg.LoadResetGeneration(),
			"worker_run_id", p.opts.workerRunID)
		// Diagnostic (Turn-Integrity Fix E): a non-zero count means a stale
		// forwarder observed the replacement. With the frozen binding (Fix A)
		// this forwarder read only its own (closed) Conn, so it consumed no
		// replacement events — the metric tracks the exit, not a real split.
		observability.StaleForwarderEvents().Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("worker_type", string(workerType))))
		return
	}

	// AEP-020: Worker.Recv() closed — get exit code to determine crash vs normal exit.
	// Must match proc.DefaultGracePeriod (5s) so SIGTERM grace isn't cut short.
	waitTimeout := 5 * time.Second
	if b.closed.Load() {
		waitTimeout = 10 * time.Second
	}
	var exitCode int
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		defer func() {
			if r := recover(); r != nil {
				lg.Error("bridge: panic in waitWorker", "session_id", p.sessionID, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		exitCode, _ = w.Wait()
	}()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		lg.Warn("bridge: Wait() timed out, force-killing", "session_id", p.sessionID, "worker_type", workerType)
		_ = w.Kill()
		// Secondary timeout: if Wait() still doesn't return after Kill()
		// (e.g., Go runtime deadlock or zombie process), abandon the wait
		// instead of blocking forwardEvents forever.
		killTimeout := time.NewTimer(waitKillTimeout)
		defer killTimeout.Stop()
		select {
		case <-ch:
			// Wait completed after Kill.
		case <-killTimeout.C:
			lg.Warn("bridge: Wait() did not return after Kill(), abandoning",
				"session_id", p.sessionID, "worker_type", workerType)
		}
	}

	releaseExit, admitted := p.lifecycle.beginEvent()
	if !admitted {
		b.detachStoppedWorkerRun(p.sessionID, w, lg)
		return
	}
	defer releaseExit()

	wasStopped := w.IsStopped()

	// Crash recovery retry: attempt when the worker exited without completing
	// its turn (no "done" received). Skip during shutdown, SIGTERM (143),
	// Applies to both fresh and resumed sessions — Resume() gracefully falls back
	// to fresh Start() for workers that cannot preserve conversation history.
	fallbackAttempted := b.sm != nil && !b.closed.Load() && !p.doneReceived && !p.terminalSent && exitCode != 143 && exitCode != -1 && !wasStopped && p.opts.retryDepth < 2 && time.Since(p.startTime) < 15*time.Second
	if fallbackAttempted && p.turnTextLen == 0 && time.Since(p.startTime) < 5*time.Second {
		lg.Info("bridge: session files missing after resume, skipping retry",
			"session_id", p.sessionID, "worker_type", workerType, "exit_code", exitCode)
		p.opts.retryDepth = 1
	}
	if fallbackAttempted {
		lastReplay := p.lastReplay
		if lastReplay.Content == "" && lastReplay.Skill == nil {
			lastReplay = p.opts.lastReplay
		}
		acc := b.getOrInitAccum(p.sessionID, "", p.startTime)
		if b.attemptResumeFallback(fallbackParams{
			sessionID:     p.sessionID,
			workDir:       p.opts.workDir,
			exitCode:      exitCode,
			retryDepth:    p.opts.retryDepth,
			workerType:    workerType,
			lastInput:     lastReplay.Content,
			lastReplay:    lastReplay,
			crashedWorker: w,
			sessPlatform:  p.sessPlatform,
			sessOwner:     p.sessOwner,
			accGeneration: acc.Generation.Load(),
			accModelName:  acc.ModelName,
		}) {
			return
		}
	}

	if b.closed.Load() {
		b.cleanupCrashedWorker(p.sessionID, w)
		return
	}

	if b.sm != nil {
		si, smErr := b.sm.Get(context.Background(), p.sessionID)
		if smErr == nil && si.State == events.StateTerminated {
			lg.Debug("bridge: session already terminated, skipping error for handler-killed worker", "session_id", p.sessionID, "worker_type", workerType)
			if !fallbackAttempted {
				b.cleanupCrashedWorker(p.sessionID, w)
			}
			return
		}
		// P0 guard: if the session was deleted and re-created (same deterministic ID)
		// with a new worker, this stale forwardEvents goroutine must not produce side
		// effects (captureSyntheticEvent, sendError) that would allocate seq numbers
		// from the new session's seqGen, causing UNIQUE constraint violations on the
		// events table. The worker CAS check reliably detects re-creation because
		// DeletePhysical sets ms.worker = nil before the new session's AttachWorker.
		if currentWorker := b.sm.GetWorker(p.sessionID); currentWorker != w {
			lg.Debug("bridge: worker replaced, skipping stale forwarder cleanup",
				"session_id", p.sessionID, "worker_type", workerType)
			return
		}
	}

	// Suppress user-facing errors when:
	// 1. Session completed normally: "done" received with no pending turn text.
	// 2. Worker was intentionally terminated: SIGTERM (exit 143) is always
	//    bridge/handler/GC-initiated, never an unexpected crash.
	if p.terminalSent {
		lg.Debug("bridge: worker exit after terminal was already sent",
			"session_id", p.sessionID, "worker_type", workerType, "exit_code", exitCode)
	} else if wasStopped {
		lg.Info("bridge: worker exit clean (stopped by user)",
			"session_id", p.sessionID, "worker_type", workerType, "exit_code", exitCode)
	} else if p.doneReceived && p.turnTextLen == 0 {
		lg.Info("bridge: worker exit clean (done received, no pending output)",
			"session_id", p.sessionID, "worker_type", workerType, "exit_code", exitCode)
	} else if exitCode == 143 {
		lg.Info("bridge: worker exit intentional (SIGTERM)",
			"session_id", p.sessionID, "worker_type", workerType)
	} else if exitCode != 0 && exitCode != -1 {
		acc := b.getOrInitAccum(p.sessionID, "", p.startTime)
		rawDec, rawHex, hasRaw := b.rawExitCodeFields(w)
		crashAttrs := []any{
			"session_id", p.sessionID,
			"worker_type", workerType,
			"exit_code", exitCode,
			"duration", time.Since(p.startTime).Round(time.Millisecond),
			"turn_count", acc.TurnCount.Load(),
		}
		if hasRaw {
			crashAttrs = append(crashAttrs, "raw_exit_code", rawDec, "raw_exit_code_hex", rawHex)
		}
		lg.Warn("bridge: worker exited with non-zero code, sending crash error", crashAttrs...)

		metricAttrs := []attribute.KeyValue{
			attribute.String("worker_type", string(workerType)),
			attribute.String("exit_code", fmt.Sprintf("%d", exitCode)),
		}
		if hasRaw {
			metricAttrs = append(metricAttrs,
				attribute.String("raw_exit_code", rawDec))
		}
		observability.WorkerCrashes().Add(context.TODO(), 1, metric.WithAttributes(metricAttrs...))

		b.sendError(p.sessionID, events.ErrCodeWorkerCrash, "worker crashed (exit code %d)", exitCode)
		syntheticMsg := fmt.Sprintf("Worker crashed with exit code %d", exitCode)
		b.captureSyntheticEvent(syntheticTurnParams{
			SessionID:  p.sessionID,
			Reason:     "worker_crash",
			Message:    syntheticMsg,
			Source:     eventstore.SourceCrash,
			Platform:   p.sessPlatform,
			Owner:      p.sessOwner,
			Model:      acc.ModelName,
			Generation: acc.Generation.Load(),
			TurnNum:    int(acc.TurnCount.Load()),
		})
	} else if exitCode == -1 {
		b.sendError(p.sessionID, events.ErrCodeSessionTerminated, "worker terminated (killed)")
	} else if !p.doneReceived {
		lg.Debug("bridge: sending error for platform cleanup (no done received)", "session_id", p.sessionID, "worker_type", workerType)
		b.sendError(p.sessionID, events.ErrCodeWorkerCrash, "worker exited without sending done")
	}

	if !fallbackAttempted {
		if wasStopped {
			// User-initiated stop: detach the dead worker and transition to Idle
			// so the session stays alive for the next turn (not Terminated).
			b.detachStoppedWorkerRun(p.sessionID, w, lg)
		} else {
			b.cleanupCrashedWorker(p.sessionID, w)
		}
	}
}

func (b *Bridge) detachStoppedWorkerRun(sessionID string, w worker.Worker, lg *slog.Logger) {
	if b.sm == nil || !b.sm.DetachWorkerIf(sessionID, w) {
		return
	}
	if err := b.sm.Transition(context.Background(), sessionID, events.StateIdle); err != nil {
		lg.Debug("bridge: transition to idle after stop", "session_id", sessionID, "err", err)
	}
}

// captureEvent persists an outbound event for replay. flog is optional;
// when non-nil (forwardEvents path) it carries trace_id; otherwise b.log is used.
func (b *Bridge) captureEvent(sessionID string, seq int64, eventType events.Kind, data any, flog ...*slog.Logger) {
	b.captureDirected(sessionID, seq, eventType, data, "outbound", flog...)
}

// nextSeqAndCapture keeps central sequence allocation and durable capture in
// the collector's session barrier. The optional callback is for associated
// writes (for example, a synthetic turn) that must finish before release.
func (b *Bridge) nextSeqAndCapture(sessionID string, eventType events.Kind, data any, source string, afterCapture func(seq int64)) int64 {
	releaseSeq, ok := b.hub.BeginSeqOperation(sessionID)
	if !ok {
		return 0
	}
	defer releaseSeq()
	return b.nextSeqAndCaptureHeld(sessionID, eventType, data, source, afterCapture)
}

// nextSeqAndCaptureHeld is the allocation/capture half used when the caller
// already pins the Hub sequence barrier for the surrounding durable operation.
func (b *Bridge) nextSeqAndCaptureHeld(sessionID string, eventType events.Kind, data any, source string, afterCapture func(seq int64)) int64 {
	if b.collector == nil {
		return b.hub.NextSeqHeld(sessionID)
	}
	ed, err := json.Marshal(data)
	if err != nil {
		b.log.Warn("bridge: capture marshal failed", "session_id", sessionID, "type", eventType, "direction", "outbound", "err", err)
		return b.hub.NextSeqHeld(sessionID)
	}
	return b.collector.CaptureWithSeq(
		sessionID,
		func() int64 { return b.hub.NextSeqHeld(sessionID) },
		eventType,
		ed,
		"outbound",
		source,
		afterCapture,
	)
}

// CaptureInboundEvent persists an inbound event for replay only (no turn write).
// Used for interaction responses (permission/question/elicitation) which are not user turns.
func (b *Bridge) CaptureInboundEvent(sessionID string, seq int64, eventType events.Kind, data any) {
	if b.collector != nil && b.hub != nil {
		releaseSeq, ok := b.hub.BeginSeqOperation(sessionID)
		if !ok {
			return
		}
		defer releaseSeq()
	}
	b.captureDirected(sessionID, seq, eventType, data, "inbound")
}

// CaptureInbound persists an inbound (user→worker) event for replay.
// Also writes a user turn record when eventType is Input.
func (b *Bridge) CaptureInbound(ctx context.Context, sessionID string, seq int64, clientMessageID string, eventType events.Kind, data any, platform, owner string) {
	if b.collector != nil && b.hub != nil {
		releaseSeq, ok := b.hub.BeginSeqOperation(sessionID)
		if !ok {
			return
		}
		defer releaseSeq()
	}
	b.captureDirected(sessionID, seq, eventType, data, "inbound")

	// Write user turn record for Input events.
	if eventType == events.Input && b.collector != nil {
		acc := b.getOrInitAccum(sessionID, "", time.Now())
		// Synchronous Generation initialization to prevent race (#658):
		// forwardEvents initializes acc.Generation asynchronously, but CaptureInbound
		// may be called from the Handler goroutine before that init completes.
		// Without this guard, user turns get Generation=0 while assistant turns get Generation=1+,
		// making the first user turn invisible after page refresh.
		if acc.Generation.Load() == 0 && acc.genInitialized.CompareAndSwap(false, true) {
			gen := int64(1)
			if b.turnsQuerier != nil {
				// shutdownCtx — same rationale as forwardEvents LatestGeneration.
				genCtx, genCancel := context.WithTimeout(b.bgCtx(), 3*time.Second)
				latest, _ := b.turnsQuerier.LatestGeneration(genCtx, sessionID)
				genCancel()
				if latest > 0 {
					gen = latest
				}
			}
			acc.Generation.Store(gen)
		}
		content := extractInputContent(data)
		turn := &eventstore.TurnWriteRequest{
			SessionID:       sessionID,
			ClientMessageID: clientMessageID,
			Generation:      acc.Generation.Load(),
			TurnNum:         int(acc.TurnCount.Load()) + 1,
			Seq:             seq,
			Role:            eventstore.RoleUser,
			Content:         content,
			Platform:        platform,
			UserID:          owner,
			Source:          eventstore.SourceNormal,
			CreatedAt:       time.Now().UnixMilli(),
		}
		b.collector.CaptureTurn(turn)
	}
}

// captureDirected marshals event data and sends it to the collector with the given direction.
// flog is optional; when non-nil, capture diagnostics carry trace_id.
func (b *Bridge) captureDirected(sessionID string, seq int64, eventType events.Kind, data any, direction string, flog ...*slog.Logger) {
	lg := b.log
	if len(flog) > 0 && flog[0] != nil {
		lg = flog[0]
	}
	if b.collector == nil {
		if lg.Enabled(context.Background(), slog.LevelDebug) {
			lg.Debug("bridge: capture skipped, collector is nil", "session_id", sessionID, "type", eventType, "direction", direction)
		}
		return
	}
	ed, err := json.Marshal(data)
	if err != nil {
		lg.Warn("bridge: capture marshal failed", "session_id", sessionID, "type", eventType, "direction", direction, "err", err)
		return
	}
	if eventType == events.ToolResult {
		ed = truncateToolResultOutput(ed)
	}
	b.collector.Capture(sessionID, seq, eventType, ed, direction, eventstore.SourceNormal)
}

const maxToolResultOutputLen = 128

// truncateToolResultOutput truncates the output field in a tool_result JSON payload.
func truncateToolResultOutput(raw json.RawMessage) json.RawMessage {
	var v struct {
		ID     string `json:"id"`
		Output any    `json:"output"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	s, ok := v.Output.(string)
	if !ok || utf8.RuneCountInString(s) <= maxToolResultOutputLen {
		return raw
	}
	runes := []rune(s)
	v.Output = string(runes[:maxToolResultOutputLen])
	truncated, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return truncated
}

// syntheticTurnParams carries metadata for writing a synthetic turn (crash/timeout/fresh_start).
type syntheticTurnParams struct {
	SessionID  string
	Reason     string
	Message    string
	Source     string
	Platform   string
	Owner      string
	Model      string
	Generation int64
	TurnNum    int
}

// captureSyntheticEvent writes a synthetic done-like event for crash/timeout/fresh_start scenarios.
// Allocates a real seq number to avoid colliding with the AEP "unassigned" convention (seq=0).
func (b *Bridge) captureSyntheticEvent(p syntheticTurnParams) {
	if b.collector == nil {
		return
	}
	data, err := marshalSyntheticEvent(p)
	if err != nil {
		return
	}
	b.nextSeqAndCapture(
		p.SessionID,
		events.Done,
		data,
		p.Source,
		func(seq int64) {
			b.captureSyntheticTurn(p, seq)
		},
	)
}

// captureSyntheticEventHeld is the sequence-lease variant used when a caller
// already owns the Hub barrier and publish-order lock.
func (b *Bridge) captureSyntheticEventHeld(p syntheticTurnParams) {
	if b.collector == nil {
		return
	}
	data, err := marshalSyntheticEvent(p)
	if err != nil {
		return
	}
	b.nextSeqAndCaptureHeld(
		p.SessionID,
		events.Done,
		data,
		p.Source,
		func(seq int64) {
			b.captureSyntheticTurn(p, seq)
		},
	)
}

func marshalSyntheticEvent(p syntheticTurnParams) ([]byte, error) {
	return json.Marshal(map[string]any{
		"success":   false,
		"reason":    p.Reason,
		"message":   p.Message,
		"synthetic": true,
	})
}

func (b *Bridge) captureSyntheticTurn(p syntheticTurnParams, seq int64) {
	if b.collector == nil {
		return
	}
	sFalse := false
	b.collector.CaptureTurn(&eventstore.TurnWriteRequest{
		SessionID:  p.SessionID,
		Generation: p.Generation,
		TurnNum:    p.TurnNum,
		Seq:        seq,
		Role:       eventstore.RoleAssistant,
		Content:    p.Message,
		Platform:   p.Platform,
		UserID:     p.Owner,
		Model:      p.Model,
		Source:     p.Source,
		Success:    &sFalse,
		CreatedAt:  time.Now().UnixMilli(),
	})
}

// captureAssistantTurn writes an assistant turn record from the done event path.
func (b *Bridge) captureAssistantTurn(sessionID string, seq int64, acc *sessionAccumulator, content, owner, platform string, timestamp int64) {
	if b.collector == nil {
		return
	}
	var toolsJSON string
	if len(acc.ToolNames) > 0 {
		b, _ := json.Marshal(acc.ToolNames)
		toolsJSON = string(b)
	}
	tokensInput := max(acc.PerTurnInput-acc.PerTurnCacheWrite-acc.PerTurnCacheRead, 0)
	s := true // Normal completion path is always success.
	success := &s

	turn := &eventstore.TurnWriteRequest{
		SessionID:        sessionID,
		Generation:       acc.Generation.Load(),
		TurnNum:          int(acc.TurnCount.Load()),
		Seq:              seq,
		Role:             eventstore.RoleAssistant,
		Content:          content,
		Platform:         platform,
		UserID:           owner,
		Model:            acc.ModelName,
		Success:          success,
		Source:           eventstore.SourceNormal,
		ToolsJSON:        toolsJSON,
		ToolCount:        int(acc.ToolCallCount.Load()),
		TokensInput:      tokensInput,
		TokensCacheWrite: acc.PerTurnCacheWrite,
		TokensCacheRead:  acc.PerTurnCacheRead,
		TokensOut:        acc.PerTurnOutput,
		DurationMs:       acc.TurnDurationMs,
		CostUSD:          acc.PerTurnCost,
		CreatedAt:        timestamp,
	}
	b.collector.CaptureTurn(turn)
}

// extractInputContent extracts user input text from event data.
func extractInputContent(data any) string {
	switch d := data.(type) {
	case events.InputData:
		return d.Content
	case map[string]any:
		if c, ok := d["content"].(string); ok {
			return c
		}
	}
	return ""
}

// extractMessageContent extracts text content from a message or message_delta event.
func extractMessageContent(env *events.Envelope) string {
	switch env.Event.Type {
	case events.Message, events.MessageDelta:
		switch d := env.Event.Data.(type) {
		case events.MessageData:
			return d.Content
		case *events.MessageData:
			if d != nil {
				return d.Content
			}
		case events.MessageDeltaData:
			return d.Content
		case *events.MessageDeltaData:
			if d != nil {
				return d.Content
			}
		}
		if m, ok := env.Event.Data.(map[string]any); ok {
			if content, ok := m["content"].(string); ok {
				return content
			}
		}
	}
	return ""
}

// extractReasoningContent extracts text content from a reasoning event.
func extractReasoningContent(env *events.Envelope) string {
	if env.Event.Type != events.Reasoning {
		return ""
	}
	if d, ok := env.Event.Data.(events.ReasoningData); ok {
		return d.Content
	}
	if m, ok := env.Event.Data.(map[string]any); ok {
		if content, ok := m["content"].(string); ok {
			return content
		}
	}
	return ""
}

// getOrInitAccum returns the session accumulator, creating one if needed.
// gitBranchOf is called outside the lock to avoid blocking all sessions
// during the ~2s git subprocess. The branch is set on the accumulator
// after re-acquiring the lock.
func (b *Bridge) getOrInitAccum(sessionID, workDir string, startTime time.Time) *sessionAccumulator {
	// Fast path: check if accumulator exists under read lock.
	b.accumMu.RLock()
	acc, ok := b.accum[sessionID]
	needsWorkDir := ok && workDir != "" && acc.WorkDir == ""
	b.accumMu.RUnlock()
	if ok {
		if needsWorkDir {
			// Resolve git branch outside any lock (up to 2s subprocess).
			branch := gitBranchOf(workDir)
			b.accumMu.Lock()
			acc.WorkDir = workDir
			acc.GitBranch = branch
			b.accumMu.Unlock()
		}
		return acc
	}

	// Slow path: resolve git branch and session creation time before acquiring write lock.
	var branch string
	if workDir != "" {
		branch = gitBranchOf(workDir)
	}
	sessionCreated := startTime
	if b.sm != nil && flag.Lookup("test.v") == nil {
		// Best-effort: resolve session creation time for accurate duration_seconds.
		// Errors silently fall back to startTime.
		func() {
			defer func() { _ = recover() }()
			if si, err := b.sm.Get(context.Background(), sessionID); err == nil && !si.CreatedAt.IsZero() {
				sessionCreated = si.CreatedAt
			}
		}()
	}

	b.accumMu.Lock()
	// Lazily init the accum map for Bridges constructed without NewBridge
	// (handler-focused tests build &Bridge{} literals). Nil-map reads are safe,
	// only the write below would panic.
	if b.accum == nil {
		b.accum = make(map[string]*sessionAccumulator)
	}
	// Double-check after acquiring write lock.
	if acc, ok := b.accum[sessionID]; ok {
		if workDir != "" && acc.WorkDir == "" {
			acc.WorkDir = workDir
			acc.GitBranch = branch
		}
		b.accumMu.Unlock()
		return acc
	}
	acc = &sessionAccumulator{
		StartedAt: sessionCreated,
		WorkDir:   workDir,
		GitBranch: branch,
	}
	b.accum[sessionID] = acc
	b.accumMu.Unlock()
	return acc
}

// injectSessionStats merges the accumulator snapshot into DoneData.Stats["_session"].
// Handles both typed DoneData and map[string]any (from events.Clone JSON round-tripping).
func (b *Bridge) injectSessionStats(env *events.Envelope, acc *sessionAccumulator) {
	dd, ok := asDoneData(env.Event.Data)
	if !ok {
		return
	}
	if dd.Stats == nil {
		dd.Stats = make(map[string]any)
	}
	dd.Stats["_session"] = acc.snapshot()

	// Write back: preserve the original representation (map stays map, struct stays struct).
	switch env.Event.Data.(type) {
	case map[string]any:
		raw, _ := json.Marshal(dd)
		_ = json.Unmarshal(raw, &env.Event.Data)
	default:
		env.Event.Data = dd
	}
}

// gitBranchOf delegates to messaging.GitBranchOf for branch resolution.
func gitBranchOf(dir string) string { return messaging.GitBranchOf(dir) }

// fetchContextUsage queries the worker for precise context usage via control channel.
// Errors are silently ignored; the caller falls back to aggregated Done stats.
func fetchContextUsage(cr worker.ControlRequester, acc *sessionAccumulator) {
	ctrlCtx, ctrlCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctrlCancel()
	if resp, err := cr.SendControlRequest(ctrlCtx, "get_context_usage", nil); err == nil {
		if cu := events.MapContextUsageResponse(resp); cu.MaxTokens > 0 || cu.TotalTokens > 0 || cu.Model != "" {
			acc.mergeContextUsage(cu)
		}
	}
}
