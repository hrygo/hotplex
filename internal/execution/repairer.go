package execution

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/observability"
)

type RepairKind int

const (
	RepairDelivery RepairKind = iota
	RepairRuntime
)

func (k RepairKind) String() string {
	if k == RepairRuntime {
		return "runtime"
	}
	return "delivery"
}

type RepairIntent struct {
	ExecutionID string
	SessionID   string
	OwnerID     string
	WorkerRunID string
	Kind        RepairKind
	Status      string
	ErrorCode   string
	enqueuedAt  time.Time
	attempts    int
	nextAttempt time.Time
}

type RepairConfig struct {
	QueueCapacity    int
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
	MaxLifetime      time.Duration
	ShutdownTimeout  time.Duration
	SyncRetryTimeout time.Duration
	TickInterval     time.Duration
}

func DefaultRepairConfig(sessionPoolMax int) RepairConfig {
	cap := sessionPoolMax * 2
	if cap < 2 {
		cap = 2
	}
	return RepairConfig{
		QueueCapacity:    cap,
		InitialBackoff:   100 * time.Millisecond,
		MaxBackoff:       5 * time.Second,
		MaxLifetime:      30 * time.Second,
		ShutdownTimeout:  5 * time.Second,
		SyncRetryTimeout: 50 * time.Millisecond,
		TickInterval:     50 * time.Millisecond,
	}
}

type Repairer struct {
	store Store
	cfg   RepairConfig
	log   *slog.Logger

	mu        sync.Mutex
	intents   map[string]*RepairIntent
	abandoned map[string]struct{}
	enqueued  int64
	succeeded int64
	timedOut  int64
	dropped   int64

	stopCh       chan struct{}
	shutdownDone chan struct{}
	wg           sync.WaitGroup
	start        sync.Once
	stop         sync.Once
	closed       atomic.Bool

	hookMu      sync.RWMutex
	successHook func(RepairIntent)
}

func NewRepairer(store Store, cfg RepairConfig, log *slog.Logger) *Repairer {
	if log == nil {
		log = slog.Default()
	}
	if cfg.QueueCapacity < 2 {
		cfg.QueueCapacity = 2
	}
	return &Repairer{
		store:        store,
		cfg:          cfg,
		log:          log.With("component", "repairer"),
		intents:      make(map[string]*RepairIntent),
		abandoned:    make(map[string]struct{}),
		stopCh:       make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
}

func (r *Repairer) Start(ctx context.Context) {
	r.start.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed.Load() {
			return
		}
		r.wg.Add(1)
		go r.loop(ctx)
	})
}

// SetSuccessHook installs a callback invoked after a repair has durably
// succeeded and been removed from the backlog. The hook runs outside Repairer
// locks; callers must return promptly and offload longer work themselves.
func (r *Repairer) SetSuccessHook(hook func(RepairIntent)) {
	r.hookMu.Lock()
	r.successHook = hook
	r.hookMu.Unlock()
}

func (r *Repairer) notifySuccess(intent RepairIntent) {
	r.hookMu.RLock()
	hook := r.successHook
	r.hookMu.RUnlock()
	if hook != nil {
		hook(intent)
	}
}

func (r *Repairer) Enqueue(intent RepairIntent) {
	intent.enqueuedAt = time.Now()
	intent.nextAttempt = time.Now()

	key := intent.ExecutionID + ":" + intent.Kind.String()

	r.mu.Lock()
	if existing, ok := r.intents[key]; ok {
		if r.isMoreTerminal(existing, &intent) {
			r.mu.Unlock()
			return
		}
	} else if len(r.intents) >= r.cfg.QueueCapacity {
		atomic.AddInt64(&r.dropped, 1)
		if intent.Kind == RepairRuntime {
			r.abandoned[intent.ExecutionID] = struct{}{}
		}
		r.mu.Unlock()
		observability.RepairDropped().Add(context.Background(), 1)
		r.log.Warn("repair intent dropped, queue full",
			"execution_id", intent.ExecutionID, "kind", intent.Kind.String(),
			"capacity", r.cfg.QueueCapacity)
		return
	}
	if intent.Kind == RepairRuntime {
		delete(r.abandoned, intent.ExecutionID)
	}
	r.intents[key] = &intent
	count := len(r.intents)
	r.mu.Unlock()

	atomic.AddInt64(&r.enqueued, 1)
	r.log.Debug("repair intent enqueued",
		"execution_id", intent.ExecutionID, "kind", intent.Kind.String(),
		"backlog", count)
}

func (r *Repairer) isMoreTerminal(old, new *RepairIntent) bool {
	if old.Kind != new.Kind {
		return false
	}
	if old.Kind == RepairRuntime {
		oldTerminal := old.Status == string(RuntimeCompleted) || old.Status == string(RuntimeFailed)
		newNonTerminal := new.Status == string(RuntimeUnknown)
		return oldTerminal && newNonTerminal
	}
	return false
}

func (r *Repairer) Lookup(executionID string) (status, kind string, found bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, intent := range r.intents {
		if intent.ExecutionID == executionID {
			return intent.Status, intent.Kind.String(), true
		}
	}
	return "", "", false
}

func (r *Repairer) Backlog() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.intents)
}

