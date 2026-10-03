package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/brain"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	defaultStopTeardownTimeout  = 8 * time.Second
	defaultStopForwarderTimeout = time.Second
)

// ErrResumeSequenceUnavailable indicates that a session sequence could not be
// hydrated from durable history, so resuming the Worker would be unsafe.
var ErrResumeSequenceUnavailable = errors.New("bridge: resume sequence unavailable")

// bridgeSM is the narrow subset of SessionManager that Bridge needs.
// Composed from canonical sub-interfaces defined in handler.go to avoid
// duplicate method declarations.
type bridgeSM interface {
	SessionReader
	SessionLifecycle
	SessionTransitioner
	SessionWorkerManager
	SessionExpirer
}

// Bridge connects the gateway to the session manager.
// It runs the read pump in a goroutine and proxies worker events to the hub.
type Bridge struct {
	log          *slog.Logger
	hub          *Hub
	sm           bridgeSM
	collector    *eventstore.Collector  // optional; nil means event storage disabled
	turnsQuerier eventstore.TurnQuerier // optional; for LatestGeneration on startup and history recovery for CodexCLI
	wf           WorkerFactory
	retryCtrl    *LLMRetryController

	fwdWg  sync.WaitGroup // tracks active forwardEvents goroutines
	closed atomic.Bool    // set during shutdown to skip crash detection
	// shutdownCtx is stored as a struct field to broadcast shutdown to async
	// autoRetry goroutines. This is an intentional exception to the "no ctx in
	// struct" convention — the alternative (passing ctx through every call site)
	// doesn't reach goroutines spawned from processForwardedEvent.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc // cancels shutdownCtx on Shutdown()
	retryCancelMu  sync.Mutex
	retryCancel    map[string]chan struct{} // sessionID → cancel channel

	agentConfigDir        string                   // agent config directory path; "" = disabled
	turnTimeout           time.Duration            // per-turn timeout; 0 = disabled
	stopTeardownTimeout   time.Duration            // provider stop + teardown server-side budget
	stopForwarderTimeout  time.Duration            // extra local forwarder settle budget
	workerEnv             []string                 // extra env vars from worker.environment config
	workerEnvBlocklist    []string                 // extra blocklist entries from worker.env_blocklist config
	cronEnv               []string                 // env vars injected only into cron platform sessions
	mcpConfigJSON         atomic.Value             // pre-serialized MCP config JSON string; "" = not configured
	defaultPermissionMode atomic.Value             // worker.PermissionMode* tier maintained via UpdateDefaultPermissionMode + hot-reload. Consumed by resolveWorkspacePermissionMode for workspaces with no explicit override (r3 #804); seeded "workspace" by Default().
	agentConfigExclude    atomic.Value             // map[string][]string: platform → inject_exclude (global default at "" key)
	wsStore               WorkspaceOverridesReader // per-workspace agent-config overrides resolver (spec ②); nil = Message Channel track
	warnedOverrides       sync.Map                 // workspaceID → struct{}: dedup override-degrade warnings (#749)

	accum map[string]*sessionAccumulator // per-session stats accumulator

	// compressCache stores async compression results keyed by sessionID.
	// Entries are invalidated when the latest turn's CreatedAt changes
	// (turns are append-only, so timestamp stability implies content stability).
	compressCache sync.Map // sessionID → *compressCacheEntry
	// accumMu protects accum. RWMutex allows concurrent reads in getOrInitAccum
	// fast path; write lock is used for create/delete/reset operations.
	accumMu sync.RWMutex

	// pending buffers user supplements that arrived while a session was busy,
	// for workers that lack mid-turn support (acp/ocs fallback). PendingBuffer
	// has its own internal mu, so no separate guard here. Initialized in
	// NewBridge; nil-safe proxy methods (BufferPending/ClearPending/...).
	pending        *PendingBuffer
	replayer       PendingReplayer // late-injected (Bridge built before Handler)
	replayMu       sync.Mutex
	replayWG       sync.WaitGroup
	replayClosed   bool
	supplementGate sync.RWMutex
	replayFences   [64]sync.RWMutex // fixed stripes; serialize replay/supplement with reset/terminate

	crashTracker   map[string]*crashHistory // per-session crash loop detection
	crashTrackerMu sync.Mutex

	// auditCollector emits tool.call audit events (issue #833 P2, spec §5.2).
	// Optional: nil means tool-call audit is disabled (mirrors the pattern on
	// Handler/Hub/Conn). Injected via SetAuditCollector during gateway init.
	auditCollector *audit.Collector

	// dedup suppresses repeated permission cards after a user denial (Permission-
	// Deny-Dedup-Spec). Nil when the feature is disabled. Methods are nil-safe,
	// so call sites can invoke b.dedup.* unconditionally.
	dedup          *PermissionDenyDedup
	executionStore execution.Store     // durable ingress runtime correlation; nil = disabled
	repairer       *execution.Repairer // terminal-state repair retry; nil-safe
	workerRuns     sync.Map            // sessionID -> workerRunBinding; updated on each successful attach
	turnTTFT       *turnTTFTTracker

	// cfgProvider returns the live config snapshot for runtime-plan resolution
	// (#946 D2). Nil means plan resolution runs with no config snapshot, which
	// the plan's coverage flags record honestly rather than treating as a
	// complete plan. Injected because the Bridge is built before the config
	// store hot-reload machinery is wired.
	cfgProvider func() *config.Config

	// sharedRuntime refuses an authoritative launch whose process-scoped
	// profile conflicts with the one a shared-process Worker is already
	// running under (#946 D3). Nil-safe via the zero value.
	sharedRuntime *sharedRuntimeGuard

	// catalogInvalidate is invoked on every Worker attach so the session's
	// command catalog is refreshed (spec §5.2, §8.7). Late-injected via
	// SetCatalogInvalidator because the Handler (which owns the catalog
	// generation map) is built after the Bridge. Nil disables invalidation.
	catalogInvalidate func(sessionID string)
	// replayValidate is injected by Handler so session-aware crash replay can
	// revalidate structured native invocations before they reach the Worker.
	replayValidate func(context.Context, string, worker.Worker, worker.InputReplay) (worker.InputReplay, error)
}

type workerRunBinding struct {
	worker    worker.Worker
	id        string
	lifecycle *workerRunLifecycle
	// launchPlan is the plan this run was actually launched under (#946 D2).
	// It is captured at bind time and never recomputed, so a diagnostic can
	// report the run's historical plan instead of re-resolving against the
	// current config and passing the result off as what actually ran.
	//
	// It is a POINTER on purpose: workerRunBinding values are stored in a
	// sync.Map and removed with CompareAndDelete, which compares the whole
	// struct. An embedded launchPlan would carry slices and make the binding
	// uncomparable, so every run teardown would panic at runtime. The pointer
	// also avoids copying the plan on each store.
	launchPlan *launchPlan
}

// workerRunLifecycle is private to one local Worker wrapper/process run. It
// never survives reset or replacement, even when the provider session identity
// is resumed by the next Worker.
type workerRunLifecycle struct {
	eventMu           sync.RWMutex
	eventAdmission    chan struct{}
	stopping          atomic.Bool
	terminalCommitted atomic.Bool
	done              chan struct{}
	conn              worker.SessionConn
}

func newWorkerRunLifecycle(conn worker.SessionConn) *workerRunLifecycle {
	lifecycle := &workerRunLifecycle{
		eventAdmission: make(chan struct{}, 1),
		done:           make(chan struct{}),
		conn:           conn,
	}
	lifecycle.eventAdmission <- struct{}{}
	return lifecycle
}

// beginEvent admits one complete event-side-effect operation. A successful
// stop takes the write side, waits for admitted work to finish, then makes the
// rejection irreversible before releasing it.
func (l *workerRunLifecycle) beginEvent() (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	if l.stopping.Load() {
		return nil, false
	}
	<-l.eventAdmission
	l.eventMu.RLock()
	l.eventAdmission <- struct{}{}
	if l.stopping.Load() {
		l.eventMu.RUnlock()
		return nil, false
	}
	return l.eventMu.RUnlock, true
}

func (l *workerRunLifecycle) withEvent(run func()) bool {
	releaseEvent, admitted := l.beginEvent()
	if !admitted {
		return false
	}
	defer releaseEvent()
	run()
	return true
}

// lockEventBarrier prevents new event admissions, then waits for already
// admitted side effects to drain. Holding the admission token makes TryLock
// starvation-free without spawning an orphan goroutine when ctx expires.
func (l *workerRunLifecycle) lockEventBarrier(ctx context.Context) bool {
	select {
	case <-l.eventAdmission:
	case <-ctx.Done():
		return false
	}

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if l.eventMu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			l.eventAdmission <- struct{}{}
			return false
		case <-ticker.C:
		}
	}
}

