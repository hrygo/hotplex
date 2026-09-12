#!/usr/bin/env python3
"""Extra exact-source regressions; bootstrap-only, excluded from product branch."""
import argparse
from hardening import write, replace


def tests() -> None:
    write("internal/worker/codexcli/cancellation_audit_test.go", r'''
package codexcli

import (
    "bytes"
    "context"
    "errors"
    "log/slog"
    "testing"
    "testing/synctest"
    "time"

    "github.com/hrygo/hotplex/internal/config"
    "github.com/hrygo/hotplex/internal/worker"
    "github.com/hrygo/hotplex/internal/worker/base"
    "github.com/stretchr/testify/require"
)

type auditRPCSink struct { bytes.Buffer }
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
''')
    write("internal/worker/opencodeserver/close_audit_test.go", r'''
package opencodeserver

import (
    "context"
    "log/slog"
    "testing"
    "testing/synctest"

    "github.com/hrygo/hotplex/internal/worker/base"
    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

func TestAuditOCSCloseSynchronizesBlockedCriticalSend(t *testing.T) {
    t.Parallel()
    synctest.Test(t, func(t *testing.T) {
        c := &conn{sessionID: "session", log: slog.Default(), recvCh: make(chan *events.Envelope)}
        w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil), httpConn: c}
        bus := make(chan *events.Envelope, 1)
        bus <- &events.Envelope{Event: events.Event{Type: events.Done}}
        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()
        go w.forwardBusEvents(ctx, "session", bus)
        synctest.Wait()
        require.NoError(t, c.Close())
        cancel()
        synctest.Wait()
        _, open := <-c.Recv()
        require.False(t, open)
    })
}
''')


def fixes() -> None:
    write("internal/worker/codexcli/response_wait_error.go", r'''
package codexcli

// responseWaitError means the frame was written, but its response was not
// observed within the caller's budget. Keep this phase distinct from a failed
// stdin write: only the latter is evidence for singleton write-stall recovery.
type responseWaitError struct { cause error }

func (e *responseWaitError) Error() string { return e.cause.Error() }
func (e *responseWaitError) Unwrap() error { return e.cause }
''')
    path = "internal/worker/codexcli/manager.go"
    replace(path, '''\tcase <-timer.C:
\t\treturn nil, fmt.Errorf("codex-app-server: %s: timeout after %v",
\t\t\tmethod, callTimeout)
\t}
}''', '''\tcase <-ctx.Done():
        return nil, &responseWaitError{cause: fmt.Errorf("codex-app-server: %s: %w", method, ctx.Err())}
    case <-timer.C:
        return nil, &responseWaitError{cause: fmt.Errorf("codex-app-server: %s: timeout after %v: %w",
            method, callTimeout, context.DeadlineExceeded)}
    }
}''')
    path = "internal/worker/codexcli/worker.go"
    replace(path, '''\tresp, err := w.manager.Call(ctx, "turn/start", params)
\tif err != nil {''', '''\tresp, err := w.manager.Call(ctx, "turn/start", params)
\tif err != nil {
        var responseErr *responseWaitError
        if errors.As(err, &responseErr) {
            return &worker.WorkerError{
                Kind: worker.ErrKindTimeout,
                Message: fmt.Sprintf("codexcli: turn/start: %v", err),
                Cause: err,
            }
        }''')
    replace(path, "w.startNewThread(", "w.startNewThread(ctx, ", count=3)
    replace(path, "func (w *AppServerWorker) startNewThread(session worker.SessionInfo, errPrefix string) error {", "func (w *AppServerWorker) startNewThread(ctx context.Context, session worker.SessionInfo, errPrefix string) error {")
    replace(path, 'w.manager.Call(context.Background(), "thread/start", params)', 'w.manager.Call(ctx, "thread/start", params)')

    path = "internal/worker/opencodeserver/worker.go"
    replace(path, "\trecvCh       chan *events.Envelope", "\trecvCh       chan *events.Envelope\n\trecvGate     base.EventGate")
    replace(path, "!trySendEnvelope(recvCh, env, false, 0)", "!ch.recvGate.TrySend(recvCh, env)")
    replace(path, "!trySendEnvelope(recvCh, env, true, criticalEventSendTimeout)", "!ch.recvGate.SendTimeout(recvCh, env, criticalEventSendTimeout)")
    replace(path, "\t\tclose(c.recvCh)", "\t\tc.recvGate.Close(c.recvCh)")
    replace(path, "\tbase.InjectWithTimeout(c.recvCh, env, c.log, c.getSessionID())", '''\tif !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) && c.log != nil {
        c.log.Warn("opencodeserver: inject failed, channel closed or full",
            "session_id", c.getSessionID(), "event_type", env.Event.Type)
    }''')
    replace(path, '''\t\t\t// Critical event: block with timeout to guarantee delivery.
\t\t\t// trySendEnvelope recovers from send-on-closed-channel panics
\t\t\t// (TOCTOU race: conn.Close can close recvCh between our closed
\t\t\t// check above and the actual send).''', '''\t\t\t// Critical events retain their bounded send budget. EventGate wakes
            // blocked sends before Close closes recvCh, eliminating the former
            // send/close race rather than merely recovering its panic.''')


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["tests", "fixes"])
    args = parser.parse_args()
    {"tests": tests, "fixes": fixes}[args.mode]()
