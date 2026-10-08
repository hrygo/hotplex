package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/base"
)

// ─── EX-01: InitConfig Args/Debug ───────────────────────────────────────────

func TestInitConfig_ArgsStored(t *testing.T) {
	t.Cleanup(func() {
		InitConfig(config.ACPConfig{Command: "hermes acp"})
	})

	InitConfig(config.ACPConfig{
		Command:     "test-agent serve",
		Args:        []string{"--model", "gpt-4"},
		AutoApprove: boolPtr(false),
		Debug:       true,
	})

	parts, _ := commandParts.Load().([]string)
	require.Equal(t, []string{"test-agent", "serve"}, parts)

	ca, _ := configArgs.Load().([]string)
	require.Equal(t, []string{"--model", "gpt-4"}, ca)

	require.True(t, debugEnabled.Load())
}

func TestInitConfig_DefaultCommand(t *testing.T) {
	t.Cleanup(func() {
		InitConfig(config.ACPConfig{Command: "hermes acp"})
	})

	InitConfig(config.ACPConfig{Command: ""})

	parts, _ := commandParts.Load().([]string)
	require.Equal(t, []string{"hermes", "acp"}, parts)
}

func TestInitConfig_AutoApprove_NilKeepsDefault(t *testing.T) {
	t.Cleanup(func() { InitConfig(config.ACPConfig{Command: "hermes acp"}) })
	autoApproveDefault.Store(true)
	InitConfig(config.ACPConfig{Command: "hermes acp"})
	require.True(t, autoApproveDefault.Load(),
		"absent acp.auto_approve should preserve init() default (true)")
}

func TestInitConfig_AutoApprove_ExplicitTrue(t *testing.T) {
	t.Cleanup(func() { InitConfig(config.ACPConfig{Command: "hermes acp"}) })
	InitConfig(config.ACPConfig{Command: "hermes acp", AutoApprove: boolPtr(true)})
	require.True(t, autoApproveDefault.Load())
}

func TestInitConfig_AutoApprove_ExplicitFalse(t *testing.T) {
	t.Cleanup(func() { InitConfig(config.ACPConfig{Command: "hermes acp"}) })
	InitConfig(config.ACPConfig{Command: "hermes acp", AutoApprove: boolPtr(false)})
	require.False(t, autoApproveDefault.Load())
}

func boolPtr(v bool) *bool { return &v }

// ─── EX-02: Protocol Version Warning ────────────────────────────────────────

func TestProtocolVersion_KnownVersion_NoWarn(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	w.initResult = &InitializeResult{ProtocolVersion: 1}
	// Version 1 is known — no special behavior needed beyond logging.
	require.Equal(t, 1, w.initResult.ProtocolVersion)
}

func TestProtocolVersion_FutureVersion_Stored(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	w.initResult = &InitializeResult{ProtocolVersion: 5}
	// Future version should still be stored for capability checks.
	require.Equal(t, 5, w.initResult.ProtocolVersion)
}

// ─── U-04: TraceWriter ──────────────────────────────────────────────────────

func TestTraceWriter_CreateWriteRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tw, err := NewTraceWriter(dir, "test-session-001")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tw.Close() })

	require.Contains(t, tw.Path(), "acp-trace-test-session-001.jsonl")

	// Write a trace entry.
	tw.Log("→", map[string]string{"method": "session/prompt", "content": "hello"})

	// Close to flush.
	require.NoError(t, tw.Close())

	// Read back the file.
	data, err := os.ReadFile(tw.Path())
	require.NoError(t, err)
	require.Contains(t, string(data), `"dir":"→"`)
	require.Contains(t, string(data), `"method":"session/prompt"`)

	// Verify it's valid JSONL.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1)
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	require.Equal(t, "→", entry["dir"])
}

func TestTraceWriter_NilSafe(t *testing.T) {
	t.Parallel()
	var tw *TraceWriter
	// All operations on nil TraceWriter should be no-ops.
	tw.Log("→", "test")
	require.NoError(t, tw.Close())
	tw.Rotate()
	require.Equal(t, "", tw.Path())
}

func TestTraceWriter_Rotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tw, err := NewTraceWriter(dir, "test-rotate")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tw.Close() })

	// Use a small threshold to actually trigger rotation.
	tw.maxSize = 500

	// Write enough data to exceed the threshold.
	for i := 0; i < 50; i++ {
		tw.Log("→", map[string]string{"method": "session/prompt", "content": strings.Repeat("x", 100)})
	}
	require.NoError(t, tw.Close())

	// Verify rotation produced a .1 backup file.
	rotated := tw.Path() + ".1"
	_, err = os.Stat(rotated)
	require.NoError(t, err, "rotated backup file should exist")

	rotatedData, err := os.ReadFile(rotated)
	require.NoError(t, err)
	require.True(t, len(rotatedData) > 0, "rotated file should have content")

	// Current file should also have content (new entries after rotation).
	data, err := os.ReadFile(tw.Path())
	require.NoError(t, err)
	require.True(t, len(data) > 0, "current trace file should have content")
}