func (l *workerRunLifecycle) unlockEventBarrier() {
	l.eventMu.Unlock()
	l.eventAdmission <- struct{}{}
}

type crashHistory struct {
	count     int
	firstSeen time.Time
}

const (
	crashLoopMax    = 3                // max consecutive crashes before abort
	crashLoopWindow = 5 * time.Minute  // window for counting consecutive crashes
	resumeTimeout   = 60 * time.Second // max time for Worker.Resume(); prevents indefinite blocking
)

// NewBridge creates a new bridge.
func NewBridge(deps BridgeDeps) *Bridge {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	stopTeardownTimeout := deps.StopTeardownTimeout
	if stopTeardownTimeout <= 0 {
		stopTeardownTimeout = defaultStopTeardownTimeout
	}
	stopForwarderTimeout := deps.StopForwarderTimeout
	if stopForwarderTimeout <= 0 {
		stopForwarderTimeout = defaultStopForwarderTimeout
	}
	b := &Bridge{
		log:                  deps.Log.With("component", "bridge"),
		hub:                  deps.Hub,
		sm:                   deps.SM,
		wf:                   defaultWorkerFactory{},
		collector:            deps.EventCollector,
		turnsQuerier:         deps.TurnsQuerier,
		retryCtrl:            deps.RetryCtrl,
		agentConfigDir:       deps.AgentConfigDir,
		turnTimeout:          deps.TurnTimeout,
		stopTeardownTimeout:  stopTeardownTimeout,
		stopForwarderTimeout: stopForwarderTimeout,
		workerEnv:            deps.WorkerEnv,
		workerEnvBlocklist:   deps.WorkerEnvBlocklist,
		cronEnv:              deps.CronEnv,
		wsStore:              deps.WSStore,
		retryCancel:          make(map[string]chan struct{}),
		accum:                make(map[string]*sessionAccumulator),
		crashTracker:         make(map[string]*crashHistory),
		shutdownCtx:          shutdownCtx,
		shutdownCancel:       shutdownCancel,
		executionStore:       deps.ExecutionStore,
		repairer:             deps.Repairer,
		turnTTFT:             newTurnTTFTTracker(),
		pending:              NewPendingBuffer(),
		sharedRuntime:        newSharedRuntimeGuard(),
	}
	b.mcpConfigJSON.Store(deps.MCPConfigJSON)
	b.defaultPermissionMode.Store(worker.NormalizePermissionMode(deps.DefaultPermissionMode))
	b.agentConfigExclude.Store(deps.AgentConfigExclude)
	if deps.PermissionDedupEnabled && deps.PermissionDedupWindow > 0 {
		b.dedup = newPermissionDenyDedup(deps.PermissionDedupWindow, nil)
	}
	return b
}

// RecordPermissionDeny registers a user's tool denial so a same-fingerprint
// retry within the window is auto-suppressed. Called by Handler on the deny
// inflow path. Nil-safe via b.dedup.
func (b *Bridge) RecordPermissionDeny(sessionID, reqID, ownerID string) {
	if b.dedup != nil {
		b.dedup.RecordDeny(sessionID, reqID, ownerID)
	}
}

// suppressPermissionRequest checks the dedup cache for a recently denied
// owner+fingerprint. On hit it delivers a local denial to the worker and
// returns true so the caller skips forwarding the card to the client. On miss
// (or feature disabled / extract failure) it returns false and the request
// flows through normally; the reqID→fp mapping is registered for a later
// RecordPermissionDeny to resolve.
func (b *Bridge) suppressPermissionRequest(ctx context.Context, env *events.Envelope, w worker.Worker) bool {
	if b.dedup == nil {
		return false
	}
	data, err := messaging.ExtractPermissionData(env)
	if err != nil {
		return false // fail-open: malformed request flows through
	}
	fp := ComputeFingerprint(data.ToolName, data.Args)
	if !b.dedup.RegisterRequest(env.SessionID, env.ID, env.OwnerID, fp) {
		return false
	}
	b.log.Info("bridge: suppressing repeated permission request",
		"request_id", env.ID, "tool", data.ToolName, "session_id", env.SessionID)
	if c := observability.GatewayPermissionDedupHits(); c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", data.ToolName)))
	}
	denyMd := messaging.BuildPermissionResponse(env.ID, false, "previously denied within dedup window")
	if err := w.Input(ctx, "", denyMd); err != nil {
		b.log.Warn("bridge: local deny delivery failed for suppressed permission request",
			"request_id", env.ID, "err", err)
	}
	return true
}

// SetWorkerFactory replaces the default worker factory. Used by tests to inject
// simulated workers without requiring external CLI binaries.
func (b *Bridge) SetWorkerFactory(wf WorkerFactory) {
	b.wf = wf
}

// UpdateMCPConfig atomically updates the MCP config JSON. Used by config hot-reload.
func (b *Bridge) UpdateMCPConfig(json string) {
	b.mcpConfigJSON.Store(json)
}

// UpdateDefaultPermissionMode atomically updates the global default permission mode
// tier. Empty normalizes to workspace (the r3 Default seed); non-empty passes through
// unchanged — callers must pass a config.Validate'd value. Used by config hot-reload (#789).
func (b *Bridge) UpdateDefaultPermissionMode(mode string) {
	b.defaultPermissionMode.Store(worker.NormalizePermissionMode(mode))
}

// UpdateAgentConfigExclude atomically updates the platform → inject_exclude map.
// Used by config hot-reload for non-platform sessions (webchat/API/cron).
func (b *Bridge) UpdateAgentConfigExclude(m map[string][]string) {
	b.agentConfigExclude.Store(m)
}

// GetWorkspaceByID retrieves a workspace by its ID from the workspace store.
func (b *Bridge) GetWorkspaceByID(ctx context.Context, id string) (*session.Workspace, error) {
	if b.wsStore == nil {
		return nil, fmt.Errorf("bridge: workspace store is not configured")
	}
	return b.wsStore.GetWorkspaceByID(ctx, id)
}

// SetAuditCollector injects the audit collector for tool.call audit events
// (issue #833 P2, spec §5.2). Optional: nil leaves tool-call audit disabled.
func (b *Bridge) SetAuditCollector(ac *audit.Collector) {
	b.auditCollector = ac
}

// PendingReplayer replays a buffered supplement as a fresh input turn.
// Implemented by Handler (which owns deliverToWorker); injected after Bridge
// construction via SetPendingReplayer because Bridge is built before Handler.
type PendingReplayer interface {
	DeliverReplay(ctx context.Context, env *events.Envelope) error
}

// SetPendingReplayer late-injects the replay target (Handler). Optional: nil
// leaves done-time replay disabled (supplements buffered but not replayed).
func (b *Bridge) SetPendingReplayer(r PendingReplayer) { b.replayer = r }

// SetConfigProvider wires the live config snapshot used to resolve the runtime
// plan at launch (#946 D2). Called once during gateway init, after the config
// store exists. Nil clears the provider; plan resolution then runs with no
// config snapshot and reports the resulting coverage gap honestly.
func (b *Bridge) SetConfigProvider(fn func() *config.Config) { b.cfgProvider = fn }

// SetCatalogInvalidator registers the callback invoked on every Worker attach
// (StartSession / ResumeSession / StartFreshWorker / crash-recovery fresh
// start), so the session-scoped command catalog is invalidated and its
// generation bumped (spec §5.2, §8.7). The Handler is built after the Bridge
// and injects itself here during gateway init; nil disables invalidation.
func (b *Bridge) SetCatalogInvalidator(fn func(sessionID string)) {
	b.catalogInvalidate = fn
}

// SetReplayValidator registers the callback used by session-aware crash and
// resume replay. The Handler owns the merged catalog and its generation.
func (b *Bridge) SetReplayValidator(fn func(context.Context, string, worker.Worker, worker.InputReplay) (worker.InputReplay, error)) {
	b.replayValidate = fn
}

// BufferPending appends a busy-supplement for the fallback path (worker lacks
// mid-turn support). Called from Handler's SESSION_BUSY branch.
func (b *Bridge) BufferPending(sessionID string, env *events.Envelope, content string) bool {
	if b.pending != nil {
		return b.pending.Append(sessionID, content, env)
	}
	return false
}

func (b *Bridge) BeginSupplement(sessionID, clientMessageID, payloadHash string) (*supplementLease, supplementDisposition) {
	if b.pending == nil {
		return nil, supplementNew
	}
	return b.pending.BeginSupplement(sessionID, clientMessageID, payloadHash)
}

