package codexcli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
)

type auditRPCSink struct{ bytes.Buffer }

func (s *auditRPCSink) Close() error { return nil }

func auditRPCManager() *CodexAppServerManager {
	m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{CallTimeout: time.Hour})
	m.stdin = &auditRPCSink{}
	m.state = stateRunning
	return m
}

func TestAuditCodexCallHonorsResponseCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.Call(ctx, "probe", nil); result <- err }()
		// The request is written and the caller is durably blocked waiting
		// for a response, without any wall-clock sleeps.
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.Canceled)
			pending := 0
			m.pending.Range(func(_, _ any) bool { pending++; return true })
			require.Zero(t, pending)
			require.True(t, m.IsRunning(), "one canceled call must not stop the shared server")
		default:
			// Advance only the bubble's virtual clock to retire the defective
			// baseline call before failing. Otherwise it can abort the entire
			// package and hide independent regression results.
			time.Sleep(2 * time.Hour)
			<-result
			t.Fatal("Call ignored cancellation after the request had been written")
		}
	})
}

func TestAuditCodexResponseCancellationIsNotUnavailable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m, threadID: "thread"}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- w.startTurn(ctx, []TurnInputItem{{Type: "text", Text: "probe"}}) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.Canceled)
			var workerErr *worker.WorkerError
			if errors.As(err, &workerErr) {
				require.NotEqual(t, worker.ErrKindUnavailable, workerErr.Kind,
					"response cancellation is not evidence of a stalled singleton stdin")
			}
			require.True(t, m.IsRunning())
		default:
			time.Sleep(2 * time.Hour) // virtual time only; drain baseline failure
			<-result
			t.Fatal("turn/start ignored caller cancellation")
		}
	})
}

func TestAuditCodexResponseTimeoutPreservesDeadlineIdentity(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		m.cfg.CallTimeout = time.Second
		_, err := m.Call(context.Background(), "probe", nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
