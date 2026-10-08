package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/observability"
)

// CollectorConfig holds the tunables for the audit collector.
type CollectorConfig struct {
	ChannelCap        int
	BatchSize         int
	BatchInterval     time.Duration
	SinkTimeout       time.Duration
	SpillBlockTimeout time.Duration
	FactsRetention    time.Duration
}

func (c *CollectorConfig) defaults() {
	if c.ChannelCap <= 0 {
		c.ChannelCap = 4096
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.BatchInterval <= 0 {
		c.BatchInterval = 1 * time.Second
	}
	if c.SinkTimeout <= 0 {
		c.SinkTimeout = 5 * time.Second
	}
	if c.SpillBlockTimeout <= 0 {
		c.SpillBlockTimeout = 5 * time.Second
	}
	if c.FactsRetention <= 0 {
		c.FactsRetention = 180 * 24 * time.Hour
	}
}

// Collector is the zero-loss audit event writer. It batches events into
// a single transaction (so the hash chain is consistent within a batch),
// fans out to AlertSinks after commit, and spills to disk on backpressure.
// "Zero-loss" is bounded: events survive channel overflow, DB flush
// failures (regular and spill-drain), and graceful shutdown — but a
// spill write failure or the no-spill bounded-block fallback counts the
// event as dropped (visible via Dropped()/metrics) instead of losing it
// silently.
type Collector struct {
	store Store
	spill *SpillFile
	sinks []AlertSink
	log   *slog.Logger
	cfg   CollectorConfig
	epoch string

	captureC  chan *UserActivity
	closeC    chan struct{}
	closeWg   sync.WaitGroup
	closeOnce sync.Once
	startMu   sync.Mutex
	started   bool

	sinkWorkers   []*sinkWorker
	sinkWg        sync.WaitGroup
	sinkStopOnce  sync.Once
	sinksDone     chan struct{}
	sinkCtx       context.Context
	sinkCancel    context.CancelFunc
	sinkCloseOnce sync.Once
	sinkCloseErr  error
	sinkCloseMu   sync.Mutex

	// lifecycleCtx is set by Start and used for normal (ticker/batch-full/
	// sentinel) flushes. shutdownCtx is set by Close and used for the final
	// flush so a slow DB during shutdown can't outlive the gateway's
	// shutdown deadline (review I5: previously flushBatch minted a fresh
	// context.Background() with a hard 30s, ignoring the shutdown ctx).
	lifecycleCtx context.Context
	shutdownCtx  context.Context

	mu       sync.Mutex // serializes flushBatch and drainSpill
	enqueued atomic.Int64
	spilled  atomic.Int64
	dropped  atomic.Int64
}

type sinkWorker struct {
	sink  AlertSink
	queue chan AuditEvent
}

// spillSentinel is a sentinel *UserActivity value that tells runWriter to drain
// the spill file on the next iteration.
var spillSentinel = &UserActivity{UserID: "__audit_spill_marker__"}

// NewCollector creates a new Collector. spill may be nil for tests.
// sinks may be nil or empty — fanOutSinks is a no-op in that case.
func NewCollector(store Store, spill *SpillFile, sinks []AlertSink, log *slog.Logger, cfg CollectorConfig) *Collector {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	epoch := legacyChainProfile.epoch
	if profile, ok := store.(interface{ ChainEpoch() string }); ok {
		epoch = profile.ChainEpoch()
	}
	if sinks == nil {
		sinks = []AlertSink{}
	}
	workers := make([]*sinkWorker, 0, len(sinks))
	for _, sink := range sinks {
		workers = append(workers, &sinkWorker{
			sink:  sink,
			queue: make(chan AuditEvent, cfg.ChannelCap),
		})
	}
	return &Collector{
		store:       store,
		spill:       spill,
		sinks:       sinks,
		log:         log,
		cfg:         cfg,
		epoch:       epoch,
		captureC:    make(chan *UserActivity, cfg.ChannelCap),
		closeC:      make(chan struct{}),
		sinkWorkers: workers,
		sinksDone:   make(chan struct{}),
	}
}

// Start launches the runWriter goroutine. Call once before Enqueue.
// ctx is stored as the lifecycle context used for normal flushes (review I5);
// pass the gateway's long-lived context so a normal flush can't outlive it.
// A nil ctx falls back to context.Background() (test convenience).
func (c *Collector) Start(ctx context.Context) {
	c.startMu.Lock()
	if c.started {
		c.startMu.Unlock()
		return
	}
	c.started = true
	if ctx == nil {
		ctx = context.Background()
	}
	c.lifecycleCtx = ctx
	c.sinkCtx, c.sinkCancel = context.WithCancel(context.Background())
	for _, worker := range c.sinkWorkers {
		c.sinkWg.Add(1)
		go c.runSinkWorker(worker)
	}
	go func() {
		c.sinkWg.Wait()
		close(c.sinksDone)
	}()
	c.closeWg.Add(1)
	go c.runWriter()
	c.startMu.Unlock()
}

// Enqueue submits an audit event with zero-loss semantics:
//  1. Try non-blocking send to captureC
//  2. On full: spill to WAL (O_SYNC)
//  3. Last resort: bounded block (SpillBlockTimeout)
//
// Returns nil if accepted (in-memory or spill), error if all paths failed.
func (c *Collector) Enqueue(ctx context.Context, ua *UserActivity) error {
	if ua == nil {
		return fmt.Errorf("audit: nil UserActivity")
	}
	if c.epoch == lifecycleChainProfile.epoch {
		now := time.Now()
		if ua.Ts <= 0 {
			ua.Ts = now.UnixMilli()
		}
		ua.ChainEpoch = c.epoch
		ua.ExpiresAt = time.UnixMilli(ua.Ts).Add(c.cfg.FactsRetention).UnixMilli()
	}
	select {
	case <-c.closeC:
		return ErrCollectorClosed
	default:
	}

	select {
	case c.captureC <- ua:
		c.enqueued.Add(1)
		return nil
	default:
	}

	if c.spill != nil {
		rec := SpillRecord{TsMs: time.Now().UnixMilli(), UA: ua}
		if err := c.spill.Write(rec); err == nil {
			c.spilled.Add(1)
			observability.AuditSpill().Add(ctx, 1, metric.WithAttributes(attribute.String("action", "spill_ok")))
			select {
			case c.captureC <- spillSentinel:
			default:
			}
			return nil
		}
		observability.AuditSpill().Add(ctx, 1, metric.WithAttributes(attribute.String("action", "spill_failed")))
	}

	select {
	case c.captureC <- ua:
		c.enqueued.Add(1)
		return nil
	case <-time.After(c.cfg.SpillBlockTimeout):
		c.dropped.Add(1)
		return fmt.Errorf("audit collector: enqueue timeout after %v", c.cfg.SpillBlockTimeout)
	case <-c.closeC:
		return fmt.Errorf("audit collector: closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Enqueued returns the count of in-memory accepts.
func (c *Collector) Enqueued() int64 { return c.enqueued.Load() }

// Spilled returns the count of spill writes.
func (c *Collector) Spilled() int64 { return c.spilled.Load() }

// Dropped returns the count of drops (should always be 0).
func (c *Collector) Dropped() int64 { return c.dropped.Load() }

// Close signals the writer to stop, waits for it to drain and flush, waits
// for in-flight sink fan-out to finish, then closes the spill file. Safe to
// call multiple times.
//
// ctx is stored as the shutdown context and used for the final flush (review
// I5): pass the gateway's shutdown ctx so a slow DB during shutdown can't
// burn the full 30s budget past the gateway's deadline. A nil ctx falls back
// to the lifecycle context, then context.Background().
//
// Order: closeC → runWriter drains captureC + final flush (which may start
// the last fan-out) → closeWg → sinkWg (drain in-flight sinks) → spill close.
// Waiting on sinkWg before closing the spill file prevents the use-after-close
// race where a fan-out goroutine touches sink resources after Close returns
// (review I3).
func (c *Collector) Close(ctx context.Context) error {
	var spillErr error
	c.closeOnce.Do(func() {
		if ctx != nil {
			c.shutdownCtx = ctx
		}
		close(c.closeC)
	})
	c.startMu.Lock()
	if !c.started {
		c.started = true
		close(c.sinksDone)
	}
	c.startMu.Unlock()
	c.closeWg.Wait()
	c.sinkStopOnce.Do(func() {
		for _, worker := range c.sinkWorkers {
			close(worker.queue)
		}
	})
	waitCtx := ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	var waitErr error
	select {
	case <-c.sinksDone:
		c.closeSinkLifecycles(waitCtx)
	case <-waitCtx.Done():
		waitErr = waitCtx.Err()
		if c.sinkCancel != nil {
			c.sinkCancel()
		}
	}
	if c.sinkCancel != nil {
		c.sinkCancel()
	}
	if c.spill != nil {
		spillErr = c.spill.Close()
		c.spill = nil
	}
	c.sinkCloseMu.Lock()
	sinkErr := c.sinkCloseErr
	c.sinkCloseMu.Unlock()
	return errors.Join(waitErr, spillErr, sinkErr)
}

func (c *Collector) runSinkWorker(worker *sinkWorker) {
	defer c.sinkWg.Done()
	for event := range worker.queue {
		c.callSink(worker.sink, event)
	}
}

func (c *Collector) callSink(sink AlertSink, event AuditEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			observability.AuditSinkFailures().Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("sink", fmt.Sprintf("%T", sink)),
			))
			c.log.Error("audit: sink panic recovered",
				"sink", fmt.Sprintf("%T", sink),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
		}
	}()
	ctx := c.sinkCtx
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.SinkTimeout)
	defer cancel()
	if err := sink.OnAuditEvent(sctx, event); err != nil {
		observability.AuditSinkFailures().Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sink", fmt.Sprintf("%T", sink)),
		))
		c.log.Warn("audit: sink failed", "err", err, "event_id", event.EventID, "action", event.Action)
	}
}