func (b *Bridge) replayFence(sessionID string) *sync.RWMutex {
	// FNV-1a is sufficient for lock striping and avoids retaining session IDs.
	var hash uint32 = 2166136261
	for i := 0; i < len(sessionID); i++ {
		hash ^= uint32(sessionID[i])
		hash *= 16777619
	}
	return &b.replayFences[hash%uint32(len(b.replayFences))]
}

func (b *Bridge) lockSupplementSession(sessionID string) (func(), bool) {
	b.supplementGate.RLock()
	b.replayMu.Lock()
	closed := b.replayClosed
	b.replayMu.Unlock()
	if closed {
		b.supplementGate.RUnlock()
		return nil, false
	}
	fence := b.replayFence(sessionID)
	fence.RLock()
	return func() {
		fence.RUnlock()
		b.supplementGate.RUnlock()
	}, true
}

// ClearPending drops buffered supplements for one session. Called when the
// session is reset/deleted.
func (b *Bridge) ClearPending(sessionID string) {
	fence := b.replayFence(sessionID)
	fence.Lock()
	defer fence.Unlock()
	if b.pending != nil {
		b.pending.Clear(sessionID)
	}
}

// ClearAllPending drops all buffered supplements. Called on bridge shutdown.
func (b *Bridge) ClearAllPending() {
	if b.pending != nil {
		b.pending.ClearAll()
	}
}

// HandleRepairSuccess resumes supplement replay after a runtime terminal write
// is repaired and the durable active gate is finally released.
func (b *Bridge) HandleRepairSuccess(intent execution.RepairIntent) {
	if intent.Kind == execution.RepairRuntime && intent.SessionID != "" {
		b.replayPending(intent.SessionID)
	}
}

func (b *Bridge) replayPending(sessionID string) {
	if b.pending == nil || b.replayer == nil || b.closed.Load() {
		return
	}
	merged, repr, token, ok := b.pending.DrainForReplay(sessionID)
	if !ok {
		return
	}
	replayEnv := cloneForReplay(repr, merged)

	b.replayMu.Lock()
	if b.replayClosed {
		b.replayMu.Unlock()
		return
	}
	b.replayWG.Add(1)
	b.replayMu.Unlock()

	go func() {
		defer b.replayWG.Done()
		fence := b.replayFence(sessionID)
		fence.RLock()
		defer fence.RUnlock()
		replayCtx, current := b.pending.ReplayContext(sessionID, token)
		if !current {
			return
		}
		if err := b.replayer.DeliverReplay(replayCtx, replayEnv); err != nil {
			b.log.Warn("bridge: supplement replay failed", "session_id", sessionID, "err", err)
			if !b.closed.Load() {
				b.pending.RequeueIfCurrent(sessionID, token, merged, replayEnv)
			}
			return
		}
		b.pending.CompleteReplay(sessionID, token)
	}()
}

func (b *Bridge) WaitPendingReplays(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		b.replayWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		b.log.Warn("bridge: shutdown timed out, supplement replays still running")
	}
}

// StopPendingReplays prevents repair callbacks or Done handlers from starting
// new replay work and cancels every in-flight per-session replay context.
func (b *Bridge) StopPendingReplays() {
	b.supplementGate.Lock()
	defer b.supplementGate.Unlock()
	b.replayMu.Lock()
	b.replayClosed = true
	b.replayMu.Unlock()
	b.ClearAllPending()
}

// StartSession creates a durable session, hydrates its sequence counter, and
// starts a worker. The hydration barrier is inside the bridge so non-WebSocket
// callers cannot start a worker before the first event sequence is safe.
func (b *Bridge) StartSession(ctx context.Context, p worker.SessionStartParams) error {
	if b.closed.Load() {
		return fmt.Errorf("bridge: rejecting new session during shutdown")
	}
	p, err := normalizeSessionStartParams(p)
	if err != nil {
		return err
	}

	observability.SessionStartAttempts().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(p.WorkerType))))
	start := time.Now()
	defer func() {
		observability.SessionStartDuration().Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("worker_type", string(p.WorkerType))))
	}()

	si, err := b.CreateSession(ctx, p)
	if err != nil {
		observability.SessionStartErrors().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(p.WorkerType)), attribute.String("error_type", "create_failed")))
		return err
	}

	if err := b.hub.EnsureSeqHydrated(p.ID); err != nil {
		if cleanupErr := b.sm.DeletePhysical(context.Background(), p.ID); cleanupErr != nil {
			return fmt.Errorf("bridge: hydrate session sequence: %w (cleanup failed: %w)", err, cleanupErr)
		}
		observability.SessionStartErrors().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(p.WorkerType)), attribute.String("error_type", "hydration_failed")))
		return fmt.Errorf("bridge: hydrate session sequence: %w", err)
	}

	if err := b.startPreparedSession(ctx, p, si); err != nil {
		observability.SessionStartErrors().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(p.WorkerType)), attribute.String("error_type", "start_failed")))
		return err
	}
	return nil
}

// CreateSession persists a CREATED session without attaching a worker. Conn
// uses this half of SessionInitGate before calling Hub.EnsureSeqHydrated.
func (b *Bridge) CreateSession(ctx context.Context, p worker.SessionStartParams) (*session.SessionInfo, error) {
	if b.closed.Load() {
		return nil, fmt.Errorf("bridge: rejecting new session during shutdown")
	}
	p, err := normalizeSessionStartParams(p)
	if err != nil {
		return nil, err
	}
	si, err := b.sm.CreateWithBot(ctx, p.ID, p.UserID, p.BotID, p.BotName, p.WorkerType, p.AllowedTools, p.Platform, p.PlatformKey, p.WorkspaceID, p.WorkDir, p.Title, p.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("bridge: create session: %w", err)
	}
	return si, nil
}

// StartSessionAfterHydration starts a worker for an already durable CREATED
// session. It is the second half of SessionInitGate and must only be called
// after Hub.EnsureSeqHydrated has succeeded.
func (b *Bridge) StartSessionAfterHydration(ctx context.Context, p worker.SessionStartParams) error {
	if b.closed.Load() {
		return fmt.Errorf("bridge: rejecting new session during shutdown")
	}
	p, err := normalizeSessionStartParams(p)
	if err != nil {
		return err
	}
	si, err := b.sm.Get(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("bridge: get prepared session: %w", err)
	}
	if si.State != events.StateCreated {
		return fmt.Errorf("bridge: prepared session %s is %s", p.ID, si.State)
	}
	return b.startPreparedSession(ctx, p, si)
}

func normalizeSessionStartParams(p worker.SessionStartParams) (worker.SessionStartParams, error) {
	if p.WorkDir == "" {
		return p, nil
	}
	expanded, err := validateAndExpandWorkDir(p.WorkDir)
	if err != nil {
		return p, fmt.Errorf("bridge: invalid work dir: %w", err)
	}
	p.WorkDir = expanded
	return p, nil
}

func (b *Bridge) startPreparedSession(ctx context.Context, p worker.SessionStartParams, si *session.SessionInfo) error {
	workerInfo := b.prepareWorkerInfo(p.ID, p.UserID, p.WorkDir, si)

	// Inject cron-specific env vars (e.g. admin API creds) only for cron sessions.
	// Detected via platformKey rather than platform value, since cron executor now
	// passes the job's actual platform for correct agent config resolution.
	if _, isCron := p.PlatformKey["cron_job_id"]; isCron && len(b.cronEnv) > 0 {
		for _, kv := range b.cronEnv {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				workerInfo.Env[kv[:i]] = kv[i+1:]
			}
		}
	}

	if _, err := b.createAndLaunchWorker(workerLaunchParams{
		ctx:                ctx,
		wt:                 p.WorkerType,
		workerInfo:         workerInfo,
		platform:           p.Platform,
		scope:              runtimeScopeForSession(p.WorkspaceID, p.BotID, p.BotName),
		botID:              p.BotID,
		botName:            p.BotName,
		forwardOpts:        &forwardOpts{workDir: p.WorkDir},
		injectExclude:      p.InjectExclude,
		workspaceOverrides: b.resolveWorkspaceOverrides(ctx, p.WorkspaceID),
		facts:              launchFactsForStart(&p),
	},
		func(ctx context.Context, w worker.Worker, info worker.SessionInfo) error {
			if err := w.Start(ctx, info); err != nil {
				// Use Background() to ensure DB delete succeeds even if the
				// request-scoped ctx has been cancelled (e.g. Slack 300s timeout).
				_ = b.sm.Delete(context.Background(), p.ID)
				return fmt.Errorf("bridge: start worker: %w", err)
			}
			return nil
		},
		func(_ worker.Worker, _ error) {
			// Use Background() for same reason as above — the attach failure
			// cleanup must not depend on the caller's ctx lifetime.
			_ = b.sm.Delete(context.Background(), p.ID)
		},
	); err != nil {
		return err
	}

	// Transition to RUNNING. (StateNotifier will emit state event automatically)
	if err := b.sm.Transition(ctx, p.ID, events.StateRunning); err != nil {
		// Worker started successfully but DB state transition failed — the session
		// is stuck in CREATED with no GC path (GC only sweeps IDLE/RUNNING/TERMINATED).
		// Terminate the orphan worker AND delete the session to prevent permanent
		// resource leaks. Use Background() to ensure cleanup succeeds even if the
		// request-scoped ctx was the cause of the transition failure.
		b.log.Error("bridge: transition to running failed, cleaning up orphan",
			"session_id", p.ID, "worker_type", p.WorkerType, "err", err)
		if w := b.sm.GetWorker(p.ID); w != nil {
			_ = w.Terminate(context.Background())
		}
		_ = b.sm.Delete(context.Background(), p.ID)
		return fmt.Errorf("bridge: transition to running: %w", err)
	}

	return nil
}

