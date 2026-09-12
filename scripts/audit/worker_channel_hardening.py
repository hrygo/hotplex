#!/usr/bin/env python3
"""Build an auditable red/green patch against the pinned HotPlex worktree.

This bootstrap-only script is not included in the product branch. Every source
edit requires an exact, unique match; a drift aborts rather than guessing.
"""
from pathlib import Path
import argparse

ROOT = Path.cwd()


def write(path: str, content: str) -> None:
    dest = ROOT / path
    if dest.exists():
        raise RuntimeError(f"Refusing to overwrite new file: {path}")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(content.lstrip("\n"), encoding="utf-8")


def replace(path: str, old: str, new: str, count: int = 1) -> None:
    dest = ROOT / path
    text = dest.read_text(encoding="utf-8")
    actual = text.count(old)
    if actual != count:
        raise RuntimeError(f"{path}: expected {count} exact matches, found {actual}")
    dest.write_text(text.replace(old, new), encoding="utf-8")


def tests() -> None:
    write("internal/worker/base/lifecycle_audit_test.go", r'''
package base

import (
    "errors"
    "sync"
    "testing"

    "github.com/hrygo/hotplex/internal/worker/proc"
    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

func TestAuditProcCompletionPreservesReplacement(t *testing.T) {
    t.Parallel()
    for _, replaceProc := range []bool{false, true} {
        name := "same_process"
        if replaceProc { name = "replacement_process" }
        t.Run(name, func(t *testing.T) {
            t.Parallel()
            oldProc, replacement := &proc.Manager{}, &proc.Manager{}
            w := NewBaseWorker(nil, nil)
            w.Proc = oldProc
            entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
            var code int
            var err error
            go func() {
                defer close(finished)
                code, err = withProcResult(w, func(p *proc.Manager) (int, error) {
                    close(entered)
                    <-release
                    return 42, nil
                }, -1, errors.New("missing process"))
            }()
            <-entered
            if replaceProc {
                w.Mu.Lock()
                w.Proc = replacement
                w.Mu.Unlock()
            }
            close(release)
            <-finished
            require.NoError(t, err)
            require.Equal(t, 42, code)
            if replaceProc { require.Same(t, replacement, w.Proc) } else { require.Nil(t, w.Proc) }
        })
    }
}

func TestAuditConnTrySendAfterClose(t *testing.T) {
    t.Parallel()
    c := NewConn(nil, nil, "user", "session")
    require.NoError(t, c.Close())
    require.NotPanics(t, func() {
        require.False(t, c.TrySend(&events.Envelope{Event: events.Event{Type: events.Done}}))
    })
    require.NoError(t, c.Close())
}

func TestAuditConnSessionIDConcurrentAccess(t *testing.T) {
    t.Parallel()
    c := NewConn(nil, nil, "user", "initial")
    defer c.Close()
    var wg sync.WaitGroup
    start := make(chan struct{})
    wg.Add(2)
    go func() {
        defer wg.Done()
        <-start
        for i := 0; i < 1000; i++ { c.SetSessionID("replacement") }
    }()
    go func() {
        defer wg.Done()
        <-start
        for i := 0; i < 1000; i++ { _ = c.SessionID() }
    }()
    close(start)
    wg.Wait()
}
''')
    write("internal/messaging/dedup_audit_test.go", r'''
package messaging

import (
    "sync"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestAuditDedupExpiresBeforeSweep(t *testing.T) {
    t.Parallel()
    d := NewDedup(2, time.Hour)
    old, accepted := d.TryRecordWithHandle("message")
    require.True(t, accepted)
    d.mu.Lock()
    entry := d.entries["message"]
    entry.recordedAt = time.Now().Add(-2 * time.Hour)
    d.entries["message"] = entry
    d.mu.Unlock()
    fresh, accepted := d.TryRecordWithHandle("message")
    require.True(t, accepted, "TTL expiry must not depend on the cleanup scheduler")
    require.NotNil(t, fresh)
    d.Rollback(old)
    require.Equal(t, 1, d.Len(), "an old failure must not erase a fresh retry")
    require.False(t, d.TryRecord("message"))
    d.Rollback(fresh)
    require.Zero(t, d.Len())
}

func TestAuditDedupCloseBeforeStart(t *testing.T) {
    t.Parallel()
    d := NewDedup(2, time.Hour)
    d.Close()
    d.StartCleanup()
    d.Close()
    select {
    case <-d.done:
    default:
        // Close the baseline's orphaned loop before reporting its failure.
        close(d.done)
        t.Fatal("StartCleanup resurrected a closed deduplicator")
    }
}

func TestAuditDedupRepeatedStartKeepsSignal(t *testing.T) {
    t.Parallel()
    d := NewDedup(2, time.Hour)
    d.StartCleanup()
    original := d.done
    d.StartCleanup()
    sameSignal := original == d.done
    d.Close()
    if !sameSignal {
        // A baseline loop may already have captured the old signal.
        close(original)
    }
    require.True(t, sameSignal, "repeated StartCleanup replaced the shutdown signal")
}

func TestAuditDedupConcurrentLifecycle(t *testing.T) {
    t.Parallel()
    d := NewDedup(2, time.Hour)
    var wg sync.WaitGroup
    start := make(chan struct{})
    for i := 0; i < 8; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            <-start
            d.StartCleanup()
        }()
    }
    close(start)
    wg.Wait()
    d.Close()
}
''')
    write("internal/messaging/feishu/chat_queue_audit_test.go", r'''
package feishu

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestAuditChatQueuePanicPreservesAcceptedTail(t *testing.T) {
    t.Parallel()
    q := NewChatQueue(nil)
    defer q.Close()
    entered, release, tail := make(chan struct{}), make(chan struct{}), make(chan struct{})
    require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error {
        close(entered)
        <-release
        panic("injected task failure")
    }))
    <-entered
    require.NoError(t, q.Enqueue("chat", func(ctx context.Context) error {
        close(tail)
        return nil
    }))
    close(release)
    select {
    case <-tail:
    case <-time.After(time.Second):
        t.Fatal("an accepted task was stranded behind a panicking task")
    }
}
''')
    write("internal/gateway/platform_writer_audit_test.go", r'''
package gateway

import (
    "context"
    "log/slog"
    "testing"
    "time"

    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

type auditPlatformConn struct { writes chan *events.Envelope }
func (c *auditPlatformConn) WriteCtx(ctx context.Context, env *events.Envelope) error {
    select {
    case c.writes <- env: return nil
    case <-ctx.Done(): return ctx.Err()
    }
}
func (c *auditPlatformConn) Close() error { return nil }

func auditWriter(t *testing.T) (*pcEntry, <-chan *events.Envelope) {
    t.Helper()
    pc := &auditPlatformConn{writes: make(chan *events.Envelope, 32)}
    e := newPCEntry(context.Background(), pc, pcEntryConfig{
        WriteBuffer: 16, DropThreshold: 15, CoalesceIntvl: 5 * time.Millisecond,
        CoalesceSize: 200, TerminalTimeout: time.Second,
    }, slog.Default())
    t.Cleanup(func() { require.NoError(t, e.Close()) })
    return e, pc.writes
}

func auditReceive(t *testing.T, ch <-chan *events.Envelope) *events.Envelope {
    t.Helper()
    select {
    case env := <-ch: return env
    case <-time.After(time.Second):
        t.Fatal("accepted platform event did not flush within the test deadline")
        return nil
    }
}

func TestAuditPlatformTimerRearmsAfterFlush(t *testing.T) {
    t.Parallel()
    e, writes := auditWriter(t)
    for _, content := range []string{"first", "second", "third"} {
        env := &events.Envelope{SessionID: "session", Event: events.Event{
            Type: events.MessageDelta, Data: events.MessageDeltaData{Content: content},
        }}
        require.NoError(t, e.WriteCtx(context.Background(), env))
        require.Equal(t, content, extractDeltaContent(auditReceive(t, writes)))
    }
}

func TestAuditPlatformReasoningIsNotTextCoalesced(t *testing.T) {
    t.Parallel()
    e, writes := auditWriter(t)
    reasoning := &events.Envelope{SessionID: "session", Event: events.Event{
        Type: events.Reasoning, Data: map[string]any{"content": "reasoning delta"},
    }}
    done := &events.Envelope{SessionID: "session", Event: events.Event{Type: events.Done}}
    require.NoError(t, e.WriteCtx(context.Background(), reasoning))
    require.NoError(t, e.WriteCtx(context.Background(), done))
    got := auditReceive(t, writes)
    require.Equal(t, events.Reasoning, got.Event.Type, "droppable does not mean text-coalescible")
    require.Equal(t, reasoning.Event.Data, got.Event.Data)
    require.Equal(t, events.Done, auditReceive(t, writes).Event.Type)
}
''')
    write("internal/worker/opencodeserver/directory_audit_test.go", r'''
package opencodeserver

import (
    "context"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

func TestAuditPromptPreservesWorkspaceDirectory(t *testing.T) {
    t.Parallel()
    for _, dir := range []string{"", "/tmp/workspace with spaces/项目&a=b"} {
        t.Run(dir, func(t *testing.T) {
            t.Parallel()
            observed := make(chan string, 1)
            server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                observed <- r.URL.Query().Get("directory")
                w.WriteHeader(http.StatusNoContent)
            }))
            defer server.Close()
            c := &conn{sessionID: "session", projectDir: dir, httpAddr: server.URL, client: server.Client()}
            require.NoError(t, c.Send(context.Background(), &events.Envelope{Event: events.Event{
                Type: events.Input, Data: events.InputData{Content: "probe"},
            }}))
            require.Equal(t, dir, <-observed)
        })
    }
}
''')
    write("internal/worker/acp/conn_audit_test.go", r'''
package acp

import (
    "log/slog"
    "sync"
    "testing"
    "time"

    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

func TestAuditACPConcurrentSendClose(t *testing.T) {
    t.Parallel()
    for i := 0; i < 32; i++ {
        c := newACPConn("user", "session", slog.Default())
        start := make(chan struct{})
        var wg sync.WaitGroup
        wg.Add(2)
        go func() {
            defer wg.Done()
            <-start
            for j := 0; j < 64; j++ {
                c.TrySend(&events.Envelope{Event: events.Event{Type: events.MessageDelta}})
            }
        }()
        go func() { defer wg.Done(); <-start; _ = c.Close() }()
        close(start)
        wg.Wait()
    }
}

func TestAuditACPCloseUnblocksCriticalSend(t *testing.T) {
    t.Parallel()
    c := newACPConn("user", "session", slog.Default())
    for i := 0; i < cap(c.recvCh); i++ {
        require.True(t, c.TrySend(&events.Envelope{Event: events.Event{Type: events.MessageDelta}}))
    }
    result := make(chan bool, 1)
    go func() { result <- c.TrySend(&events.Envelope{Event: events.Event{Type: events.Done}}) }()
    require.NoError(t, c.Close())
    select {
    case sent := <-result: require.False(t, sent)
    case <-time.After(time.Second): t.Fatal("Close did not unblock a critical sender")
    }
}
''')