func (c *Collector) closeSinkLifecycles(ctx context.Context) {
	c.sinkCloseOnce.Do(func() {
		var closeErr error
		for _, sink := range c.sinks {
			closer, ok := sink.(AlertSinkCloser)
			if !ok {
				continue
			}
			closeErr = errors.Join(closeErr, closer.Close(ctx))
		}
		c.sinkCloseMu.Lock()
		c.sinkCloseErr = closeErr
		c.sinkCloseMu.Unlock()
	})
}

func (c *Collector) runWriter() {
	defer c.closeWg.Done()
	ticker := time.NewTicker(c.cfg.BatchInterval)
	defer ticker.Stop()

	var batch []*UserActivity

	for {
		select {
		case <-c.closeC:
			// Shutdown path: use the shutdown context (set by Close) for the
			// final flush so it respects the gateway's shutdown deadline
			// rather than a detached context.Background() (review I5).
			ctx := c.shutdownCtx
			if ctx == nil {
				ctx = c.lifecycleCtx
			}
			c.mu.Lock()
			for {
				select {
				case ua := <-c.captureC:
					if ua == spillSentinel {
						continue
					}
					batch = append(batch, ua)
				default:
					goto finalFlush
				}
			}
		finalFlush:
			c.flushLocked(ctx, batch)
			batch = nil
			c.drainSpillLocked(ctx)
			c.mu.Unlock()
			return

		case ua := <-c.captureC:
			if ua == spillSentinel {
				c.mu.Lock()
				c.flushLocked(c.lifecycleCtx, batch)
				batch = batch[:0]
				c.drainSpillLocked(c.lifecycleCtx)
				c.mu.Unlock()
				continue
			}
			batch = append(batch, ua)
			if len(batch) >= c.cfg.BatchSize {
				c.mu.Lock()
				c.flushLocked(c.lifecycleCtx, batch)
				batch = batch[:0]
				c.mu.Unlock()
			}

		case <-ticker.C:
			c.mu.Lock()
			c.flushLocked(c.lifecycleCtx, batch)
			batch = batch[:0]
			c.drainSpillLocked(c.lifecycleCtx)
			c.mu.Unlock()
		}
	}
}

