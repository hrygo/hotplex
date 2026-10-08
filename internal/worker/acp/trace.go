package acp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var activeTraceFiles = struct {
	mu    sync.Mutex
	paths map[string]int
}{paths: make(map[string]int)}

// TraceWriter logs all JSON-RPC messages to a JSONL trace file for debugging.
// Enabled via acp.debug: true in config.yaml.
//
// File location: {dir}/acp-trace-{base(sessionID)}.jsonl
// Each line: {"ts":"...","dir":"→|←","msg":{...}}
// Rotation: when file exceeds maxSize, renamed to .1 and a new file is created.
type TraceWriter struct {
	mu           sync.Mutex
	file         *os.File
	path         string
	maxSize      int64
	writtenBytes int64
	registered   bool
}

// NewTraceWriter creates a trace writer that appends to a JSONL file.
func NewTraceWriter(dir, sessionID string) (*TraceWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("acp trace: create dir: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(dir, "acp-trace-"+filepath.Base(sessionID)+".jsonl"))
	if err != nil {
		return nil, fmt.Errorf("acp trace: resolve file path: %w", err)
	}
	registerTraceFile(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		unregisterTraceFile(path)
		return nil, fmt.Errorf("acp trace: open file: %w", err)
	}
	// Initialize byte counter from existing file size so rotation works
	// correctly after process restart with a pre-existing trace file.
	var written int64
	if info, statErr := f.Stat(); statErr == nil {
		written = info.Size()
	}
	return &TraceWriter{
		file:         f,
		path:         path,
		maxSize:      50 * 1024 * 1024, // 50 MB
		writtenBytes: written,
		registered:   true,
	}, nil
}

func tracePathKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func registerTraceFile(path string) {
	key := tracePathKey(path)
	activeTraceFiles.mu.Lock()
	activeTraceFiles.paths[key]++
	activeTraceFiles.mu.Unlock()
}

func unregisterTraceFile(path string) {
	key := tracePathKey(path)
	activeTraceFiles.mu.Lock()
	if activeTraceFiles.paths[key] <= 1 {
		delete(activeTraceFiles.paths, key)
	} else {
		activeTraceFiles.paths[key]--
	}
	activeTraceFiles.mu.Unlock()
}

// PruneExpiredTraceFiles removes old HotPlex ACP trace files and rotated
// backups. Active writers are protected from unlinking; other files in the
// directory are never considered.
func PruneExpiredTraceFiles(dir string, now time.Time, retention time.Duration, limit int) (int, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("acp trace: retention must be positive")
	}
	if limit <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("acp trace: read trace directory: %w", err)
	}

	cutoff := now.Add(-retention)
	deleted := 0
	for _, entry := range entries {
		if deleted >= limit || entry.IsDir() || !isACPTraceName(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		key := tracePathKey(path)

		// Hold the registry lock through removal so a new writer cannot open
		// the same path between the active check and os.Remove.
		activeTraceFiles.mu.Lock()
		if activeTraceFiles.paths[key] > 0 {
			activeTraceFiles.mu.Unlock()
			continue
		}
		info, err := entry.Info()
		if err != nil {
			activeTraceFiles.mu.Unlock()
			return deleted, fmt.Errorf("acp trace: stat trace file: %w", err)
		}
		if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			activeTraceFiles.mu.Unlock()
			continue
		}
		if err := os.Remove(path); err != nil {
			activeTraceFiles.mu.Unlock()
			return deleted, fmt.Errorf("acp trace: remove expired trace: %w", err)
		}
		activeTraceFiles.mu.Unlock()
		deleted++
	}
	return deleted, nil
}

func isACPTraceName(name string) bool {
	if !strings.HasPrefix(name, "acp-trace-") {
		return false
	}
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.1")
}

// Log writes a single trace entry. Safe to call on a nil TraceWriter (no-op).
// Uses a byte counter under mutex to track size, avoiding Stat() syscall on every call.
func (tw *TraceWriter) Log(direction string, msg any) {
	if tw == nil {
		return
	}
	// Marshal outside the lock to reduce contention.
	line, err := json.Marshal(map[string]any{
		"ts":  time.Now().Format(time.RFC3339Nano),
		"dir": direction,
		"msg": msg,
	})
	if err != nil {
		return
	}
	line = append(line, '\n')

	tw.mu.Lock()
	if tw.file == nil {
		tw.mu.Unlock()
		return
	}
	_, _ = tw.file.Write(line)
	tw.writtenBytes += int64(len(line))

	// Check rotation using byte counter.
	if tw.writtenBytes >= tw.maxSize {
		tw.writtenBytes = 0
		tw.rotateLocked()
	}
	tw.mu.Unlock()
}

// rotateLocked performs file rotation. Caller must hold tw.mu.
// On failure, tw.file is set to nil so subsequent Log() calls degrade gracefully.
func (tw *TraceWriter) rotateLocked() {
	if err := tw.file.Close(); err != nil {
		tw.file = nil
		tw.unregisterLocked()
		return
	}
	rotated := tw.path + ".1"
	_ = os.Remove(rotated)
	if err := os.Rename(tw.path, rotated); err != nil {
		slog.Warn("acp trace: rename failed, trace data lost", "err", err, "path", tw.path)
		tw.file = nil
		tw.unregisterLocked()
		return
	}
	f, err := os.OpenFile(tw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.Warn("acp trace: reopen failed after rotation", "err", err, "path", tw.path)
		tw.file = nil
		tw.unregisterLocked()
		return
	}
	tw.file = f
}

func (tw *TraceWriter) unregisterLocked() {
	if !tw.registered {
		return
	}
	unregisterTraceFile(tw.path)
	tw.registered = false
}

// Close flushes and closes the trace file.
func (tw *TraceWriter) Close() error {
	if tw == nil {
		return nil
	}
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.file == nil {
		tw.unregisterLocked()
		return nil
	}
	err := tw.file.Close()
	tw.file = nil
	tw.unregisterLocked()
	return err
}

// Rotate forces a rotation. Safe to call on nil TraceWriter.
// Rotation is also triggered automatically by Log() when the file exceeds maxSize.
func (tw *TraceWriter) Rotate() {
	if tw == nil {
		return
	}
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.file == nil {
		return
	}
	tw.writtenBytes = 0
	tw.rotateLocked()
}

// Path returns the current trace file path (for diagnostics).
func (tw *TraceWriter) Path() string {
	if tw == nil {
		return ""
	}
	return tw.path
}
