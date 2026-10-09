package worker

import (
	"context"
	"errors"
	"sync"
)

// SessionCleanupFunc removes a worker-runtime session after its owning
// HotPlex session has been explicitly deleted. It must be idempotent because
// a deleted session can later be physically removed from the local store.
type SessionCleanupFunc func(context.Context, string) error

var (
	// ErrSessionCleanupUnsupported means a native session exists but its
	// worker has no deletion capability. It must not be reported as deleted.
	ErrSessionCleanupUnsupported = errors.New("worker: session cleanup unsupported")

	sessionCleanupMu sync.RWMutex
	sessionCleaners  = make(map[WorkerType]SessionCleanupFunc)
)

// RegisterSessionCleanup associates a worker type with its persistent-session
// cleanup implementation. Workers without persistent remote sessions do not
// register a cleaner.
func RegisterSessionCleanup(t WorkerType, cleanup SessionCleanupFunc) {
	if cleanup == nil {
		panic("worker: session cleanup is nil")
	}
	sessionCleanupMu.Lock()
	defer sessionCleanupMu.Unlock()
	if _, exists := sessionCleaners[t]; exists {
		panic("worker: session cleanup registered twice for type " + string(t))
	}
	sessionCleaners[t] = cleanup
}

// CleanupSession removes the worker-runtime session for an explicitly deleted
// HotPlex session. Empty IDs have no remote deletion obligation.
func CleanupSession(ctx context.Context, t WorkerType, workerSessionID string) error {
	if workerSessionID == "" {
		return nil
	}
	sessionCleanupMu.RLock()
	cleanup := sessionCleaners[t]
	sessionCleanupMu.RUnlock()
	if cleanup == nil {
		return ErrSessionCleanupUnsupported
	}
	return cleanup(ctx, workerSessionID)
}