func (c *Collector) flushLocked(ctx context.Context, batch []*UserActivity) {
	if len(batch) == 0 {
		return
	}
	if err := c.flushBatch(ctx, batch); err != nil {
		// DB flush failed — re-spill the in-flight batch so no event is
		// lost (same zero-loss contract as the spill-drain path); the next
		// drain attempt replays them. Without this the regular flush path
		// silently dropped up to BatchSize events on a transient outage.
		c.reSpillUAsLocked(batch)
		c.log.Error("audit: flush batch failed, records re-spilled",
			"err", err, "size", len(batch))
	}
}

func (c *Collector) drainSpillLocked(ctx context.Context) {
	if c.spill == nil {
		return
	}
	// Drain atomically reads + truncates the spill file under a single
	// s.mu lock acquisition. This prevents the race where a concurrent
	// Write appends new records between ReadAll and Truncate (which would
	// lose those records). Spec section 5.10 zero-loss guarantee.
	records, err := c.spill.Drain()
	if err != nil {
		c.log.Error("audit: spill drain", "err", err)
		return
	}
	if len(records) == 0 {
		return
	}
	uas := make([]*UserActivity, 0, len(records))
	for _, r := range records {
		if r.UA != nil {
			uas = append(uas, r.UA)
		}
	}
	if err := c.flushBatch(ctx, uas); err != nil {
		// DB flush failed — the spill file was already truncated by Drain,
		// so re-spill the records we just read to preserve the zero-loss
		// guarantee (spec §5.10). Without this, a transient DB outage
		// during drain would permanently lose every spilled event.
		c.reSpillLocked(records)
		c.log.Error("audit: spill flush failed, records re-spilled", "err", err, "count", len(records))
		return
	}
}

