package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/gateway"
	"github.com/hrygo/hotplex/internal/messaging"
)

// adapterLookup resolves a platform's receipt-capable sender from adapters
// that start after the cron scheduler is wired.
//
// It is mutex-guarded because the scheduler's timer loop may resolve a sender
// while startup is still publishing the adapter list.
type adapterLookup struct {
	mu       sync.RWMutex
	adapters []messaging.PlatformAdapterInterface
}

func (l *adapterLookup) set(adapters ...messaging.PlatformAdapterInterface) {
	l.mu.Lock()
	l.adapters = adapters
	l.mu.Unlock()
}

// senderFor returns the adapter for a platform only if it can return a
// receipt. An adapter without the controlled path is deliberately not
// returned: routing a gateway-mode job to a sender that cannot report what
// happened would silently downgrade the guarantee.
func (l *adapterLookup) senderFor(platform string) (messaging.ControlledSender, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, a := range l.adapters {
		if string(a.Platform()) != platform {
			continue
		}
		sender, ok := a.(messaging.ControlledSender)
		return sender, ok
	}
	return nil, false
}

// newCronEffectDelivery builds the gateway-owned delivery path for cron jobs
// that recorded DeliveryModeGateway.
//
// It returns nil when no effect store is available, which leaves those jobs
// undelivered rather than falling back to a path that cannot record an
// intent — a silent downgrade would be worse than a visible gap.
func newCronEffectDelivery(
	log *slog.Logger,
	stores *gatewayStores,
	ownerInstanceID string,
	owners *adapterLookup,
	occurrences cron.OccurrenceStore,
	jobs cron.Store,
	store effect.Store,
) cron.EffectDelivery {
	if store == nil {
		return nil
	}

	flush := func() {
		if stores.collector != nil {
			if err := stores.collector.Flush(); err != nil {
				log.Warn("cron effect delivery: flush before query", "err", err)
			}
		}
	}

	return gateway.NewEffectDeliverer(gateway.EffectDelivererConfig{
		Log:     log,
		Effects: store,
		SenderFor: func(platform string) (messaging.ControlledSender, bool) {
			return owners.senderFor(platform)
		},
		ResolveTarget: func(
			ctx context.Context, occurrenceID string,
		) (string, map[string]string, error) {
			return resolveDeliveryTarget(ctx, occurrences, jobs, occurrenceID)
		},
		OwnerInstanceID: ownerInstanceID,
		Extract: func(ctx context.Context, sessionID, executionID string) (string, error) {
			if stores.turnQuerier == nil {
				return "", gateway.ErrNoFinalOutput
			}
			flush()
			// Latest-turns scoping keeps the selection inside the current
			// generation: a cron session is created per occurrence, so the
			// newest generation is this run's, and an earlier generation's
			// answer can never be published as if it were current.
			turns, err := stores.turnQuerier.QueryLatestTurns(ctx, sessionID, gateway.FinalOutputLookback)
			if err != nil {
				return "", err
			}
			return gateway.SelectFinalAssistantOutput(turns)
		},
	})
}

// resolveDeliveryTarget re-authorizes an effect against the job as it exists
// now.
//
// Recovery must not deliver on the strength of what was true when the intent
// was planned. A deleted job, a job switched back to the legacy owner, or a
// disabled job all mean the delivery is no longer authorized, and the effect
// stays owed and visible instead of being sent or silently dropped.
func resolveDeliveryTarget(
	ctx context.Context,
	occurrences cron.OccurrenceStore,
	jobs cron.Store,
	occurrenceID string,
) (string, map[string]string, error) {
	if occurrences == nil || jobs == nil {
		return "", nil, fmt.Errorf("%w: delivery state unavailable", gateway.ErrTargetUnauthorized)
	}
	occ, err := occurrences.GetByID(ctx, occurrenceID)
	if err != nil {
		return "", nil, fmt.Errorf("%w: occurrence %s", gateway.ErrTargetUnauthorized, occurrenceID)
	}
	job, err := jobs.Get(ctx, occ.JobID)
	if err != nil {
		return "", nil, fmt.Errorf("%w: job %s", gateway.ErrTargetUnauthorized, occ.JobID)
	}
	if cron.ResolveDeliveryMode(job.DeliveryMode) != cron.DeliveryModeGateway {
		return "", nil, fmt.Errorf("%w: job %s is no longer gateway-owned",
			gateway.ErrTargetUnauthorized, job.ID)
	}
	if job.Silent {
		return "", nil, fmt.Errorf("%w: job %s is silent", gateway.ErrTargetUnauthorized, job.ID)
	}
	return job.Platform, job.PlatformKey, nil
}

// effectStoreFor returns the effect ledger for the active dialect, sharing the
// gateway's connection and write mutex.
func effectStoreFor(log *slog.Logger, stores *gatewayStores) effect.Store {
	var store effect.Store
	if stores.dialect == "postgres" && stores.db != nil {
		store = effect.NewPGStore(stores.db, log)
	} else if stores.sqlDB != nil {
		store = effect.NewSQLiteStore(stores.sqlDB, log, stores.writeMu)
	}
	if configurable, ok := store.(interface {
		SetRetentionPolicy(time.Duration, time.Duration, string)
	}); ok && stores.lifecycle.Policy == config.LifecyclePolicyV2 {
		configurable.SetRetentionPolicy(stores.lifecycle.EffectPayload.RetentionAfterSettlement,
			stores.lifecycle.Facts.RetentionAfterSettlement, config.LifecyclePolicyRevision(stores.lifecycle))
	}
	return store
}