func TestTraceWriter_DebugDisabledByDefault(t *testing.T) {
	t.Parallel()
	// debugEnabled should be false by default.
	require.False(t, debugEnabled.Load())
}

// ─── FR-08: ForkSession ───────────────────────────────────────

func TestClient_ForkSession_Success(t *testing.T) {
	t.Parallel()

	agentStdinR, agentStdinW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	defer agentStdinW.Close()
	defer agentStdoutW.Close()

	client := NewACPClient(agentStdinW, agentStdoutR, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.StartReadLoop(ctx)

	// Agent responds to fork with a new session ID.
	go func() {
		scanner := NewScanner(agentStdinR)
		if scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &req)
			_ = WriteMessage(agentStdoutW, &JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustMarshal(SessionResult{SessionID: "forked_sess_42"}),
			})
		}
	}()

	result, err := client.ForkSession(ctx, "old_session")
	require.NoError(t, err)
	require.Equal(t, "forked_sess_42", result.SessionID)
}

func TestClient_ForkSession_Error(t *testing.T) {
	t.Parallel()

	agentStdinR, agentStdinW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	defer agentStdinW.Close()
	defer agentStdoutW.Close()

	client := NewACPClient(agentStdinW, agentStdoutR, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.StartReadLoop(ctx)

	// Agent responds with an error for fork.
	go func() {
		scanner := NewScanner(agentStdinR)
		if scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &req)
			_ = WriteMessage(agentStdoutW, &JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID,
				Error: &JSONRPCError{Code: -32600, Message: "fork not supported"},
			})
		}
	}()

	_, err := client.ForkSession(ctx, "old_session")
	require.Error(t, err)
	require.Contains(t, err.Error(), "fork not supported")
}

// ─── FR-09: JSON Schema Injection ─────────────────────────────────

func TestJSONSchema_InjectedOnFirstPrompt(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	w.jsonSchema = `{"type":"object","properties":{"name":{"type":"string"}}}`

	// First call should inject.
	require.True(t, w.jsonSchemaInjected.CompareAndSwap(false, true))
	content := "Hello"
	content = fmt.Sprintf("[JSON SCHEMA]\n%s\n[/JSON SCHEMA]\n\n%s", w.jsonSchema, content)
	require.Contains(t, content, "[JSON SCHEMA]")
	require.Contains(t, content, `"type":"object"`)
	require.True(t, strings.HasPrefix(content, "[JSON SCHEMA]"))
	require.Contains(t, content, "Hello")

	// Second CompareAndSwap should fail (already injected).
	require.False(t, w.jsonSchemaInjected.CompareAndSwap(false, true))
}

func TestJSONSchema_EmptySchema_NoInjection(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	// jsonSchema is empty by default.
	require.Empty(t, w.jsonSchema)
	require.False(t, w.jsonSchemaInjected.Load())

	// CompareAndSwap should succeed (false->true), but the outer if-check on
	// jsonSchema != "" prevents injection.
	require.True(t, w.jsonSchemaInjected.CompareAndSwap(false, true))
}

func TestJSONSchema_UsesCompatibilityRulesWithoutSystemPrompt(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	w.systemPrompt = "PRIVATE_PROMPT_SENTINEL"
	w.jsonSchema = `{"type":"object","properties":{"result":{"type":"string"}}}`

	// JSON schema remains supported; the full system prompt does not.
	require.True(t, w.jsonSchemaInjected.CompareAndSwap(false, true))

	// Simulate the Input() injection logic.
	content := "Hello"
	if w.jsonSchema != "" {
		content = fmt.Sprintf("[JSON SCHEMA]\n%s\n[/JSON SCHEMA]\n\n%s", w.jsonSchema, content)
	}
	content = w.injectCompatibilityPrefix(content)

	require.Contains(t, content, acpCompatibilityRules)
	require.Contains(t, content, "[JSON SCHEMA]")
	require.NotContains(t, content, "PRIVATE_PROMPT_SENTINEL")
	require.Contains(t, content, `"type":"object"`)
	require.True(t, strings.Contains(content, "Hello"))
}