// ResumeSession reattaches to an existing session.
// workDir overrides the stored project directory (used by platform sessions that need a consistent workspace).
func (b *Bridge) ResumeSession(ctx context.Context, id, workDir string) error {
	return b.resumeWithOpts(ctx, id, workDir, forwardOpts{resumed: true, workDir: workDir})
}

// CurrentWorkerRunID returns the opaque run ID of the Worker currently attached
// to sessionID. The worker identity check prevents a stale attach from supplying
// correlation data after the SessionManager has already replaced it.
func (b *Bridge) CurrentWorkerRunID(sessionID string) (string, bool) {
	_, runID, ok := b.CurrentWorkerBinding(sessionID)
	return runID, ok
}

// CurrentWorkerBinding returns a Worker and its attach run ID from the same
// immutable binding. A replacement after this method returns does not break
// correlation: callers dispatch to the returned Worker and persist its run ID.
func (b *Bridge) CurrentWorkerBinding(sessionID string) (worker.Worker, string, bool) {
	value, ok := b.workerRuns.Load(sessionID)
	if !ok {
		return nil, "", false
	}
	binding, ok := value.(workerRunBinding)
	if !ok || binding.id == "" || b.sm == nil || b.sm.GetWorker(sessionID) != binding.worker {
		return nil, "", false
	}
	return binding.worker, binding.id, true
}

// StartFreshWorker isolates the currently attached Worker and starts a new
// provider session without Resume or input replay. It returns only after Start
// succeeds, so callers can safely use the returned run ID to conditionally clear
// an ambiguity fence before accepting a new input.
func (b *Bridge) StartFreshWorker(ctx context.Context, sessionID string) (string, error) {
	if b.closed.Load() {
		return "", fmt.Errorf("bridge: rejecting fresh worker during shutdown")
	}

	si, err := b.sm.Get(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if si.State == events.StateDeleted {
		return "", session.ErrSessionNotFound
	}

	if existing := b.sm.GetWorker(sessionID); existing != nil {
		if err := existing.Terminate(ctx); err != nil {
			return "", fmt.Errorf("bridge: isolate fenced worker: %w", err)
		}
		if !b.sm.DetachWorkerIf(sessionID, existing) {
			return "", fmt.Errorf("bridge: fenced worker changed during isolation")
		}
		b.clearWorkerRun(sessionID, existing, "")
	}

	if si.State == events.StateTerminated {
		if err := b.sm.Transition(ctx, sessionID, events.StateRunning); err != nil {
			return "", fmt.Errorf("bridge: transition fenced session to running: %w", err)
		}
		si.State = events.StateRunning
	}

	opts := forwardOpts{workDir: si.WorkDir}
	workerInfo := b.prepareWorkerInfo(si.ID, si.UserID, si.WorkDir, si)
	// A fence requires a provider-fresh session. In particular ACP Start uses
	// WorkerSessionID to load/fork a remote session, so clear all native-resume
	// identity before invoking Start.
	workerInfo.WorkerSessionID = ""
	workerInfo.ForkSession = false
	_, err = b.createAndLaunchWorker(workerLaunchParams{
		ctx:                ctx,
		wt:                 si.WorkerType,
		workerInfo:         workerInfo,
		platform:           si.Platform,
		scope:              runtimeScopeForSession(si.WorkspaceID, si.BotID, si.BotName),
		botID:              si.BotID,
		botName:            si.BotName,
		forwardOpts:        &opts,
		workspaceOverrides: b.resolveWorkspaceOverrides(ctx, si.WorkspaceID),
		facts:              launchFactsFor(si),
	}, func(ctx context.Context, w worker.Worker, info worker.SessionInfo) error {
		if err := w.Start(ctx, info); err != nil {
			return fmt.Errorf("bridge: start fresh worker: %w", err)
		}
		return nil
	}, func(_ worker.Worker, attachErr error) {
		b.log.Warn("bridge: attach fresh worker failed", "session_id", sessionID, "err", attachErr)
	})
	if err != nil {
		return "", err
	}
	return opts.workerRunID, nil
}

// resumeWithOpts is the internal implementation of ResumeSession that accepts
// forwardOpts for controlling retry behavior.
func (b *Bridge) resumeWithOpts(ctx context.Context, id, workDir string, opts forwardOpts) error {
	if b.closed.Load() {
		return fmt.Errorf("bridge: rejecting resume during shutdown")
	}

	// Validate workDir for consistency with StartSession.
	if workDir != "" {
		expanded, err := validateAndExpandWorkDir(workDir)
		if err != nil {
			return fmt.Errorf("bridge: invalid resume work dir: %w", err)
		}
		workDir = expanded
		opts.workDir = expanded
	}

	si, err := b.sm.Get(ctx, id)
	if err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		observability.SessionStartDuration().Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("worker_type", string(si.WorkerType))))
	}()

	if si.State == events.StateDeleted {
		return session.ErrSessionNotFound
	}

	// Hydrate the sequence before changing lifecycle state or touching the
	// current Worker. A failed hydration must leave resume retryable and cannot
	// risk allocating a sequence that collides with durable history.
	if err := b.hub.EnsureSeqHydrated(id); err != nil {
		return fmt.Errorf("%w: %w", ErrResumeSequenceUnavailable, err)
	}

	// Capture pending input before terminating so it can be re-delivered to the new worker.
	// This prevents input loss when ResumeSession is called concurrently (e.g., a
	// second user message arrives while attemptResumeFallback is starting a fresh worker).
	var pendingReplay worker.InputReplay
	if existing := b.sm.GetWorker(id); existing != nil {
		pendingReplay = snapshotInputReplay(existing.Conn())
		_ = existing.Terminate(context.Background())
		b.sm.DetachWorker(id)
		b.clearWorkerRun(id, existing, "")
	}

	// Transition TERMINATED sessions to RUNNING before attaching the worker.
	// AttachWorker rejects non-active sessions, so we must promote the state
	// first.  IDLE/CREATED sessions are active and can be attached as-is.
	if si.State == events.StateTerminated {
		if err := b.sm.Transition(ctx, id, events.StateRunning); err != nil {
			return fmt.Errorf("bridge: pre-attach transition TERMINATED→RUNNING: %w", err)
		}
		si.State = events.StateRunning
	}

	workerInfo := b.prepareWorkerInfo(si.ID, si.UserID, workDir, si)
	w, err := b.createAndLaunchWorker(workerLaunchParams{
		ctx:                ctx,
		wt:                 si.WorkerType,
		workerInfo:         workerInfo,
		platform:           si.Platform,
		scope:              runtimeScopeForSession(si.WorkspaceID, si.BotID, si.BotName),
		botID:              si.BotID,
		botName:            si.BotName,
		forwardOpts:        &opts,
		workspaceOverrides: b.resolveWorkspaceOverrides(ctx, si.WorkspaceID),
		facts:              launchFactsFor(si),
	},
		func(ctx context.Context, w worker.Worker, info worker.SessionInfo) error {
			if si.State != events.StateRunning {
				if err := b.sm.Transition(ctx, id, events.StateRunning); err != nil {
					return err
				}
			}
			resumeCtx, resumeCancel := context.WithTimeout(ctx, resumeTimeout)
			err := w.Resume(resumeCtx, info)
			resumeCancel()
			if err != nil && !errors.Is(err, worker.ErrFellBackToFreshStart) {
				return fmt.Errorf("bridge: resume start: %w", err)
			}
			if errors.Is(err, worker.ErrFellBackToFreshStart) {
				opts.resumed = false
			}
			return nil
		},
		func(_ worker.Worker, _ error) {
			// Roll back to TERMINATED if AttachWorker fails (pool full, ctx
			// cancelled, etc.). Without this, the session stays in RUNNING
			// with no worker and no forwardEvents goroutine for self-healing.
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bgCancel()
			if err := b.sm.Transition(bgCtx, id, events.StateTerminated); err != nil {
				b.log.Warn("bridge: resume attach-error rollback to TERMINATED failed",
					"session_id", id, "err", err)
			}
		},
	)
	if err != nil {
		return err
	}

	// Refresh ExpiresAt so a reactivated session isn't immediately killed by GC max_lifetime.
	if err := b.sm.ResetExpiry(ctx, id); err != nil {
		b.log.Warn("bridge: resume reset expiry failed", "session_id", id, "err", err)
	}

	// Re-deliver pending input that was captured before the old worker was terminated.
	// This covers the case where a concurrent message triggered ResumeSession while
	// attemptResumeFallback was starting a fresh worker — the fresh worker's buffered
	// input would otherwise be lost when the old worker is terminated here.
	if pendingReplay.Content != "" || pendingReplay.Skill != nil {
		b.log.Info("bridge: re-delivering pending input to resumed worker",
			"session_id", id, "content_len", len(pendingReplay.Content),
			"skill", pendingReplay.Skill != nil)
		if err := b.deliverInputReplayForSession(ctx, id, w, pendingReplay); err != nil {
			b.log.Warn("bridge: pending input re-delivery failed",
				"session_id", id, "err", err)
		}
	}

	// Notify client of current state.
	stateToNotify := si.State
	if stateToNotify == events.StateTerminated || stateToNotify == events.StateIdle {
		stateToNotify = events.StateRunning // We just transitioned it
	}
	stateEvt := events.NewEnvelope(aep.NewID(), id, 0, events.State, events.StateData{
		State: stateToNotify,
	})
	if err := b.hub.SendToSession(ctx, stateEvt); err != nil {
		b.log.Warn("bridge: resume state notify failed", "session_id", id, "err", err)
	}

	return nil
}

