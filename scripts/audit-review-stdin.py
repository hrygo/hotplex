#!/usr/bin/env python3
"""Keep metadata snapshots race-free without acquiring the I/O write mutex."""
import argparse
from hardening import write, replace


def tests() -> None:
    write('internal/worker/base/stdin_snapshot_audit_test.go', r'''
package base

import (
    "os"
    "sync"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestAuditStdinSnapshotDoesNotWaitForPipeWriter(t *testing.T) {
    t.Parallel()
    c := NewConn(nil, nil, "user", "session")
    defer c.Close()
    c.WriteMu().Lock()
    got := make(chan struct{})
    go func() {
        c.StdinUnlocked()
        _ = c.SessionID()
        close(got)
    }()
    timely := false
    select {
    case <-got: timely = true
    case <-time.After(time.Second):
    }
    // Release before asserting so a defective getter does not leak its goroutine.
    c.WriteMu().Unlock()
    <-got
    require.True(t, timely, "metadata snapshot blocked before context-guarded I/O could start")
}

func TestAuditStdinSnapshotConcurrentCloseInput(t *testing.T) {
    t.Parallel()
    for i := 0; i < 32; i++ {
        reader, writer, err := os.Pipe()
        require.NoError(t, err)
        c := NewConn(nil, writer, "user", "session")
        start := make(chan struct{})
        var wg sync.WaitGroup
        wg.Add(2)
        go func() {
            defer wg.Done()
            <-start
            for j := 0; j < 256; j++ { c.StdinUnlocked() }
        }()
        go func() { defer wg.Done(); <-start; _ = c.CloseInput() }()
        close(start)
        wg.Wait()
        stdin, mu := c.StdinUnlocked()
        require.Nil(t, stdin)
        require.Same(t, c.WriteMu(), mu)
        require.NoError(t, c.Close())
        require.NoError(t, reader.Close())
    }
}
''')


def fixes() -> None:
    path = 'internal/worker/base/conn.go'
    replace(path, '\tstdin     *os.File', '\tstdin     *os.File\n\t// stateMu protects sessionID and stdin pointer snapshots, never pipe I/O.\n\t// Pointer mutation holds mu then stateMu; readers using mu alone remain safe.\n\tstateMu sync.RWMutex')
    replace(path, '''func (c *Conn) StdinUnlocked() (*os.File, *sync.Mutex) {
\tc.mu.Lock()
\tdefer c.mu.Unlock()
\treturn c.stdin, &c.mu
}''', '''func (c *Conn) StdinUnlocked() (*os.File, *sync.Mutex) {
    c.stateMu.RLock()
    defer c.stateMu.RUnlock()
    return c.stdin, &c.mu
}''')
    replace(path, '''func (c *Conn) SessionID() string {
\tc.mu.Lock()
\tdefer c.mu.Unlock()
\treturn c.sessionID
}''', '''func (c *Conn) SessionID() string {
    c.stateMu.RLock()
    defer c.stateMu.RUnlock()
    return c.sessionID
}''')
    replace(path, '''func (c *Conn) SetSessionID(id string) {
\tc.mu.Lock()
\tdefer c.mu.Unlock()
\tc.sessionID = id
}''', '''func (c *Conn) SetSessionID(id string) {
    c.stateMu.Lock()
    defer c.stateMu.Unlock()
    c.sessionID = id
}''')
    replace(path, '''func (c *Conn) CloseInput() error {
\tc.mu.Lock()
\tdefer c.mu.Unlock()
\tif c.stdin != nil {
\t\terr := c.stdin.Close()
\t\tc.stdin = nil
\t\treturn err
\t}
\treturn nil
}''', '''func (c *Conn) CloseInput() error {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.stateMu.Lock()
    stdin := c.stdin
    c.stdin = nil
    c.stateMu.Unlock()
    if stdin != nil {
        return stdin.Close()
    }
    return nil
}''')
    replace(path, '''// The pointer is snapshotted under the mutex. CloseInput may subsequently
// close the file; callers must still hold the returned mutex while writing.''', '''// The pointer snapshot uses a separate, short-lived state lock. It must not
// wait behind an orphaned pipe write holding mu before WriteWithCtx can apply
// the caller's cancellation budget. CloseInput may subsequently close the file;
// callers must still hold the returned write mutex while writing.''')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['tests', 'fixes'])
    args = parser.parse_args()
    {'tests': tests, 'fixes': fixes}[args.mode]()