def fixes() -> None:
    replace("internal/worker/base/worker.go", "\tw.Mu.Lock()\n\tw.Proc = nil\n\tw.Mu.Unlock()\n\n\treturn result, nil", "\tw.Mu.Lock()\n\t// A reset may have installed a replacement while fn waited for the old\n\t// process. Only the operation that still owns Proc may clear it.\n\tif w.Proc == p {\n\t\tw.Proc = nil\n\t}\n\tw.Mu.Unlock()\n\n\treturn result, nil")
    replace("internal/worker/base/worker.go", "// returns (zero, errNil). On success, Proc is nil'd under the mutex.", "// returns (zero, errNil). On success, Proc is cleared only if it is still the snapshot.")

    path = "internal/gateway/platform_writer.go"
    text = (ROOT / path).read_text()
    start, end = text.index("func (e *pcEntry) writeLoop()"), text.index("func (e *pcEntry) writeOne(")
    body = text[start:end]
    if body.count("isDroppable(") != 3:
        raise RuntimeError("unexpected platform coalescing branches")
    body = body.replace("isDroppable(", "isCoalesciblePlatformEvent(")
    body = body.replace("timer.Reset(e.cfg.CoalesceIntvl)", "timer.Reset(e.cfg.CoalesceIntvl)\n\t\t\t\t\ttimerCh = timer.C")
    body = body.replace("\tvar timerCh <-chan time.Time", "\tdefer func() {\n\t\tif timer != nil {\n\t\t\ttimer.Stop()\n\t\t}\n\t}()\n\tvar timerCh <-chan time.Time")
    (ROOT / path).write_text(text[:start] + body + text[end:])
    replace(path, "func isTerminalPlatformEvent(kind events.Kind) bool {", "// Droppability is a congestion policy, not permission to rewrite an event.\n// Reasoning keeps its type and payload when admitted; only text-compatible\n// deltas use the legacy text coalescer.\nfunc isCoalesciblePlatformEvent(kind events.Kind) bool {\n\treturn kind == events.MessageDelta || kind == events.Raw\n}\n\nfunc isTerminalPlatformEvent(kind events.Kind) bool {")

    path = "internal/messaging/dedup.go"
    replace(path, "\tcloseOnce  sync.Once", "\tcloseOnce  sync.Once\n\tstartOnce  sync.Once")
    replace(path, "\t\tttl:        ttl,", "\t\tttl:        ttl,\n\t\tdone:       make(chan struct{}),")
    replace(path, "\td.done = make(chan struct{})\n\tgo d.cleanupLoop()", "\td.startOnce.Do(func() {\n\t\tselect {\n\t\tcase <-d.done:\n\t\t\treturn\n\t\tdefault:\n\t\t\tgo d.cleanupLoop()\n\t\t}\n\t})")
    replace(path, "\tif _, seen := d.entries[id]; seen {\n\t\treturn nil, false\n\t}", "\tnow := time.Now()\n\tif entry, seen := d.entries[id]; seen {\n\t\tif now.Before(entry.recordedAt.Add(d.ttl)) {\n\t\t\treturn nil, false\n\t\t}\n\t\tdelete(d.entries, id)\n\t\td.removeOrderEntryLocked(id, entry.handle)\n\t}")
    replace(path, "dedupEntry{recordedAt: time.Now(), handle: handle}", "dedupEntry{recordedAt: now, handle: handle}")
    replace(path, "\tticker := time.NewTicker(d.ttl / 2)", "\t// Positive TTLs smaller than 2ns must not produce a zero ticker period.\n\tinterval := max(d.ttl/2, time.Nanosecond)\n\tticker := time.NewTicker(interval)")

    path = "internal/messaging/feishu/chat_queue.go"
    old = '''\t\t\tctx, cancel := context.WithTimeout(context.Background(), chatTaskTimeout)
\t\t\tw.mu.Lock()
\t\t\tw.cancel = cancel
\t\t\tw.mu.Unlock()

\t\t\tif err := task(ctx); err != nil && q.log != nil {
\t\t\t\tif ctx.Err() != nil {
\t\t\t\t\tq.log.Warn("feishu: chat queue task timed out", "chat_id", chatID, "err", err)
\t\t\t\t} else {
\t\t\t\t\tq.log.Warn("feishu: chat queue task error", "chat_id", chatID, "err", err)
\t\t\t\t}
\t\t\t}
\t\t\tcancel()'''
    replace(path, old, "\t\t\tq.runTask(chatID, w, task)")
    helper = '''// runTask contains failures to one accepted task. Recovering only in
// runWorker would retire the worker and strand the rest of its accepted queue.
func (q *ChatQueue) runTask(chatID string, w *chatWorker, task func(context.Context) error) {
    ctx, cancel := context.WithTimeout(context.Background(), chatTaskTimeout)
    w.mu.Lock()
    w.cancel = cancel
    w.mu.Unlock()
    defer func() {
        cancel()
        w.mu.Lock()
        w.cancel = nil
        w.mu.Unlock()
        if r := recover(); r != nil && q.log != nil {
            q.log.Error("feishu: panic in chat queue task", "chat_id", chatID, "panic", r, "stack", string(debug.Stack()))
        }
    }()
    if err := task(ctx); err != nil && q.log != nil {
        if ctx.Err() != nil {
            q.log.Warn("feishu: chat queue task timed out", "chat_id", chatID, "err", err)
        } else {
            q.log.Warn("feishu: chat queue task error", "chat_id", chatID, "err", err)
        }
    }
}

'''
    replace(path, "// tryRemoveIdleWorker retires w only when", helper + "// tryRemoveIdleWorker retires w only when")

    path = "internal/worker/opencodeserver/worker.go"
    replace(path, '\tmsgURL := fmt.Sprintf("%s/session/%s/prompt_async", c.httpAddr, url.PathEscape(sessionID))', '\tmsgURL := fmt.Sprintf("%s/session/%s/prompt_async", c.httpAddr, url.PathEscape(sessionID))\n\tif c.projectDir != "" {\n\t\tmsgURL += "?directory=" + url.QueryEscape(c.projectDir)\n\t}')

    write("internal/worker/base/event_gate.go", r'''
package base

import (
    "sync"
    "time"

    "github.com/hrygo/hotplex/pkg/events"
)

// EventGate serializes sends with receive-channel closure without sharing a
// worker's stdin lock. Its zero value is ready to use. A gate owns exactly one
// channel for its entire lifetime and must not be copied or reused after Close.
// Close first wakes blocked senders, then waits for in-flight sends before
// closing the channel. Panic recovery is not a substitute for this ordering.
type EventGate struct {
    initOnce sync.Once
    closeOnce sync.Once
    mu sync.RWMutex
    done chan struct{}
    closed bool
}

func (g *EventGate) init() {
    g.initOnce.Do(func() { g.done = make(chan struct{}) })
}

// TrySend returns false when the channel is full or shutdown has started.
func (g *EventGate) TrySend(ch chan *events.Envelope, env *events.Envelope) bool {
    g.init()
    g.mu.RLock()
    defer g.mu.RUnlock()
    if g.closed { return false }
    select { case <-g.done: return false; default: }
    select {
    case ch <- env: return true
    case <-g.done: return false
    default: return false
    }
}

// SendTimeout preserves critical-event budgets while allowing immediate shutdown.
func (g *EventGate) SendTimeout(ch chan *events.Envelope, env *events.Envelope, timeout time.Duration) bool {
    if g.TrySend(ch, env) { return true }
    g.mu.RLock()
    defer g.mu.RUnlock()
    if g.closed { return false }
    timer := time.NewTimer(timeout)
    defer timer.Stop()
    select {
    case ch <- env: return true
    case <-g.done: return false
    case <-timer.C: return false
    }
}

// Close is idempotent and leaves accepted buffered events available to readers.
func (g *EventGate) Close(ch chan *events.Envelope) {
    g.init()
    g.closeOnce.Do(func() {
        close(g.done)
        g.mu.Lock()
        defer g.mu.Unlock()
        g.closed = true
        if ch != nil { close(ch) }
    })
}
''')
    path = "internal/worker/base/conn.go"
    replace(path, "\trecvCh    chan *events.Envelope", "\trecvCh    chan *events.Envelope\n\trecvGate  EventGate")
    old = '''func (c *Conn) TrySend(env *events.Envelope) bool {
\tselect {
\tcase c.recvCh <- env:
\t\treturn true
\tdefault:
\t\treturn false
\t}
}'''
    replace(path, old, '''func (c *Conn) TrySend(env *events.Envelope) bool {
    return c.recvGate.TrySend(c.recvCh, env)
}''')
    replace(path, "\tInjectWithTimeout(c.recvCh, env, c.log, c.sessionID)", "\tif !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) {\n\t\tc.log.Warn(\"base: inject failed, channel closed or full\",\n\t\t\t\"session_id\", c.SessionID(), \"event_type\", env.Event.Type)\n\t}")
    replace(path, "\tclose(c.recvCh)", "\tc.recvGate.Close(c.recvCh)")
    replace(path, "func (c *Conn) SessionID() string {\n\treturn c.sessionID", "func (c *Conn) SessionID() string {\n\tc.mu.Lock()\n\tdefer c.mu.Unlock()\n\treturn c.sessionID")
    replace(path, "func (c *Conn) StdinUnlocked() (*os.File, *sync.Mutex) {\n\treturn c.stdin, &c.mu", "func (c *Conn) StdinUnlocked() (*os.File, *sync.Mutex) {\n\tc.mu.Lock()\n\tdefer c.mu.Unlock()\n\treturn c.stdin, &c.mu")
    replace(path, "// The returned file is safe to read for nil-checking; concurrent Close may\n// set it to nil, so callers that need a stable snapshot should use StdinLocked\n// under the lock instead.", "// The pointer is snapshotted under the mutex. CloseInput may subsequently\n// close the file; callers must still hold the returned mutex while writing.")

    path = "internal/worker/acp/conn.go"
    replace(path, "\trecvCh    chan *events.Envelope", "\trecvCh    chan *events.Envelope\n\trecvGate  base.EventGate")
    start = (ROOT / path).read_text()
    a = start.index("func (c *acpConn) trySendNonBlocking(")
    b = start.index("// Close shuts down", a)
    (ROOT / path).write_text(start[:a] + '''func (c *acpConn) trySendNonBlocking(env *events.Envelope) bool {
    return c.recvGate.TrySend(c.recvCh, env)
}

// safeSend keeps the critical-event budget, but Close wakes blocked sends before
// closing recvCh. No send/close race is hidden behind panic recovery.
func (c *acpConn) safeSend(env *events.Envelope) bool {
    return c.recvGate.SendTimeout(c.recvCh, env, 5*time.Second)
}

''' + start[b:])
    replace(path, "\tclose(c.recvCh)", "\tc.recvGate.Close(c.recvCh)")
    replace(path, "\tbase.InjectWithTimeout(c.recvCh, env, c.log, c.sessionID)", "\tif !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) && c.log != nil {\n\t\tc.log.Warn(\"acp conn: inject failed, channel closed or full\",\n\t\t\t\"session_id\", c.sessionID, \"event_type\", env.Event.Type)\n\t}")
    replace(path, "// All channel sends are protected by recover() to prevent send-on-closed-channel panics\n// during the shutdown race between TrySend and Close.", "// EventGate synchronizes channel sends with Close, including blocked senders.")
    replace(path, "// trySendNonBlocking attempts a non-blocking send with panic recovery.\n// Returns false if the channel is full, closed, or a panic was recovered.", "// trySendNonBlocking returns false if the channel is full or closing.")

    write("internal/worker/base/event_gate_test.go", r'''
package base

import (
    "sync"
    "testing"
    "time"

    "github.com/hrygo/hotplex/pkg/events"
    "github.com/stretchr/testify/require"
)

func TestEventGateCloseUnblocksAndPreservesBufferedEvents(t *testing.T) {
    t.Parallel()
    var gate EventGate
    ch := make(chan *events.Envelope, 1)
    first := &events.Envelope{ID: "accepted"}
    require.True(t, gate.TrySend(ch, first))
    result := make(chan bool, 1)
    go func() { result <- gate.SendTimeout(ch, &events.Envelope{}, time.Minute) }()
    gate.Close(ch)
    select {
    case sent := <-result: require.False(t, sent)
    case <-time.After(time.Second): t.Fatal("blocked sender outlived Close")
    }
    require.Same(t, first, <-ch)
    _, open := <-ch
    require.False(t, open)
    require.False(t, gate.TrySend(ch, first))
    require.NotPanics(t, func() { gate.Close(ch) })
}

func TestEventGateConcurrentSendClose(t *testing.T) {
    t.Parallel()
    for i := 0; i < 100; i++ {
        var gate EventGate
        ch := make(chan *events.Envelope, 8)
        var wg sync.WaitGroup
        start := make(chan struct{})
        wg.Add(2)
        go func() {
            defer wg.Done()
            <-start
            for j := 0; j < 16; j++ { gate.TrySend(ch, &events.Envelope{}) }
        }()
        go func() { defer wg.Done(); <-start; gate.Close(ch) }()
        close(start)
        wg.Wait()
    }
}
''')


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["tests", "fixes"])
    args = parser.parse_args()
    {"tests": tests, "fixes": fixes}[args.mode]()