// copyEnvelope delegates to events.Clone, which performs a deep copy of
// map[string]any Event.Data to eliminate shared map headers.
// This prevents data races when Hub.Run encodes the clone concurrently with
// Bridge.forwardEvents encoding the original (e.g., for msgStore.Append).
var _ = events.Clone // compile-time check that Clone is accessible

// StartPlatformSession creates a session for a platform message if it doesn't already exist.
// Implements messaging.SessionStarter. Idempotent: returns nil if session exists with a live worker.
//
// Decision logic (state-based with Resume→Start fallback):
//  1. No DB record → Create + Start (--session-id)
//  2. Worker alive → Reuse (forward message)
//  3. No worker, state=CREATED → Start (--session-id)
//  4. No worker, state=TERMINATED → Resume (--resume), fallback to Start
//     Preserves conversation context for idle_timeout/max_lifetime/gc/crash paths.
//     After /reset, session files are already deleted so Resume falls back naturally.
//  5. No worker, state=DELETED → Start fresh (no DB record to resume)
//  6. No worker, state=RUNNING/IDLE → Resume (--resume), fallback to Start
func (b *Bridge) StartPlatformSession(ctx context.Context, params worker.SessionStartParams) error {
	sessionID, ownerID, workerType, workDir, sandbox, platform := params.ID, params.UserID, string(params.WorkerType), params.WorkDir, params.PlatformKey["_sandbox"], params.Platform
	platformKey, botID, botName, injectExclude := params.PlatformKey, params.BotID, params.BotName, params.InjectExclude
	b.log.Debug("bridge: StartPlatformSession called", "session_id", sessionID, "owner_id", ownerID, "worker_type", workerType, "work_dir", workDir, "sandbox", sandbox, "platform", platform, "platform_key", platformKey, "bot_id", botID, "bot_name", botName)
	injectSandbox(platformKey, sandbox)
	si, err := b.sm.Get(ctx, sessionID)
	if err == nil {
		if w := b.sm.GetWorker(sessionID); w != nil {
			// Only reuse if session is still active. TERMINATED sessions with a stale
			// worker pointer must fall through to ResumeSession to ensure the message
			// is delivered, not silently dropped (bug: worker pointer non-nil after
			// transitionState nils it, but only after SIGTERM completes asynchronously).
			if si.State.IsActive() {
				// Re-inject current sandbox into persisted PlatformKey so
				// stale values don't remain after bot config changes.
				injectSandbox(si.PlatformKey, sandbox)
				return nil
			}
		}
		// Orphan: session record exists but worker is gone.
		if si.State == events.StateCreated {
			b.log.Info("bridge: orphan platform session unstarted, starting fresh", "session_id", sessionID)
			return b.startOrResumeOnInUse(ctx, sessionID, ownerID, worker.WorkerType(workerType), workDir, platform, platformKey, botID, botName, injectExclude...)
		}
		// TERMINATED — try resume first to preserve conversation context.
		// ResumeSession handles TERMINATED→RUNNING transition internally.
		// If session files are missing (e.g. after /reset deleted them),
		// the worker's Resume() falls back to fresh start automatically.
		// This restores pre-fe8dae54 behavior where all orphan sessions
		// attempted resume regardless of state. Fixes #682.
		if si.State == events.StateTerminated {
			// Some workers (e.g. CodexCLI with singleton process) cannot
			// resume terminated sessions. Query capability instead of type switch.
			if !worker.CanResumeTerminated(worker.WorkerType(workerType)) {
				b.log.Info("bridge: skipping resume for terminated session, worker cannot resume terminated state",
					"session_id", sessionID, "worker_type", workerType)
				injectSandbox(si.PlatformKey, sandbox)
				return b.startOrResumeOnInUse(ctx, sessionID, ownerID, worker.WorkerType(workerType), workDir, platform, platformKey, botID, botName, injectExclude...)
			}
			b.log.Info("bridge: orphan platform session terminated, attempting resume", "session_id", sessionID)
			injectSandbox(si.PlatformKey, sandbox)
			if err := b.ResumeSession(ctx, sessionID, workDir); err != nil {
				if errors.Is(err, ErrResumeSequenceUnavailable) {
					return err
				}
				if errors.Is(err, worker.ErrResumeCheckFailed) {
					return fmt.Errorf("bridge: resume verification failed for terminated session: %w", err)
				}
				b.log.Warn("bridge: resume failed for terminated session, falling back to new session",
					"session_id", sessionID, "err", err)
				return b.startOrResumeOnInUse(ctx, sessionID, ownerID, worker.WorkerType(workerType), workDir, platform, platformKey, botID, botName, injectExclude...)
			}
			return nil
		}
		if si.State == events.StateDeleted {
			b.log.Info("bridge: orphan platform session already deleted, starting fresh", "session_id", sessionID)
			return b.startOrResumeOnInUse(ctx, sessionID, ownerID, worker.WorkerType(workerType), workDir, platform, platformKey, botID, botName, injectExclude...)
		}
		// If Resume fails (session files deleted or corrupted), fall back to Start.
		b.log.Info("bridge: orphan platform session, resuming", "session_id", sessionID, "state", si.State)
		// Re-inject current sandbox into the loaded session so resume uses
		// the latest config, not a potentially stale persisted value.
		injectSandbox(si.PlatformKey, sandbox)
		if err := b.ResumeSession(ctx, sessionID, workDir); err != nil {
			if errors.Is(err, ErrResumeSequenceUnavailable) {
				return err
			}
			if errors.Is(err, worker.ErrResumeCheckFailed) {
				return fmt.Errorf("bridge: resume verification failed: %w", err)
			}
			b.log.Warn("bridge: resume failed, falling back to new session",
				"session_id", sessionID, "state", si.State, "err", err)
			return b.startOrResumeOnInUse(ctx, sessionID, ownerID, worker.WorkerType(workerType), workDir, platform, platformKey, botID, botName, injectExclude...)
		}
		return nil
	}

	wt := worker.WorkerType(workerType)
	if wt == "" {
		return fmt.Errorf("bridge: no worker_type configured for platform session %s", sessionID)
	}

	return b.startOrResumeOnInUse(ctx, sessionID, ownerID, wt, workDir, platform, platformKey, botID, botName, injectExclude...)
}

// startOrResumeOnInUse attempts StartSession; if the worker reports its session
// files are already in use (leftover from a crashed session), falls back to
// ResumeSession to recover the existing conversation history.
func (b *Bridge) startOrResumeOnInUse(ctx context.Context, sessionID, ownerID string, wt worker.WorkerType, workDir, platform string, platformKey map[string]string, botID, botName string, injectExclude ...string) error {
	if err := b.StartSession(ctx, worker.SessionStartParams{
		ID:            sessionID,
		UserID:        ownerID,
		BotID:         botID,
		BotName:       botName,
		WorkerType:    wt,
		WorkDir:       workDir,
		Platform:      platform,
		PlatformKey:   platformKey,
		InjectExclude: injectExclude,
	}); err != nil {
		if isWorkerInUseError(err) {
			b.log.Info("bridge: worker rejected as in-use, switching to resume", "session_id", sessionID, "err", err)
			return b.ResumeSession(ctx, sessionID, workDir)
		}
		return err
	}
	return nil
}

