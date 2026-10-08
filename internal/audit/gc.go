// Package audit — gc.go implements retention garbage collection with checkpoint rebase.
// See spec §5.5 (checkpoint rebase) and §5.7 (retention GC).
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hrygo/hotplex/internal/observability"
)

// GCConfig holds GC tunables.
type GCConfig struct {
	Retention time.Duration // default 3 years
	Interval  time.Duration // default 1 hour
}

func (c *GCConfig) defaults() {
	if c.Retention <= 0 {
		c.Retention = 3 * 365 * 24 * time.Hour // ~3 years
	}
	if c.Interval <= 0 {
		c.Interval = 1 * time.Hour
	}
}

// GC prunes old audit rows and writes a checkpoint before each prune.
type GC struct {
	store Store
	cfg   GCConfig
	log   *slog.Logger

	mu sync.Mutex // guards cfg.Retention against concurrent UpdateRetention/Tick
}

// NewGC creates a GC with the given store, config, and logger.
// Zero-value config fields are filled with defaults.
func NewGC(store Store, cfg GCConfig, log *slog.Logger) *GC {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	return &GC{store: store, cfg: cfg, log: log}
}

// UpdateRetention atomically swaps the retention window. Safe to call
// concurrently with Tick; the next Tick observes the new value. Called
// from the config hot-reload callback (spec §8 lists retention as
// hot-reloadable). A non-positive duration is ignored (defensive).
func (g *GC) UpdateRetention(d time.Duration) {
	if d <= 0 {
		return
	}
	g.mu.Lock()
	g.cfg.Retention = d
	g.mu.Unlock()
}

// Retention returns the currently effective retention window. Useful for
// tests and observability.
func (g *GC) Retention() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg.Retention
}

// Run launches a background ticker that calls Tick on each interval.
// Returns when ctx is cancelled.
func (g *GC) Run(ctx context.Context) {
	ticker := time.NewTicker(g.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := g.Tick(ctx); err != nil {
				g.log.Error("audit gc: tick failed", "err", err)
			}
		}
	}
}

// Tick performs one GC pass. Returns (deleted, err).
//
// Algorithm per spec section 5.5/5.7, executed as a SINGLE transaction via
// deleteBeforeTx: find the highest-id row with ts < cutoff, anchor it with
// a checkpoint, delete the prefix, and (when the table empties) rewrite
// the checkpoint with LastSelfHash="" so the next append is genesis.
// Running the whole prune under one Tx closes the C1/C2 race: previously
// the find → checkpoint → delete → checkpoint ran as separate store calls,
// each acquiring and releasing the writer lock independently. A concurrent
// flushBatch could insert a row between the checkpoint and the delete whose
// self_hash became part of the chain but whose row was then pruned, breaking
// the chain and producing a false-positive tamper alert. Now no other writer
// can interleave between the anchor read and the prune commit. The anchor
// must also precede the DELETE in the same Tx — trg_ua_no_delete (migration
// 030) rejects any DELETE that is not checkpoint-anchored.
func (g *GC) Tick(ctx context.Context) (int64, error) {
	g.mu.Lock()
	retention := g.cfg.Retention
	g.mu.Unlock()
	cutoff := time.Now()
	usesPersistedExpiry := false
	if expiryStore, ok := g.store.(interface{ UsesPersistedExpiry() bool }); ok {
		usesPersistedExpiry = expiryStore.UsesPersistedExpiry()
	}
	if !usesPersistedExpiry {
		cutoff = cutoff.Add(-retention)
	}

	tx, err := g.store.BeginTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit gc: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	deleted, cp, err := deleteBeforeTx(ctx, tx, cutoff)
	if err != nil {
		return 0, err
	}
	if deleted == 0 && cp.NextID == 0 {
		return 0, nil // nothing to prune
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("audit gc: commit: %w", err)
	}
	committed = true

	g.log.Info("audit gc: pruned",
		"deleted", deleted,
		"checkpoint_next_id", cp.NextID,
		"last_self_hash", cp.LastSelfHash,
	)
	kind := observability.LifecycleGCProcessedAuditFactLegacy
	if usesPersistedExpiry {
		kind = observability.LifecycleGCProcessedAuditFactV2
	}
	observability.RecordLifecycleGCProcessed(ctx, kind, deleted)
	return deleted, nil
}