func (r *Repairer) loop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.processDue(ctx)
		}
	}
}

func (r *Repairer) processDue(ctx context.Context) {
	now := time.Now()
	var due []*RepairIntent

	r.mu.Lock()
	for key, intent := range r.intents {
		if now.Sub(intent.enqueuedAt) > r.cfg.MaxLifetime {
			delete(r.intents, key)
			if intent.Kind == RepairRuntime {
				r.abandoned[intent.ExecutionID] = struct{}{}
			}
			atomic.AddInt64(&r.timedOut, 1)
			observability.RepairTimeout().Add(ctx, 1)
			r.log.Warn("repair intent timed out",
				"execution_id", intent.ExecutionID, "kind", intent.Kind.String(),
				"attempts", intent.attempts)
			continue
		}
		if !now.Before(intent.nextAttempt) {
			due = append(due, intent)
		}
	}
	r.mu.Unlock()

	for _, intent := range due {
		r.tryProcess(ctx, intent)
	}
}

func (r *Repairer) tryProcess(ctx context.Context, intent *RepairIntent) {
	intent.attempts++
	observability.RepairAttempts().Add(ctx, 1)

	err := r.processIntent(ctx, intent)
	if err == nil {
		key := intent.ExecutionID + ":" + intent.Kind.String()
		r.mu.Lock()
		if current, ok := r.intents[key]; ok && current == intent {
			delete(r.intents, key)
			if intent.Kind == RepairRuntime {
				delete(r.abandoned, intent.ExecutionID)
			}
		}
		count := len(r.intents)
		r.mu.Unlock()
		atomic.AddInt64(&r.succeeded, 1)
		observability.RepairSuccess().Add(ctx, 1)
		r.log.Debug("repair intent succeeded",
			"execution_id", intent.ExecutionID, "attempts", intent.attempts,
			"backlog", count)
		r.notifySuccess(*intent)
		return
	}

	r.log.Debug("repair attempt failed",
		"execution_id", intent.ExecutionID, "attempt", intent.attempts, "err", err)

	backoff := r.cfg.InitialBackoff << uint(intent.attempts)
	if backoff > r.cfg.MaxBackoff {
		backoff = r.cfg.MaxBackoff
	}
	if backoff < r.cfg.InitialBackoff {
		backoff = r.cfg.InitialBackoff
	}
	intent.nextAttempt = time.Now().Add(backoff)
}

// AbandonedExecutionIDs returns active executions whose terminal repair was
// dropped or exceeded its retry lifetime. Lease renewal excludes these IDs so
// database recovery can eventually converge them to unknown+fenced.
func (r *Repairer) AbandonedExecutionIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.abandoned))
	for executionID := range r.abandoned {
		ids = append(ids, executionID)
	}
	return ids
}

// ClearAbandonedExecutionIDs releases renewal exclusions after the store has
// confirmed that the corresponding executions are no longer active.
func (r *Repairer) ClearAbandonedExecutionIDs(executionIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, executionID := range executionIDs {
		delete(r.abandoned, executionID)
	}
}

func (r *Repairer) processIntent(ctx context.Context, intent *RepairIntent) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	switch intent.Kind {
	case RepairDelivery:
		return r.store.SetDelivery(ctx, intent.ExecutionID, intent.OwnerID, Status(intent.Status), intent.ErrorCode)
	case RepairRuntime:
		return r.store.FinishRuntime(ctx, intent.ExecutionID, intent.WorkerRunID, RuntimeStatus(intent.Status), intent.ErrorCode)
	default:
		return fmt.Errorf("unknown repair kind: %d", intent.Kind)
	}
}

func (r *Repairer) Shutdown(ctx context.Context) {
	shutdownCtx, cancel := context.WithTimeout(ctx, r.cfg.ShutdownTimeout)
	defer cancel()
	r.stop.Do(func() {
		// Serialize Start's WaitGroup registration with shutdown. Closing before
		// waiting also prevents a late Start from adding another processing loop.
		r.mu.Lock()
		r.closed.Store(true)
		close(r.stopCh)
		r.mu.Unlock()

		// Once starts one owner, but never holds its lock during callbacks.
		go r.finishShutdown(shutdownCtx)
	})
	select {
	case <-r.shutdownDone:
	case <-shutdownCtx.Done():
	}
}

func (r *Repairer) finishShutdown(ctx context.Context) {
	defer close(r.shutdownDone)
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	// Intent retry fields belong to the processing loop. Drain only after
	// that loop releases ownership; timeout never starts a second processor.
	select {
	case <-done:
		r.drain(ctx)
	case <-ctx.Done():
	}
	if ctx.Err() != nil && r.Backlog() > 0 {
		r.log.Warn("repairer shutdown timed out",
			"backlog", r.Backlog(), "timeout", r.cfg.ShutdownTimeout)
	}
}

func (r *Repairer) drain(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		r.processDue(ctx)
		if r.Backlog() == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Repairer) Stats() (enqueued, succeeded, timedOut, dropped int64) {
	return atomic.LoadInt64(&r.enqueued),
		atomic.LoadInt64(&r.succeeded),
		atomic.LoadInt64(&r.timedOut),
		atomic.LoadInt64(&r.dropped)
}