// ResetSession terminates the worker, deletes session files, and starts fresh.
// Crash recovery: orphan sessions try Resume first; if files are gone,
// StartPlatformSession falls back to Start(--session-id).
func (b *Bridge) ResetSession(ctx context.Context, sessionID string) error {
	w := b.sm.GetWorker(sessionID)
	if w == nil {
		return fmt.Errorf("bridge: reset: no worker for session %s", sessionID)
	}
	fence := b.replayFence(sessionID)
	fence.Lock()
	defer fence.Unlock()

	// ResetContext may replace the Worker's connection before returning. Remove
	// the old run binding first so concurrent input fails closed instead of being
	// sent to a new connection while persisted against the old run.
	suspendedBinding, bindingSuspended := b.suspendWorkerRun(sessionID, w)
	result, err := w.ResetContext(ctx)
	if err != nil {
		if bindingSuspended {
			b.restoreWorkerRun(sessionID, suspendedBinding)
		}
		return fmt.Errorf("bridge: reset worker: %w", err)
	}
	// A successful reset establishes a new conversation context under the same
	// session ID. Discard supplements and invalidate in-flight replay tokens from
	// the old context before the replacement forwarder can emit events.
	if b.pending != nil {
		b.pending.Clear(sessionID)
	}
	workerRunID := ""
	var replacementBinding workerRunBinding
	if result.ConnReplaced {
		if b.sm.GetWorker(sessionID) != w {
			return fmt.Errorf("bridge: reset worker changed before run binding")
		}
		// A reset replaces the CONNECTION, not the process: no new Worker was
		// launched, so re-resolving the plan here would claim this run was
		// launched under a config it never saw. The previous run's plan is
		// carried forward as the historical fact, and the carry is logged so a
		// later diagnostic does not mistake it for a fresh resolution.
		carried := launchPlan{SessionID: sessionID}
		if suspendedBinding.launchPlan != nil {
			carried = *suspendedBinding.launchPlan
		}
		if carried.Fingerprint == "" {
			// The previous run predates plan capture (or was bound before this
			// code shipped, e.g. a binding made by a test). Drop the partial
			// value rather than keeping a half-populated plan that would read
			// like "resolved and empty".
			carried = launchPlan{SessionID: sessionID}
		}
		replacementBinding = b.bindWorkerRun(sessionID, w, "", carried)
		b.log.Debug("bridge: reset carried the previous launch plan forward",
			"session_id", sessionID,
			"plan_fingerprint", carried.Fingerprint,
			"worker_type", carried.WorkerType)
		workerRunID = replacementBinding.id
	} else if bindingSuspended {
		b.restoreWorkerRun(sessionID, suspendedBinding)
	}

	// Reload agent config so the worker's next session picks up file changes.
	if si, err := b.sm.Get(ctx, sessionID); err == nil {
		if su, ok := w.(worker.SystemPromptUpdater); ok {
			info := b.buildWorkerInfo(si.ID, si.UserID, si.WorkDir, si)
			injectSlackEnv(&info, si.PlatformKey)
			info.Env = injectGatewayContext(info.Env, si.Platform, si.BotID, si.BotName, si.UserID, si.PlatformKey, si.ID, si.WorkDir)
			facts := buildRuntimeFacts(w, info, si.Platform, runtimeScopeForSession(si.WorkspaceID, si.BotID, si.BotName))
			b.injectAgentConfig(&info, facts, si.Platform, si.BotName, si.BotID, nil, b.resolveWorkspaceOverrides(ctx, si.WorkspaceID))
			if info.SystemPrompt != "" {
				su.UpdateSystemPrompt(info.SystemPrompt)
				b.log.Info("bridge: reset reloaded agent config",
					"session_id", sessionID, "platform", si.Platform, "bot_id", si.BotID, "bot_name", si.BotName,
					"prompt_len", len(info.SystemPrompt))
			}
		}
	}

	b.accumMu.Lock()
	if acc, ok := b.accum[sessionID]; ok {
		acc.TurnCount.Store(0)
		if result.ConnReplaced {
			acc.Generation.Add(1)
		}
	}
	b.accumMu.Unlock()
	b.compressCache.Delete(sessionID)
	b.dedup.ClearSession(sessionID) // nil-safe: reset clears denial memory so the user can re-authorize

	if !result.ConnReplaced {
		return nil
	}

	// The worker already incremented resetGeneration in its ResetContext
	// (before terminating the old process), so the OLD forwardEvents goroutine
	// can detect the generation mismatch. No need to increment again here.

	b.fwdWg.Add(1)
	go func() {
		defer b.fwdWg.Done()
		defer close(replacementBinding.lifecycle.done)
		defer b.clearWorkerRun(sessionID, w, workerRunID)
		b.launchForwarderLocked(replacementBinding, sessionID, forwardOpts{ctx: context.Background(), workerRunID: workerRunID})
	}()

	return nil
}

// ErrWorkDirImmutable is returned by SwitchWorkDir when the target session is
// bound to a workspace (si.WorkspaceID != ""). Workspace-bound sessions derive
// work_dir from their workspace, immutable for the session's lifetime; /cd would
// start the worker in a directory the workspace doesn't own while keeping the
// session bound to it — and, because DeleteWorkspaceIfEmpty counts active
// sessions by workspace_id (not work_dir), could let another workspace be
// hard-deleted while a worker still runs in its directory. The REST path
// (api.go) guards this at the HTTP layer; this sentinel lets the WS /cd path
// (commands.go) reuse the same rule so WebChat's native channel can't bypass it
// (review P1-2).
var ErrWorkDirImmutable = errors.New("work_dir immutable for workspace-bound session")

// SwitchWorkDirResult holds the result of a workdir switch operation.
type SwitchWorkDirResult struct {
	OldSessionID string
	NewSessionID string
	WorkDir      string
	Resumed      bool // true = resumed existing session with conversation history
}

// SwitchWorkDir terminates the current session's worker, transitions it to idle,
// and creates a new session with the given workDir. The new session inherits
// the same user, bot, worker type, and platform context.
// If the target directory has an existing session, it is resumed to preserve
// conversation history. Otherwise a fresh session is created.
func (b *Bridge) SwitchWorkDir(ctx context.Context, oldSessionID, newWorkDir string) (*SwitchWorkDirResult, error) {
	si, err := b.sm.Get(ctx, oldSessionID)
	if err != nil {
		return nil, fmt.Errorf("switch-workdir: get session: %w", err)
	}

	if !si.State.IsActive() {
		return nil, fmt.Errorf("switch-workdir: session not active (state: %s)", si.State)
	}

	// Workspace-bound sessions can't /cd: their work_dir is owned by the workspace.
	// See ErrWorkDirImmutable. REST enforces this too (api.go); this guard is the
	// backstop for the WS /cd path that bypasses the HTTP layer (review P1-2).
	if si.WorkspaceID != "" {
		return nil, fmt.Errorf("switch-workdir: %w", ErrWorkDirImmutable)
	}

	expanded, err := validateAndExpandWorkDir(newWorkDir)
	if err != nil {
		return nil, fmt.Errorf("switch-workdir: %w", err)
	}

	// Terminate old worker and park old session.
	if w := b.sm.GetWorker(oldSessionID); w != nil {
		if err := w.Terminate(ctx); err != nil {
			b.log.Warn("switch-workdir: worker terminate failed", "session_id", oldSessionID, "err", err)
		}
		b.sm.DetachWorker(oldSessionID)
		b.clearWorkerRun(oldSessionID, w, "")
	}

	if err := b.sm.Transition(ctx, oldSessionID, events.StateIdle); err != nil {
		b.log.Warn("switch-workdir: transition to idle failed", "session_id", oldSessionID, "err", err)
	}

	// Derive target session key using the new workDir.
	var newID string
	if si.Platform != "" && len(si.PlatformKey) > 0 {
		var pc session.PlatformContext
		pc.Platform = si.Platform
		pc.WorkDir = expanded
		pc.FromMap(si.PlatformKey)
		newID = session.DerivePlatformSessionKey(si.UserID, si.WorkerType, pc)
	} else {
		// WebChat / direct-WS: derive deterministically from (owner, workerType,
		// clientKey, workspace, workDir) so switch-workdir is resumable with a
		// stable ID (review P2). Mirrors DerivePlatformSessionKey's doc note
		// (key.go) which directs Web callers to DeriveSessionKey directly.
		newID = session.DeriveSessionKey(si.UserID, si.WorkerType, si.ClientKey, si.WorkspaceID, expanded)
	}

	// Try to resume existing target session first (preserve conversation history).
	resumed := false
	targetSI, err := b.sm.Get(ctx, newID)
	if err == nil && targetSI.State != events.StateDeleted {
		if b.sm.GetWorker(newID) != nil {
			b.log.Warn("switch-workdir: target session already has active worker", "session_id", newID)
		} else if err := b.ResumeSession(ctx, newID, expanded); err != nil {
			b.log.Warn("switch-workdir: resume failed, creating fresh session",
				"session_id", newID, "state", targetSI.State, "err", err)
		} else {
			resumed = true
			b.log.Info("switch-workdir: resumed existing session",
				"old_session_id", oldSessionID,
				"new_session_id", newID,
				"work_dir", expanded,
			)
		}
	}

	if !resumed {
		// TODO(inject-per-bot): per-bot injectExclude from the messaging adapter
		// is not persisted in the session record, so SwitchWorkDir falls back to
		// platform/global from the atomic config map. Fix requires storing
		// injectExclude in the session record or giving bridge access to adapters.
		excl := b.resolveInjectExclude(si.Platform, nil)
		if err := b.StartSession(ctx, worker.SessionStartParams{
			ID:            newID,
			UserID:        si.UserID,
			BotID:         si.BotID,
			BotName:       si.BotName,
			WorkerType:    si.WorkerType,
			AllowedTools:  si.AllowedTools,
			WorkDir:       expanded,
			Platform:      si.Platform,
			PlatformKey:   si.PlatformKey,
			Title:         si.Title,
			WorkspaceID:   si.WorkspaceID,
			InjectExclude: excl,
		}); err != nil {
			return nil, fmt.Errorf("switch-workdir: start session: %w", err)
		}
		b.log.Info("switch-workdir: created fresh session",
			"old_session_id", oldSessionID,
			"new_session_id", newID,
			"work_dir", expanded,
		)
	}

	return &SwitchWorkDirResult{
		OldSessionID: oldSessionID,
		NewSessionID: newID,
		WorkDir:      expanded,
		Resumed:      resumed,
	}, nil
}

