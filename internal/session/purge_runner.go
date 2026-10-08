package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/hrygo/hotplex/internal/observability"
)

// PurgeContentExecutor removes the content owned by one HotPlex session.
type PurgeContentExecutor func(context.Context, string) error

// PurgeItemRunner leases and executes durable conversation-content cleanup.
type PurgeItemRunner struct {
	log     *slog.Logger
	store   PurgeItemStore
	execute PurgeContentExecutor
	now     func() time.Time
}

func NewPurgeItemRunner(log *slog.Logger, store PurgeItemStore, execute PurgeContentExecutor) *PurgeItemRunner {
	if log == nil {
		log = slog.Default()
	}
	return &PurgeItemRunner{
		log:     log.With("component", "session_content_purge"),
		store:   store,
		execute: execute,
		now:     time.Now,
	}
}

// Run drains due work at startup and then polls until ctx ends.
func (r *PurgeItemRunner) Run(ctx context.Context) {
	if r == nil || r.store == nil || r.execute == nil {
		return
	}
	r.RunOnce(ctx)
	ticker := time.NewTicker(cleanupPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce executes one leased batch. It is exported for deterministic tests.
func (r *PurgeItemRunner) RunOnce(ctx context.Context) {
	if r == nil || r.store == nil || r.execute == nil {
		return
	}
	now := r.now()
	tasks, err := r.store.ClaimPurgeItems(ctx, now, now.Add(cleanupLeaseDuration), cleanupBatchSize)
	if err != nil {
		r.log.Warn("session purge: claim content items failed", "err", err)
		return
	}
	for _, task := range tasks {
		attemptCtx, cancel := context.WithTimeout(ctx, cleanupAttemptTimeout)
		err := r.execute(attemptCtx, task.SessionID)
		cancel()
		if err == nil {
			if completeErr := r.store.CompletePurgeItem(ctx, task.ID, task.LeaseToken); completeErr != nil {
				r.log.Warn("session purge: complete content item failed", "item_id", task.ID, "session_id", task.SessionID, "err", completeErr)
			} else {
				observability.RecordLifecycleGCProcessed(ctx, observability.LifecycleGCProcessedConversationPurgeItem, 1)
			}
			continue
		}
		next := r.now().Add(cleanupBackoff(task.Attempts))
		if retryErr := r.store.RetryPurgeItem(ctx, task.ID, task.LeaseToken, next, "cleanup_failed"); retryErr != nil {
			r.log.Warn("session purge: schedule content retry failed", "item_id", task.ID, "session_id", task.SessionID, "err", retryErr)
			continue
		}
		// Provider errors may contain identifiers or content; retain only a
		// stable error code in storage and logs.
		r.log.Warn("session purge: content deletion failed; retry scheduled",
			"item_id", task.ID, "session_id", task.SessionID,
			"attempt", task.Attempts, "next_attempt_at", next, "error_code", "cleanup_failed")
	}
}
