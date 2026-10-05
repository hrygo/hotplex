package execution

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/pkg/events"
)

type flakyStore struct {
	*SQLStore
	mu    sync.Mutex
	fail  bool
	calls int64
}

type blockingRuntimeStore struct {
	*flakyStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type shutdownBlockingStore struct {
	Store
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

type shutdownHookStore struct{ Store }

func (s *shutdownHookStore) SetDelivery(context.Context, string, string, Status, string) error {
	return nil
}

func TestRepairer_ShutdownHookCanCallShutdownWithCancelledContext(t *testing.T) {
	t.Parallel()
	r := NewRepairer(&shutdownHookStore{}, fastRepairConfig(), nil)
	r.Enqueue(RepairIntent{ExecutionID: "hook", Kind: RepairDelivery, Status: string(StatusDelivered)})
	r.SetSuccessHook(func(RepairIntent) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.Shutdown(ctx)
	})
	done := make(chan struct{})
	go func() {
		r.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("success callback deadlocked while re-entering shutdown")
	}
}

func (s *shutdownBlockingStore) SetDelivery(ctx context.Context, _, _ string, _ Status, _ string) error {
	if s.calls.Add(1) != 1 {
		return errors.New("concurrent retry while the first attempt is blocked")
	}
	close(s.started)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRepairer_ShutdownDoesNotDrainWhileLoopIsProcessing(t *testing.T) {
	t.Parallel()
	store := &shutdownBlockingStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	cfg := fastRepairConfig()
	cfg.ShutdownTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRepairer(store, cfg, nil)
	t.Cleanup(func() {
		close(store.release)
		cancel()
		r.wg.Wait()
	})
	r.Start(ctx)
	r.Enqueue(RepairIntent{ExecutionID: "blocked", Kind: RepairDelivery, Status: string(StatusDelivered)})
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("background repair did not start")
	}

	r.Shutdown(context.Background())
	require.EqualValues(t, 1, store.calls.Load(), "shutdown must not process an in-flight intent a second time")
	require.Equal(t, 1, r.Backlog(), "a timed-out shutdown must leave the in-flight repair intact")
}

func TestRepairer_StartAfterShutdownDoesNotLaunchLoop(t *testing.T) {
	t.Parallel()
	r := NewRepairer(nil, fastRepairConfig(), nil)
	r.Shutdown(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.Start(ctx)
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start registered a processing loop after shutdown")
	}
}

func TestRepairer_ConcurrentShutdownDrainsOnce(t *testing.T) {
	t.Parallel()
	store := &flakyStore{}
	store.setFail(true)
	cfg := fastRepairConfig()
	cfg.ShutdownTimeout = 50 * time.Millisecond
	cfg.InitialBackoff = time.Second
	r := NewRepairer(store, cfg, nil)
	r.Enqueue(RepairIntent{ExecutionID: "pending", Kind: RepairDelivery, Status: string(StatusDelivered)})
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			<-start
			r.Shutdown(context.Background())
			done <- struct{}{}
		}()
	}
	close(start)
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent shutdown did not complete")
		}
	}
	require.EqualValues(t, 1, atomic.LoadInt64(&store.calls), "shutdown callers must share one drain")
}

func (s *blockingRuntimeStore) FinishRuntime(ctx context.Context, execID, runID string, status RuntimeStatus, ec string) error {
	if status == RuntimeUnknown {
		s.once.Do(func() {
			close(s.started)
			<-s.release
		})
	}
	return s.flakyStore.FinishRuntime(ctx, execID, runID, status, ec)
}

func (f *flakyStore) SetDelivery(ctx context.Context, execID, ownerID string, status Status, ec string) error {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("db temporarily unavailable")
	}
	return f.SQLStore.SetDelivery(ctx, execID, ownerID, status, ec)
}

func (f *flakyStore) FinishRuntime(ctx context.Context, execID, runID string, status RuntimeStatus, ec string) error {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("db temporarily unavailable")
	}
	return f.SQLStore.FinishRuntime(ctx, execID, runID, status, ec)
}

func (f *flakyStore) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func fastRepairConfig() RepairConfig {
	return RepairConfig{
		QueueCapacity:    4,
		InitialBackoff:   20 * time.Millisecond,
		MaxBackoff:       100 * time.Millisecond,
		MaxLifetime:      2 * time.Second,
		ShutdownTimeout:  500 * time.Millisecond,
		SyncRetryTimeout: 20 * time.Millisecond,
		TickInterval:     10 * time.Millisecond,
	}
}

func newRepairTestStore(t *testing.T) (*flakyStore, *session.SQLiteStore) {
	t.Helper()
	store, sessionStore := newTestSQLStore(t)
	return &flakyStore{SQLStore: store}, sessionStore
}