// isWorkerInUseError checks if the worker rejected the session because its
// session files already exist on disk (e.g. from a mid-start crash).
func isWorkerInUseError(err error) bool {
	var we *worker.WorkerError
	return errors.As(err, &we) && we.Kind == worker.ErrKindSessionInUse
}

// Shutdown signals the bridge that the gateway is shutting down.
// It sets the closed flag so forwardEvents goroutines skip crash detection,
// cancels the shutdown context to abort pending auto-retries,
// then waits for all forwardEvents goroutines to complete or ctx to expire.
func (b *Bridge) Shutdown(ctx context.Context) {
	b.MarkClosed()
	b.WaitForwarders(ctx)
	b.WaitPendingReplays(ctx)
	// Clear compressCache after all async compression goroutines have finished.
	b.compressCache.Range(func(key, _ any) bool {
		b.compressCache.Delete(key)
		return true
	})
	// Drop any buffered mid-turn supplements so stale entries don't survive
	// shutdown. The buffer is in-memory (not persisted), but clearing here
	// keeps the Bridge's post-Shutdown state coherent if anything re-enters
	// the Bridge during the tear-down tail (e.g. a late handler callback).
	// Task 11 cleanup path; runs after forwarders drain so a racing
	// DrainAndMerge in replay completes first.
	b.ClearAllPending()
}

// MarkClosed sets the closed flag and cancels the shutdown context so that:
//   - New session creation (StartSession/resumeWithOpts) is rejected immediately.
//   - handleWorkerExit skips crash recovery during shutdown.
//   - Pending auto-retry goroutines are cancelled.
//
// Must be called BEFORE TerminateAllWorkers to prevent the race where an
// in-flight message handler creates a worker that is immediately killed.
func (b *Bridge) MarkClosed() {
	b.closed.Store(true)
	b.StopPendingReplays()
	b.shutdownCancel()
}

// WaitForwarders waits for all forwardEvents goroutines to complete or ctx to expire.
func (b *Bridge) WaitForwarders(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		b.fwdWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		b.log.Warn("bridge: shutdown timed out, some forwardEvents goroutines still running")
	}
}

// defaultBrainFn adapts brain.Global() to the compressorBrain interface.
// Returns nil if Brain is not configured (graceful degradation).
//
// Race note: brain.Global() returns a snapshot that could be replaced by
// config hot-reload between this call and the actual ChatWithOptions invocation.
// This is an acceptable tradeoff: the worst case is a stale (but still valid)
// Brain client being used for one compression cycle — functionally correct,
// just potentially using an older config or API key.
func defaultBrainFn() compressorBrain {
	b := brain.Global()
	if b == nil {
		return nil
	}
	return b
}

// compressCacheEntry stores the result of async history compression.
type compressCacheEntry struct {
	turns               []worker.ConversationTurn
	latestTurnCreatedAt int64 // unix millis of the newest turn used; mismatch = stale
}

// resolveCachedHistory checks the compressCache for a valid cached result.
// Returns (turns, true) on cache hit, (nil, false) on miss/stale/invalid.
// Stale or invalid entries are cleaned up automatically.
func (b *Bridge) resolveCachedHistory(sessionID string, latestCreatedAt int64) ([]worker.ConversationTurn, bool) {
	cached, ok := b.compressCache.Load(sessionID)
	if !ok {
		return nil, false
	}
	entry, ok := cached.(*compressCacheEntry)
	if !ok {
		b.compressCache.Delete(sessionID)
		return nil, false
	}
	if entry.latestTurnCreatedAt == latestCreatedAt {
		return entry.turns, true
	}
	b.compressCache.Delete(sessionID)
	return nil, false
}

// buildNotifyEnvelope creates a synthetic Message event for user notifications.
func buildNotifyEnvelope(sessionID, msg string, seq int64) *events.Envelope {
	return events.NewEnvelope(aep.NewID(), sessionID, seq, events.Message, map[string]any{"content": msg})
}