// reSpillLocked appends the given records back to the spill file. It must
// be called with c.mu held (the caller — drainSpillLocked — already holds it).
// The spill file's own mutex serializes the writes against concurrent Enqueue.
func (c *Collector) reSpillLocked(records []SpillRecord) {
	for _, r := range records {
		if r.UA == nil {
			continue
		}
		c.reSpillRecordLocked(r)
	}
}

// reSpillUAsLocked writes the given events back to the spill file so a
// failed DB flush cannot lose them. Called with c.mu held (all flushLocked
// call sites hold it). With no spill file configured the events cannot be
// preserved — they are counted as dropped so the loss stays visible.
func (c *Collector) reSpillUAsLocked(uas []*UserActivity) {
	if c.spill == nil {
		c.dropped.Add(int64(len(uas)))
		return
	}
	for _, ua := range uas {
		if ua == nil {
			continue
		}
		c.reSpillRecordLocked(SpillRecord{TsMs: time.Now().UnixMilli(), UA: ua})
	}
}

// reSpillRecordLocked writes one record to the spill file; a write failure
// (disk full etc.) is the last resort and is counted as dropped so the
// loss stays visible to monitoring.
func (c *Collector) reSpillRecordLocked(r SpillRecord) {
	if c.spill == nil {
		c.dropped.Add(1)
		return
	}
	if err := c.spill.Write(r); err != nil {
		c.dropped.Add(1)
		c.log.Error("audit: re-spill write failed (record lost)", "err", err)
	}
}

func (c *Collector) flushBatch(ctx context.Context, uas []*UserActivity) error {
	if len(uas) == 0 {
		return nil
	}

	// Inherit from the caller's context (lifecycle for normal flushes,
	// shutdown for the final flush) rather than minting a detached
	// context.Background(). The 30s cap still bounds a single attempt, but
	// now a slow DB during shutdown is cut off by the gateway's shutdown
	// deadline instead of running past it (review I5).
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		observability.AuditWriteFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("action", "begin_tx")))
		return fmt.Errorf("audit: begin tx: %w", err)
	}
	// committedTx guards the deferred Rollback: once Commit succeeds it is
	// set true so the deferred Rollback is a no-op (tx.done also guards, but
	// the flag keeps the intent explicit and avoids relying on internal state).
	// The defer also guarantees writeMu release on panic between BeginTx and
	// Commit/Rollback — without it, a panic would leak the writeMu and
	// permanently deadlock the audit writer.
	committedTx := false
	defer func() {
		if !committedTx {
			_ = tx.Rollback() // Rollback is a no-op if already rolled back/committed
		}
	}()

	tail, err := tx.TailHash(ctx)
	if err != nil {
		return fmt.Errorf("audit: read tail: %w", err)
	}

	committed := make([]*UserActivity, 0, len(uas))
	for _, ua := range uas {
		ua.PrevHash = tail
		h, err := ComputeSelfHash(tail, ua)
		if err != nil {
			return fmt.Errorf("audit: compute hash: %w", err)
		}
		ua.SelfHash = h
		if err := tx.Append(ctx, ua); err != nil {
			observability.AuditWriteFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("action", "append")))
			return fmt.Errorf("audit: flush: %w", err)
		}
		committed = append(committed, ua)
		tail = h
	}

	if err := tx.Commit(); err != nil {
		observability.AuditWriteFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("action", "commit")))
		return fmt.Errorf("audit: commit: %w", err)
	}
	committedTx = true

	for _, ua := range committed {
		observability.AuditEvents().Add(context.Background(), 1,
			metric.WithAttributes(
				attribute.String("action", ua.Action),
				attribute.String("outcome", ua.Outcome),
			),
		)
	}

	c.fanOutSinks(committed)
	return nil
}