func ensureRepairSession(t *testing.T, ss *session.SQLiteStore, sessionID string) {
	t.Helper()
	if sessionID == "session-1" {
		return
	}
	now := time.Now()
	require.NoError(t, ss.Upsert(context.Background(), &session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: "claude_code",
		State: events.StateRunning, CreatedAt: now, UpdatedAt: now,
	}))
}

func TestRepairer_SuccessfulProcessing(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp1", ClientMessageID: "msg-rp1", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)

	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		OwnerID:     testOwner,
		Kind:        RepairDelivery,
		Status:      string(StatusDelivered),
	})

	require.Eventually(t, func() bool {
		stored, err := store.getByID(context.Background(), rec.ExecutionID)
		if err != nil {
			return false
		}
		return stored.Status == StatusDelivered
	}, 2*time.Second, 10*time.Millisecond, "delivery status must be repaired to delivered")

	r.Shutdown(context.Background())
}

func TestRepairer_RuntimeSuccessHook(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-hook")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-hook", ClientMessageID: "msg-hook", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.SetDelivery(ctx, rec.ExecutionID, testOwner, StatusDelivered, ""))
	require.NoError(t, store.MarkRunning(ctx, rec.ExecutionID, testOwner, testRun))

	hookCh := make(chan RepairIntent, 1)
	r := NewRepairer(store, fastRepairConfig(), nil)
	r.SetSuccessHook(func(intent RepairIntent) {
		hookCh <- intent
	})
	r.Start(ctx)
	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		SessionID:   "session-hook",
		WorkerRunID: testRun,
		Kind:        RepairRuntime,
		Status:      string(RuntimeCompleted),
	})

	select {
	case got := <-hookCh:
		require.Equal(t, "session-hook", got.SessionID)
		require.Equal(t, RepairRuntime, got.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("runtime repair success hook was not called")
	}
	r.Shutdown(context.Background())
}

func TestRepairer_RetryAfterFailure(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp2", ClientMessageID: "msg-rp2", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)

	store.setFail(true)
	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		OwnerID:     testOwner,
		Kind:        RepairDelivery,
		Status:      string(StatusDelivered),
	})

	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&store.calls) >= 3
	}, 2*time.Second, 10*time.Millisecond, "must have attempted at least 3 retries")

	require.Greater(t, r.Backlog(), 0, "intent must still be in queue while DB is down")

	store.setFail(false)

	require.Eventually(t, func() bool {
		stored, err := store.getByID(context.Background(), rec.ExecutionID)
		if err != nil {
			return false
		}
		return stored.Status == StatusDelivered && r.Backlog() == 0
	}, 2*time.Second, 10*time.Millisecond, "must succeed after DB recovers")

	r.Shutdown(context.Background())
}

func TestRepairer_MergeTerminalPreferred(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp3", ClientMessageID: "msg-rp3", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, rec.ExecutionID, testOwner, testRun))

	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)
	store.setFail(true)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		WorkerRunID: testRun,
		Kind:        RepairRuntime,
		Status:      string(RuntimeUnknown),
	})

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		WorkerRunID: testRun,
		Kind:        RepairRuntime,
		Status:      string(RuntimeCompleted),
	})

	status, _, found := r.Lookup(rec.ExecutionID)
	require.True(t, found)
	require.Equal(t, string(RuntimeCompleted), status, "completed must replace unknown")

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		WorkerRunID: testRun,
		Kind:        RepairRuntime,
		Status:      string(RuntimeUnknown),
	})

	status, _, found = r.Lookup(rec.ExecutionID)
	require.True(t, found)
	require.Equal(t, string(RuntimeCompleted), status, "completed must NOT regress to unknown")

	store.setFail(false)
	r.Shutdown(context.Background())
}

func TestRepairer_MaxLifetimeTimeout(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp4", ClientMessageID: "msg-rp4", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)

	cfg := fastRepairConfig()
	cfg.MaxLifetime = 100 * time.Millisecond

	store.setFail(true)
	r := NewRepairer(store, cfg, nil)
	r.Start(ctx)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		OwnerID:     testOwner,
		Kind:        RepairDelivery,
		Status:      string(StatusDelivered),
	})

	require.Eventually(t, func() bool {
		_, _, timedOut, _ := r.Stats()
		return timedOut > 0
	}, 3*time.Second, 10*time.Millisecond, "intent must time out after MaxLifetime")

	require.Equal(t, 0, r.Backlog(), "timed-out intent must be removed from queue")

	store.setFail(false)
	r.Shutdown(context.Background())
}

