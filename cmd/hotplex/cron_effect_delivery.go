package main

import (
	"context"
	"log/slog"
	"sync"

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
) cron.EffectDelivery {
	store := effectStoreFor(log, stores)
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

// effectStoreFor returns the effect ledger for the active dialect, sharing the
// gateway's connection and write mutex.
func effectStoreFor(log *slog.Logger, stores *gatewayStores) effect.Store {
	if stores.dialect == "postgres" && stores.db != nil {
		return effect.NewPGStore(stores.db, log)
	}
	if stores.sqlDB != nil {
		return effect.NewSQLiteStore(stores.sqlDB, log, stores.writeMu)
	}
	return nil
}
