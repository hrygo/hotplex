package main

import (
	"context"
	"errors"

	"github.com/hrygo/hotplex/internal/admin"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/gateway"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

type sessionManagerAdapter struct {
	sm *session.Manager
}

func (a *sessionManagerAdapter) Stats() (int, int, int) {
	return a.sm.Stats()
}

func (a *sessionManagerAdapter) List(ctx context.Context, userID, platform string, limit, offset int) ([]any, error) {
	sessions, err := a.sm.List(ctx, userID, platform, "", limit, offset)
	if err != nil {
		return nil, err
	}
	result := make([]any, len(sessions))
	for i, s := range sessions {
		result[i] = s
	}
	return result, nil
}

func (a *sessionManagerAdapter) Get(ctx context.Context, id string) (any, error) {
	return a.sm.Get(ctx, id)
}

func (a *sessionManagerAdapter) Delete(ctx context.Context, id string) error {
	return a.sm.Delete(ctx, id)
}

func (a *sessionManagerAdapter) WorkerHealthStatuses() []worker.WorkerHealth {
	return a.sm.WorkerHealthStatuses()
}

func (a *sessionManagerAdapter) DebugSnapshot(id string) (admin.DebugSessionSnapshot, bool) {
	snap, ok := a.sm.DebugSnapshot(id)
	if !ok {
		return admin.DebugSessionSnapshot{}, false
	}
	return admin.DebugSessionSnapshot{
		TurnCount:    snap.TurnCount,
		WorkerHealth: snap.WorkerHealth,
		HasWorker:    snap.HasWorker,
	}, true
}

func (a *sessionManagerAdapter) Transition(ctx context.Context, id string, to events.SessionState) error {
	return a.sm.Transition(ctx, id, to)
}

func (a *sessionManagerAdapter) DeletePhysical(ctx context.Context, id string) error {
	return a.sm.DeletePhysical(ctx, id)
}

func (a *sessionManagerAdapter) ResetExpiry(ctx context.Context, id string) error {
	return a.sm.ResetExpiry(ctx, id)
}

type hubAdapter struct {
	hub *gateway.Hub
}

func (a *hubAdapter) ConnectionsOpen() int {
	return a.hub.ConnectionsOpen()
}

func (a *hubAdapter) NextSeqPeek(sessionID string) int64 {
	return a.hub.NextSeqPeek(sessionID)
}

type turnsStoreAdapter struct {
	es eventstore.TurnQuerier
}

func (a *turnsStoreAdapter) TurnStats(ctx context.Context, sessionID string) (*eventstore.TurnStats, error) {
	return a.es.QueryTurnStats(ctx, sessionID)
}

func (a *turnsStoreAdapter) LatestSeq(ctx context.Context, sessionID string) (int64, error) {
	return a.es.LatestSeq(ctx, sessionID)
}

type bridgeAdapter struct {
	bridge *gateway.Bridge
}

func (a *bridgeAdapter) StartSession(ctx context.Context, p worker.SessionStartParams) error {
	return a.bridge.StartSession(ctx, p)
}

type configAdapter struct {
	cfgStore *config.ConfigStore
}

func (a *configAdapter) Get() *config.Config {
	return a.cfgStore.Load()
}

type configWatcherAdapter struct {
	watcher *config.Watcher
}

func (a *configWatcherAdapter) Rollback(version int) (*config.Config, int, error) {
	if a.watcher == nil {
		return nil, -1, errors.New("config watcher is nil")
	}
	return a.watcher.Rollback(version)
}

type botListerAdapter struct {
	registry *messaging.BotRegistry
}

func toAdminBotEntry(e *messaging.BotEntry) admin.BotEntry {
	botID := e.BotID
	displayName := e.Name
	if displayName == "" {
		// Single-bot mode: auto-generated Name is empty. Provide a human-readable
		// fallback so the admin UI shows "slack (default)" instead of just "slack:".
		displayName = string(e.Platform) + " (default)"
	}
	if botID == "" {
		// Fallback when platform BotID is not yet available (adapter not started).
		// Use platform:name to guarantee uniqueness across platforms.
		botID = string(e.Platform) + ":" + e.Name
	}
	return admin.BotEntry{
		Name:        displayName,
		Platform:    string(e.Platform),
		BotID:       botID,
		Status:      string(e.Status),
		ConnectedAt: e.ConnectedAt.Format("2006-01-02T15:04:05Z"),
		WorkerType:  e.WorkerType,
	}
}

func (a *botListerAdapter) ListBots() []admin.BotEntry {
	entries := a.registry.ListAll()
	result := make([]admin.BotEntry, len(entries))
	for i, e := range entries {
		result[i] = toAdminBotEntry(e)
	}
	return result
}

func (a *botListerAdapter) GetBot(name string) (*admin.BotEntry, bool) {
	e, ok := a.registry.GetByName(name)
	if !ok {
		return nil, false
	}
	entry := toAdminBotEntry(e)
	return &entry, true
}

// executionProviderAdapter exposes the durable-ingress store to the Admin API
// for operator fence decisions (#877). Pass-through only — the store owns all
// conditional-update semantics.
type executionProviderAdapter struct {
	store execution.Store
}

func (a *executionProviderAdapter) ListFences(ctx context.Context, sessionID string, limit, offset int) ([]*execution.Record, error) {
	return a.store.ListFences(ctx, sessionID, limit, offset)
}

func (a *executionProviderAdapter) ApplyFenceDecision(ctx context.Context, request execution.FenceActionRequest) (*execution.Record, error) {
	return a.store.ApplyFenceDecision(ctx, request)
}

// effectProviderAdapter exposes the delivery ledger to the Admin API for the
// operator effect console. Pass-through only — the store owns every
// conditional-update semantic, so the adapter cannot introduce a second way to
// move an effect.
type effectProviderAdapter struct {
	store effect.Store
}

func (a *effectProviderAdapter) ListForOperator(
	ctx context.Context, f effect.OperatorListFilter,
) ([]*effect.Effect, error) {
	return a.store.ListForOperator(ctx, f)
}

func (a *effectProviderAdapter) GetByID(ctx context.Context, effectID string) (*effect.Effect, error) {
	return a.store.GetByID(ctx, effectID)
}

func (a *effectProviderAdapter) ListAttempts(
	ctx context.Context, effectID string,
) ([]*effect.Attempt, error) {
	return a.store.ListAttempts(ctx, effectID)
}

func (a *effectProviderAdapter) ApplyOperatorAction(
	ctx context.Context, req effect.OperatorActionRequest,
) (*effect.Effect, error) {
	return a.store.ApplyOperatorAction(ctx, req)
}

// runtimeEventNotifier emits the additive runtime.execution.failed event with
// OPERATOR_ABANDONED after an abandon decision, so connected clients observe
// the terminal state. Best-effort by contract: fenced sessions rarely have
// live connections and the store write is already durable.
type runtimeEventNotifier struct {
	hub *gateway.Hub
}

func (n *runtimeEventNotifier) NotifyExecutionAbandoned(ctx context.Context, sessionID, executionID string) {
	if n.hub == nil {
		return
	}
	seq := n.hub.NextSeq(sessionID)
	if seq == 0 {
		return
	}
	env := events.NewEnvelope(aep.NewID(), sessionID, seq, events.RuntimeExecutionFailed, events.RuntimeExecutionData{
		ExecutionID: executionID,
		Status:      string(execution.RuntimeFailed),
		ErrorCode:   events.ErrCodeOperatorAbandoned,
	})
	_ = n.hub.SendToSession(ctx, env)
}