func TestRepairer_LookupForDuplicateOverlay(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp5")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp5", ClientMessageID: "msg-rp5", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)

	store.setFail(true)
	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		OwnerID:     testOwner,
		Kind:        RepairDelivery,
		Status:      string(StatusDelivered),
	})

	status, kind, found := r.Lookup(rec.ExecutionID)
	require.True(t, found)
	require.Equal(t, string(StatusDelivered), status)
	require.Equal(t, "delivery", kind)

	store.setFail(false)
	r.Shutdown(context.Background())
}

func TestRepairer_ShutdownDrainCompletes(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp6")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp6", ClientMessageID: "msg-rp6", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)

	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID,
		OwnerID:     testOwner,
		Kind:        RepairDelivery,
		Status:      string(StatusDelivered),
	})

	start := time.Now()
	r.Shutdown(context.Background())
	elapsed := time.Since(start)

	require.Less(t, elapsed, 1*time.Second, "shutdown must complete quickly when DB is healthy")
	require.Equal(t, 0, r.Backlog(), "backlog must be empty after successful drain")
}

func TestRepairer_InFlightIntentDoesNotDeleteNewerTerminal(t *testing.T) {
	t.Parallel()
	base, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp7")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := base.Accept(ctx, AcceptRequest{
		SessionID: "session-rp7", ClientMessageID: "msg-rp7", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)
	require.NoError(t, base.MarkRunning(ctx, rec.ExecutionID, testOwner, testRun))

	store := &blockingRuntimeStore{
		flakyStore: base,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	r := NewRepairer(store, fastRepairConfig(), nil)
	r.Start(ctx)
	defer r.Shutdown(context.Background())

	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID, WorkerRunID: testRun,
		Kind: RepairRuntime, Status: string(RuntimeUnknown),
	})
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("unknown repair did not start")
	}
	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID, WorkerRunID: testRun,
		Kind: RepairRuntime, Status: string(RuntimeCompleted),
	})
	close(store.release)

	require.Eventually(t, func() bool {
		stored, getErr := base.getByID(context.Background(), rec.ExecutionID)
		return getErr == nil && stored.RuntimeStatus == RuntimeCompleted && r.Backlog() == 0
	}, 2*time.Second, 10*time.Millisecond)
}

func TestRepairer_TimedOutIntentIsExcludedFromLeaseRenewal(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp8")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp8", ClientMessageID: "msg-rp8", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, rec.ExecutionID, testOwner, testRun))

	cfg := fastRepairConfig()
	cfg.MaxLifetime = 50 * time.Millisecond
	store.setFail(true)
	r := NewRepairer(store, cfg, nil)
	r.Start(ctx)
	defer r.Shutdown(context.Background())
	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID, WorkerRunID: testRun,
		Kind: RepairRuntime, Status: string(RuntimeCompleted),
	})

	require.Eventually(t, func() bool {
		return len(r.AbandonedExecutionIDs()) == 1
	}, time.Second, 10*time.Millisecond)
	store.setFail(false)
	renewed, err := store.RenewLeases(ctx, testOwner, 120, r.AbandonedExecutionIDs())
	require.NoError(t, err)
	require.Zero(t, renewed, "abandoned repair must stop extending its execution lease")
}

func TestRepairer_TimedOutDeliveryDoesNotStopLeaseRenewal(t *testing.T) {
	t.Parallel()
	store, sessionStore := newRepairTestStore(t)
	ensureRepairSession(t, sessionStore, "session-rp9")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec, _, err := store.Accept(ctx, AcceptRequest{
		SessionID: "session-rp9", ClientMessageID: "msg-rp9", PayloadHash: "h",
		OwnerInstanceID: testOwner, WorkerRunID: testRun,
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkRunning(ctx, rec.ExecutionID, testOwner, testRun))

	cfg := fastRepairConfig()
	cfg.MaxLifetime = 50 * time.Millisecond
	store.setFail(true)
	r := NewRepairer(store, cfg, nil)
	r.Start(ctx)
	defer r.Shutdown(context.Background())
	r.Enqueue(RepairIntent{
		ExecutionID: rec.ExecutionID, OwnerID: testOwner,
		Kind: RepairDelivery, Status: string(StatusDelivered),
	})

	require.Eventually(t, func() bool {
		return r.Backlog() == 0
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, r.AbandonedExecutionIDs())
	store.setFail(false)
	renewed, err := store.RenewLeases(ctx, testOwner, 120, r.AbandonedExecutionIDs())
	require.NoError(t, err)
	require.EqualValues(t, 1, renewed, "delivery-only repair failure must not abandon a live runtime")
}