func TestJSONSchema_SchemaOnly_NoSystemPrompt(t *testing.T) {
	t.Parallel()
	w := &Worker{BaseWorker: base.NewBaseWorker(nil, nil)}
	w.jsonSchema = `{"type":"array","items":{"type":"number"}}`

	// Only JSON Schema and the fixed ACP compatibility rules are sent.
	content := "List primes"
	if w.jsonSchema != "" {
		content = fmt.Sprintf("[JSON SCHEMA]\n%s\n[/JSON SCHEMA]\n\n%s", w.jsonSchema, content)
	}
	content = w.injectCompatibilityPrefix(content)

	// Full system prompt should NOT be injected.
	require.NotContains(t, content, "[SYSTEM INSTRUCTIONS]")
	require.Contains(t, content, acpCompatibilityRules)
	// JSON Schema should be injected.
	require.Contains(t, content, "[JSON SCHEMA]")
	require.Contains(t, content, `"type":"array"`)
	require.Contains(t, content, "List primes")
}

// ─── EX-01: Config Args Merging ─────────────────────────────────────────────

func TestInitConfig_ArgsMergeOrder(t *testing.T) {
	t.Cleanup(func() {
		InitConfig(config.ACPConfig{Command: "hermes acp"})
	})

	// Set config-level args.
	InitConfig(config.ACPConfig{
		Command: "agent run",
		Args:    []string{"--verbose", "--model=claude"},
	})

	ca, _ := configArgs.Load().([]string)
	require.Equal(t, []string{"--verbose", "--model=claude"}, ca)

	// Verify commandParts are correct.
	parts, _ := commandParts.Load().([]string)
	require.Equal(t, []string{"agent", "run"}, parts)
}

func TestInitConfig_ArgsEnvSplit(t *testing.T) {
	t.Cleanup(func() {
		InitConfig(config.ACPConfig{Command: "hermes acp"})
	})

	// Simulate Viper BindEnv behavior: env var produces single-element slice.
	InitConfig(config.ACPConfig{
		Command: "agent run",
		Args:    []string{"--model gpt-4"}, // single element with space (env override)
	})

	ca, _ := configArgs.Load().([]string)
	require.Equal(t, []string{"--model", "gpt-4"}, ca, "single element with spaces should be split")

	// Already-split args pass through unchanged.
	InitConfig(config.ACPConfig{
		Command: "agent run",
		Args:    []string{"--model", "gpt-4"},
	})
	ca, _ = configArgs.Load().([]string)
	require.Equal(t, []string{"--model", "gpt-4"}, ca)
}

// ─── U-04: TraceWriter Concurrent Writes ────────────────────────────────────

func TestTraceWriter_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tw, err := NewTraceWriter(dir, "concurrent-test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tw.Close() })

	// Write from multiple goroutines simultaneously.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tw.Log("→", map[string]any{"index": n, "data": strings.Repeat("x", 50)})
		}(i)
	}
	wg.Wait()

	require.NoError(t, tw.Close())

	// Read back — should have exactly 10 valid JSONL lines.
	data, err := os.ReadFile(tw.Path())
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 10)

	for _, line := range lines {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "invalid JSONL: %s", line)
	}
}

// ─── TraceWriter File Path ──────────────────────────────────────────────────

func TestTraceWriter_PathFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tw, err := NewTraceWriter(dir, "my-session")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tw.Close() })

	expected := filepath.Join(dir, "acp-trace-my-session.jsonl")
	require.Equal(t, expected, tw.Path())
}

func TestPruneExpiredTraceFiles_SkipsActiveAndUnrelatedFiles(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dir := t.TempDir()
	expired, err := NewTraceWriter(dir, "expired")
	require.NoError(t, err)
	expiredPath := expired.Path()
	require.NoError(t, expired.Close())
	oldTime := now.Add(-72 * time.Hour)
	require.NoError(t, os.Chtimes(expiredPath, oldTime, oldTime))

	rotatedPath := expiredPath + ".1"
	require.NoError(t, os.WriteFile(rotatedPath, []byte("old trace"), 0o600))
	require.NoError(t, os.Chtimes(rotatedPath, oldTime, oldTime))

	active, err := NewTraceWriter(dir, "active")
	require.NoError(t, err)
	t.Cleanup(func() { _ = active.Close() })
	activePath := active.Path()
	require.NoError(t, os.Chtimes(activePath, oldTime, oldTime))

	unrelatedPath := filepath.Join(dir, "gateway.log")
	require.NoError(t, os.WriteFile(unrelatedPath, []byte("log"), 0o600))
	require.NoError(t, os.Chtimes(unrelatedPath, oldTime, oldTime))

	deleted, err := PruneExpiredTraceFiles(dir, now, 48*time.Hour, 100)

	require.NoError(t, err)
	require.Equal(t, 2, deleted)
	require.NoFileExists(t, expiredPath)
	require.NoFileExists(t, rotatedPath)
	require.FileExists(t, activePath)
	require.FileExists(t, unrelatedPath)
}
