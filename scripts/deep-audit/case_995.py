from common import write, replace, between, main


def tests():
    write('internal/gateway/deep_audit_995_test.go', r'''
package gateway

import (
    "context"
    "log/slog"
    "sync"
    "testing"
    "testing/synctest"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/hrygo/hotplex/pkg/events"
)

type deep995Conn struct { send func(context.Context) error }
func (c *deep995Conn) WriteCtx(ctx context.Context,_ *events.Envelope) error { return c.send(ctx) }
func (c *deep995Conn) Close() error { return nil }
func deep995Writer(pc *deep995Conn) *pcEntry {
    return newPCEntry(context.Background(),pc,pcEntryConfig{WriteBuffer:8,DropThreshold:7,CoalesceIntvl:time.Millisecond,CoalesceSize:200,TerminalTimeout:time.Second},slog.Default())
}
func deep995Done() *events.Envelope { return &events.Envelope{Event:events.Event{Type:events.Done}} }

func TestDeepAudit995SuccessfulReceiptNeverBecomesTimeout(t *testing.T) {
    t.Parallel()
    synctest.Test(t,func(t *testing.T) {
        e := deep995Writer(&deep995Conn{send:func(context.Context)error{return nil}})
        defer e.Close()
        result := make(chan error,1)
        require.NoError(t,e.EnqueueWrite(context.Background(),deep995Done(),result))
        require.NoError(t,<-result)
        // Advance the bubble's virtual clock beyond the receipt budget.
        <-time.After(2*time.Second)
        synctest.Wait()
        select {
        case err := <-result: t.Fatalf("completed receipt produced a second outcome: %v",err)
        default:
        }
    })
}

func TestDeepAudit995TerminalWithoutReceiptKeepsLiveContext(t *testing.T) {
    t.Parallel()
    synctest.Test(t,func(t *testing.T) {
        observed := make(chan error,1)
        e := deep995Writer(&deep995Conn{send:func(ctx context.Context)error{observed<-ctx.Err();return nil}})
        defer e.Close()
        require.NoError(t,e.EnqueueWrite(context.Background(),deep995Done(),nil))
        require.NoError(t,<-observed,"not requesting a receipt must not cancel the platform write")
    })
}

func TestDeepAudit995LateWriteCannotOverwriteTimeout(t *testing.T) {
    t.Parallel()
    synctest.Test(t,func(t *testing.T) {
        entered,release := make(chan struct{}),make(chan struct{})
        var releaseOnce sync.Once
        e := deep995Writer(&deep995Conn{send:func(context.Context)error{close(entered);<-release;return nil}})
        defer e.Close()
        defer releaseOnce.Do(func(){close(release)})
        result := make(chan error,1)
        require.NoError(t,e.EnqueueWrite(context.Background(),deep995Done(),result))
        <-entered
        require.ErrorIs(t,<-result,context.DeadlineExceeded)
        releaseOnce.Do(func(){close(release)})
        synctest.Wait()
        select {
        case err := <-result: t.Fatalf("late platform completion produced a second outcome: %v",err)
        default:
        }
    })
}
''')


def fixes():
    path = 'internal/gateway/platform_writer.go'
    replace(path,'\tresult chan<- error\n}', '\tresult chan<- error\n\tcompletion *platformWriteCompletion\n}')
    between(path,'\twrite := platformWrite{env: env, ctx: ctx}', '\t// The closed check and the enqueue',r'''
    write := platformWrite{env: env, ctx: ctx}
    if isTerminalPlatformEvent(env.Event.Type) {
        terminalCtx,cancel := context.WithTimeout(ctx,e.cfg.TerminalTimeout)
        completion := &platformWriteCompletion{result:result,cancel:cancel,done:make(chan struct{})}
        write.ctx = terminalCtx
        write.completion = completion
        ctx = terminalCtx
        // Admission failure owns cleanup but is returned synchronously, not
        // delivered as a second asynchronous receipt.
        defer func() {
            if err != nil { completion.abandon() }
        }()
        go func() {
            select {
            case <-completion.done:
            case <-terminalCtx.Done():
                completion.finish(fmt.Errorf("platform conn terminal write timeout: %w",terminalCtx.Err()))
            case <-e.done:
                completion.finish(errors.New("platform conn closed"))
            }
        }()
    }
''')
    replace(path,'''\tif write.result != nil {
\t\tselect {
\t\tcase write.result <- err:
\t\tdefault:
\t\t}
\t}''', '''\tif write.completion != nil {
        write.completion.finish(err)
    } else if write.result != nil {
        // Legacy internal test fixtures may provide an unguarded receipt.
        select {
        case write.result <- err:
        default:
        }
    }''')
    write('internal/gateway/platform_write_completion.go',r'''
package gateway

import (
    "context"
    "sync"
)

// platformWriteCompletion arbitrates local write, timeout and shutdown results.
// This is a local one-shot receipt, not an external delivery exactly-once claim.
type platformWriteCompletion struct {
    once sync.Once
    result chan<- error
    cancel context.CancelFunc
    done chan struct{}
}

func (c *platformWriteCompletion) finish(err error) {
    c.once.Do(func() {
        c.cancel()
        close(c.done)
        if c.result != nil {
            select {
            case c.result <- err:
            default:
            }
        }
    })
}

func (c *platformWriteCompletion) abandon() {
    c.once.Do(func() {
        c.cancel()
        close(c.done)
    })
}
''')


if __name__ == '__main__': main(tests,fixes)