// sanitizeLastInput filters control-like text from lastInput before re-delivery
// during crash recovery. When a worker crashes, the last user input is captured
// for crash recovery. If that input matches a control command pattern ($gc, /reset,
// etc.), re-delivering it would cause the new worker to interpret it as a command,
// triggering another termination — defeating the purpose of crash recovery.
func sanitizeLastInput(input string) string {
	if input == "" {
		return ""
	}
	// Single-line control command: discard entirely.
	if messaging.ParseControlCommand(input) != nil {
		return ""
	}
	// Multi-line: filter out lines that are control commands.
	lines := strings.Split(input, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if messaging.ParseControlCommand(strings.TrimSpace(line)) != nil {
			continue
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return ""
	}
	return strings.Join(filtered, "\n")
}

func (b *Bridge) deliverInputReplay(ctx context.Context, w worker.Worker, replay worker.InputReplay) error {
	// This legacy low-level helper has no session identity and therefore cannot
	// establish catalog authority. Keep it fail-closed for structured replay;
	// production paths must call deliverInputReplayForSession.
	if replay.Skill != nil {
		return fmt.Errorf("%w: session-aware native replay validation required", worker.ErrSkillNotSupported)
	}
	return b.deliverInputReplayForSession(ctx, "", w, replay)
}

func (b *Bridge) deliverInputReplayForSession(ctx context.Context, sessionID string, w worker.Worker, replay worker.InputReplay) error {
	if replay.Skill != nil && sessionID != "" {
		if b.replayValidate == nil {
			return fmt.Errorf("%w: native replay validation unavailable", worker.ErrSkillNotSupported)
		}
		validated, err := b.replayValidate(ctx, sessionID, w, replay)
		if err != nil {
			return err
		}
		replay = validated
	}
	if replay.Skill == nil {
		if replay.Content == "" {
			return nil
		}
		return w.Input(ctx, replay.Content, nil)
	}
	if invoker, ok := worker.AsNativeInvoker(w); ok {
		return invoker.InvokeNativeCommand(ctx, *replay.Skill)
	}
	// The replacement Worker lacks any native command path — typically
	// because worker_type changed between the crash and recovery. The
	// invocation must NOT be replayed as an ordinary prompt: that would turn
	// "execute the Skill" into "let the model guess the Skill" (plan
	// constraint #7; NativeCommandInvoker contract: "Workers that cannot
	// execute a native command must not receive the invocation as an ordinary
	// prompt"). Fail loudly instead of silently degrading the semantics.
	b.log.Warn("bridge: native Skill replay rejected, replacement worker lacks native command invoker",
		"worker_type", w.Type(), "skill", replay.Skill.Name)
	return fmt.Errorf("%w: worker %s cannot replay native Skill %q as text", worker.ErrSkillNotSupported, w.Type(), replay.Skill.Name)
}

// firstNonEmpty returns the first non-empty string from the given values.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// injectSandbox writes the current sandbox value into the platformKey map
// so it is persisted and propagated through session resume paths.
func injectSandbox(platformKey map[string]string, sandbox string) {
	if sandbox != "" && platformKey != nil {
		platformKey[worker.SandboxPlatformKey] = sandbox
	}
}

// prepareWorkerInfo builds a complete worker.SessionInfo with all standard env
// injection applied. This consolidates the buildWorkerInfo + injectSlackEnv +
// injectGatewayContext trio that was previously duplicated across 3 call sites.
func (b *Bridge) prepareWorkerInfo(sessionID, userID, workDir string, si *session.SessionInfo) worker.SessionInfo {
	info := b.buildWorkerInfo(sessionID, userID, workDir, si)

	// Populate conversation history from the turns table for workers that may
	// need text-level context recovery. CodexCLI always creates a new thread;
	// ACP may need the same fallback when its agent cannot load the session.
	// Claude Code and OpenCode Server retain their native session state.
	// Fresh sessions (StateCreated) are intentionally NOT skipped: DeriveSessionKey
	// deterministically reuses the same sessionID for a given chat, so after zombie
	// reclamation (or gateway restart) re-creates the session record, the turns table
	// still holds prior turns. Injecting them gives the new codex ephemeral thread
	// text-level context continuity (issue #815, L3/L4 fallback). QueryTurns returns
	// empty for genuinely new sessions (different chat → different sessionID), so the
	// len(turns) > 0 guard below is a no-op there.
	if (si.WorkerType == worker.TypeCodexCLI || si.WorkerType == worker.TypeACP) && b.turnsQuerier != nil {
		turns, err := b.turnsQuerier.QueryTurns(b.shutdownCtx, sessionID, 50, 0)
		if err != nil {
			b.log.Warn("bridge: query turns for history recovery failed", "session_id", sessionID, "err", err)
		} else if len(turns) > 0 {
			compressor := NewHistoryCompressor(b.log, b.hub)
			latestCreatedAt := turns[len(turns)-1].CreatedAt

			// Check async compression cache: if turns haven't changed, reuse result.
			if cached, hit := b.resolveCachedHistory(sessionID, latestCreatedAt); hit {
				info.ConversationHistory = cached
				b.log.Debug("bridge: using cached compressed history",
					"session_id", sessionID, "turns", len(cached))
			}

			// Cache miss: inject truncated history immediately (sub-ms),
			// then fire async Brain compression for next resume.
			if info.ConversationHistory == nil {
				result := compressor.TruncateHistory(turns)
				info.ConversationHistory = result.Turns
				b.log.Debug("bridge: truncated history injected, scheduling async compression",
					"session_id", sessionID, "turns", len(result.Turns))

				// Skip async compression during shutdown to avoid spawning
				// goroutines that fwdWg.Wait() must then wait for.
				if b.closed.Load() {
					return info
				}
				b.fwdWg.Add(1)
				go func() {
					defer b.fwdWg.Done()
					asyncResult := compressor.CompressHistory(b.shutdownCtx, turns, sessionID, defaultBrainFn)
					if asyncResult.Compressed {
						b.compressCache.Store(sessionID, &compressCacheEntry{
							turns:               asyncResult.Turns,
							latestTurnCreatedAt: latestCreatedAt,
						})
					}
				}()
			}
		}
	}

	injectSlackEnv(&info, si.PlatformKey)
	info.Env = injectGatewayContext(info.Env, si.Platform, si.BotID, si.BotName, si.UserID, si.PlatformKey, sessionID, workDir)
	return info
}

// buildWorkerInfo constructs a worker.SessionInfo from session metadata,
// carrying over bridge-level config (workerEnv, blocklist).
func (b *Bridge) buildWorkerInfo(sessionID, userID, workDir string, si *session.SessionInfo) worker.SessionInfo {
	permissionMode := si.PermissionCeiling
	if permissionMode == "" {
		permissionMode = b.resolveWorkspacePermissionMode(si.WorkspaceID)
	}
	info := worker.SessionInfo{
		SessionID:       sessionID,
		UserID:          userID,
		ProjectDir:      workDir,
		AllowedTools:    si.AllowedTools,
		WorkerSessionID: si.WorkerSessionID,
		ConfigEnv:       b.workerEnv,
		ConfigBlocklist: b.workerEnvBlocklist,
		Sandbox:         si.PlatformKey[worker.SandboxPlatformKey],
		ACPCommand:      si.PlatformKey[worker.ACPCommandPlatformKey],
		ForkSession:     si.PlatformKey[worker.ForkSessionPlatformKey] == "true",
		JSONSchema:      si.PlatformKey[worker.JSONSchemaPlatformKey],
		PermissionMode:  permissionMode,
		// TODO: platform adapters (Slack/Feishu) need to populate ForkSession/JSONSchema
		// into PlatformKey for this wiring to take effect; tracked in UX follow-up.
	}

	// MCP config injection — 3 scenarios:
	// 1. Cron platform: suppress all MCP to save ~600 MB per worker
	// 2. Configured MCP servers: restrict workers to declared servers only
	// 3. Not configured: no injection → Claude Code default discovery
	if _, isCron := si.PlatformKey["cron_job_id"]; isCron {
		info.MCPConfig = `{"mcpServers":{}}`
		info.StrictMCPConfig = true
	} else if mcp, _ := b.mcpConfigJSON.Load().(string); mcp != "" {
		info.MCPConfig = mcp
		info.StrictMCPConfig = true
	}

	return info
}

// injectSlackEnv injects HOTPLEX_SLACK_CHANNEL_ID and HOTPLEX_SLACK_THREAD_TS
// into the worker env map for CLI subcommand auto-resolution.
func injectSlackEnv(info *worker.SessionInfo, platformKey map[string]string) {
	if platformKey == nil {
		return
	}
	if chID := platformKey["channel_id"]; chID != "" {
		if info.Env == nil {
			info.Env = make(map[string]string)
		}
		info.Env["HOTPLEX_SLACK_CHANNEL_ID"] = chID
		if threadTS := platformKey["thread_ts"]; threadTS != "" {
			info.Env["HOTPLEX_SLACK_THREAD_TS"] = threadTS
		}
	}
}

// validateAndExpandWorkDir expands a work directory path and validates it for safety.
// Combines config.ExpandAndAbs + security.ValidateWorkDir into a single call to prevent
// accidental omission of the security check at call sites.
func validateAndExpandWorkDir(input string) (string, error) {
	expanded, err := config.ExpandAndAbs(input)
	if err != nil {
		return "", fmt.Errorf("expand work dir: %w", err)
	}
	if err := security.ValidateWorkDir(expanded); err != nil {
		return "", fmt.Errorf("unsafe work dir: %w", err)
	}
	return expanded, nil
}

// injectGatewayContext injects unified GATEWAY_* environment variables into
// the worker env map. These vars provide platform-agnostic runtime context so
// workers can call platform APIs, construct paths, and understand their session
// without parsing logs or gateway internals.
//
// Existing HOTPLEX_SLACK_* vars are preserved for backward compatibility.
func injectGatewayContext(env map[string]string, platform, botID, botName, userID string, platformKey map[string]string, sessionID, workDir string) map[string]string {
	if env == nil {
		env = make(map[string]string)
	}
	env["GATEWAY_PLATFORM"] = platform
	env["GATEWAY_BOT_ID"] = botID
	if botName != "" {
		env["GATEWAY_BOT_NAME"] = botName
	}
	env["GATEWAY_USER_ID"] = userID
	env["GATEWAY_SESSION_ID"] = sessionID
	if workDir != "" {
		env["GATEWAY_WORK_DIR"] = workDir
	}
	if chID := firstNonEmpty(platformKey["channel_id"], platformKey["chat_id"]); chID != "" {
		env["GATEWAY_CHANNEL_ID"] = chID
	}
	if threadID := firstNonEmpty(platformKey["thread_ts"], platformKey["message_id"]); threadID != "" {
		env["GATEWAY_THREAD_ID"] = threadID
	}
	if teamID := platformKey["team_id"]; teamID != "" {
		env["GATEWAY_TEAM_ID"] = teamID
	}
	// Webhook-triggered context: pr_number injected by TriggerByName extra map.
	if prNum := platformKey["pr_number"]; prNum != "" {
		env["TARGET_PR"] = prNum
	}
	return env
}

func (b *Bridge) sendError(sessionID string, code events.ErrorCode, format string, args ...any) {
	env := events.NewEnvelope(aep.NewID(), sessionID, 0, events.Error, events.ErrorData{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	})
	_ = b.hub.SendToSession(context.Background(), env)
}