// fanOutSinks dispatches every committed event to every sink. Concurrency is
// bounded by the per-sink worker queue capacity (ChannelCap). If a queue is
// full, events are dropped for that sink to prevent blocking the main collector.
func (c *Collector) fanOutSinks(uas []*UserActivity) {
	for _, ua := range uas {
		ev := userActivityToAuditEvent(ua)
		for _, worker := range c.sinkWorkers {
			select {
			case worker.queue <- ev:
			default:
				observability.AuditSinkFailures().Add(context.Background(), 1, metric.WithAttributes(
					attribute.String("sink", fmt.Sprintf("%T", worker.sink)),
				))
				c.log.Warn("audit: sink queue full, event dropped",
					"sink", fmt.Sprintf("%T", worker.sink),
					"event_id", ev.EventID,
					"action", ev.Action,
				)
			}
		}
	}
}

func userActivityToAuditEvent(ua *UserActivity) AuditEvent {
	return AuditEvent{
		EventID:      newEventID(),
		Ts:           time.UnixMilli(ua.Ts),
		UserID:       ua.UserID,
		UserIDType:   ua.UserIDType,
		Platform:     ua.Platform,
		SessionID:    ua.SessionID,
		Action:       ua.Action,
		ResourceType: ua.ResourceType,
		ResourceID:   ua.ResourceID,
		Outcome:      ua.Outcome,
		Detail:       decodeDetail(ua.DetailJSON),
		EventRef:     ua.EventRef,
		IP:           ua.IP,
		UserAgent:    ua.UserAgent,
	}
}

// eventIDCounter is a per-process monotonic counter combined with the
// millisecond timestamp to guarantee uniqueness for two IDs generated in
// the same millisecond on the same process (RFC 9562 §6.2 "monotonic
// random" method). The counter wraps at 12 bits (rand_a field); within a
// single millisecond a single process can mint at most 4096 UUIDv7s
// before the counter would collide — far above the audit collector's
// realistic throughput.
var eventIDCounter atomic.Uint32

// newEventID generates a UUIDv7 per RFC 9562.
//
// Layout (big-endian, 128 bits):
//
//	xxxxxxxx-xxxx-7xxx-yxxx  (48-bit unix-ms | 4-bit ver=7 | 12-bit counter)
//	xxxxxxxxxxxx             (2-bit var=10 | 62-bit crypto-random)
//
// The 12-bit per-process counter guarantees uniqueness within a single
// millisecond on one process (up to 4096 IDs/ms). The 62 crypto-random
// bits in the trailing field give vanishingly low cross-process collision
// probability for IDs minted in the same millisecond.
//
// Spec §5.6 requires EventID to be UUIDv7 for sink dedup/idempotency.
func newEventID() string {
	var b [16]byte

	// 48-bit big-endian unix milliseconds (bytes 0..5).
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	// 12-bit monotonic counter into rand_a (low 12 bits of bytes 6..7).
	// Version nibble (0x7) occupies the high 4 bits of byte 6.
	ctr := uint16(eventIDCounter.Add(1) & 0x0FFF)
	b[6] = 0x70 | byte(ctr>>8)
	b[7] = byte(ctr)

	// Draw 62 crypto-random bits into bytes 8..15. We overwrite the top
	// 2 bits of byte 8 with the RFC 4122 variant (10) afterwards.
	//
	// crypto/rand should never fail on Linux/macOS/Windows in normal
	// operation; if it does, we fall back to whatever bytes happen to be
	// in b[8:] (zeros) and proceed anyway — a structurally-valid UUID with
	// weak randomness is preferable to dropping an audit event. The
	// monotonic counter still guarantees per-ms uniqueness.
	_, _ = rand.Read(b[8:])

	// Force the RFC 4122 variant bits (top two bits of byte 8 = 10).
	b[8] = (b[8] & 0x3F) | 0x80

	// Canonical 8-4-4-4-12 hex form with dashes.
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

func decodeDetail(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		// Malformed detail_json should never reach the store (callers
		// write valid JSON), but we must not let a sink fan-out panic
		// the collector. Return nil so the sink sees no detail rather
		// than crashing.
		return nil
	}
	return m
}

var ErrCollectorClosed = errors.New("audit: collector closed")
