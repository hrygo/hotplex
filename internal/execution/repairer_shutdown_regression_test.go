package execution

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockedShutdownDeliveryStore struct {
	Store
	calls   atomic.Int64
	entered chan struct{}
	second  chan struct{}
	release chan struct{}
}

func (s *blockedShutdownDeliveryStore) SetDelivery(ctx context.Context, id, owner string, status Status, code string) error {
	switch s.calls.Add(1) {
	case 1:
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	case 2:
		close(s.second)
	}
	return s.Store.SetDelivery(ctx, id, owner, status, code)
}

func TestRepairer_ShutdownDoesNotOverlapLoopOrAnotherDrain(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		name := "drain_only"
		if started {
			name = "active_loop"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base, sessions := newRepairTestStore(t)
			ensureRepairSession(t, sessions, name)
			ctx, cancel := context.WithCancel(t.Context())
			record, _, err := base.Accept(ctx, AcceptRequest{
				SessionID: name, ClientMessageID: "shutdown-message", PayloadHash: "hash",
				OwnerInstanceID: testOwner, WorkerRunID: testRun,
			})
			require.NoError(t, err)
			store := &blockedShutdownDeliveryStore{
				Store: base, entered: make(chan struct{}),
				second: make(chan struct{}), release: make(chan struct{}),
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(store.release) }) }
			repairer := NewRepairer(store, fastRepairConfig(), nil)
			var hooks atomic.Int64
			repairer.SetSuccessHook(func(RepairIntent) { hooks.Add(1) })
			t.Cleanup(func() {
				release()
				cancel()
				repairer.Shutdown(context.Background())
			})
			if started {
				repairer.Start(ctx)
			}
			repairer.Enqueue(RepairIntent{
				ExecutionID: record.ExecutionID, OwnerID: testOwner,
				Kind: RepairDelivery, Status: string(StatusDelivered),
			})
			firstDone := make(chan struct{})
			if started {
				select {
				case <-store.entered:
				case <-time.After(time.Second):
					t.Fatal("background repair did not enter the store")
				}
			}
			go func() {
				repairer.Shutdown(context.Background())
				close(firstDone)
			}()
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("shutdown drain did not enter the store")
			}
			select {
			case <-repairer.stopCh:
			case <-time.After(time.Second):
				t.Fatal("shutdown did not signal the loop")
			}
			secondDone := make(chan struct{})
			go func() {
				repairer.Shutdown(context.Background())
				close(secondDone)
			}()

			overlap := false
			select {
			case <-store.second:
				overlap = true
			case <-time.After(100 * time.Millisecond):
			}
			release()
			for _, done := range []<-chan struct{}{firstDone, secondDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("shutdown did not complete after the store was released")
				}
			}
			require.False(t, overlap, "shutdown must not process an intent while another processor owns it")
			require.EqualValues(t, 1, store.calls.Load(), "the repair must be written exactly once")
			require.EqualValues(t, 1, hooks.Load(), "the repair success hook must fire exactly once")
			require.Zero(t, repairer.Backlog())
			_, succeeded, _, _ := repairer.Stats()
			require.EqualValues(t, 1, succeeded)
			stored, err := base.getByID(t.Context(), record.ExecutionID)
			require.NoError(t, err)
			require.Equal(t, StatusDelivered, stored.Status)
		})
	}
}

func TestRepairer_InterruptedShutdownDoesNotDrainAnActiveLoop(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"timeout", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base, sessions := newRepairTestStore(t)
			ensureRepairSession(t, sessions, name)
			ctx, cancel := context.WithCancel(t.Context())
			record, _, err := base.Accept(ctx, AcceptRequest{
				SessionID: name, ClientMessageID: "interrupted-shutdown", PayloadHash: "hash",
				OwnerInstanceID: testOwner, WorkerRunID: testRun,
			})
			require.NoError(t, err)
			store := &blockedShutdownDeliveryStore{
				Store: base, entered: make(chan struct{}),
				second: make(chan struct{}), release: make(chan struct{}),
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(store.release) }) }
			cfg := fastRepairConfig()
			cfg.ShutdownTimeout = 30 * time.Millisecond
			repairer := NewRepairer(store, cfg, nil)
			t.Cleanup(func() {
				release()
				cancel()
				repairer.Shutdown(context.Background())
				repairer.wg.Wait()
			})
			repairer.Start(ctx)
			repairer.Enqueue(RepairIntent{
				ExecutionID: record.ExecutionID, OwnerID: testOwner,
				Kind: RepairDelivery, Status: string(StatusDelivered),
			})
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("background repair did not enter the store")
			}
			shutdownCtx, stopShutdown := context.WithCancel(context.Background())
			defer stopShutdown()
			if name == "cancelled" {
				stopShutdown()
			}

			repairer.Shutdown(shutdownCtx)

			require.True(t, repairer.closed.Load())
			require.EqualValues(t, 1, store.calls.Load(), "an interrupted wait must not start a concurrent drain")
			release()
			loopDone := make(chan struct{})
			go func() {
				repairer.wg.Wait()
				close(loopDone)
			}()
			select {
			case <-loopDone:
			case <-time.After(time.Second):
				t.Fatal("background loop did not exit after the store was released")
			}
			require.EqualValues(t, 1, store.calls.Load())
			require.Zero(t, repairer.Backlog())
			stored, err := base.getByID(t.Context(), record.ExecutionID)
			require.NoError(t, err)
			require.Equal(t, StatusDelivered, stored.Status)
		})
	}
}
