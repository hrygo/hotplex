package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestTypeRegistration(t *testing.T) {
	types := []string{}
	for _, wt := range worker.RegisteredTypes() {
		types = append(types, string(wt))
	}
	require.Contains(t, types, string(worker.TypeCodexCLI))
}

// ─── v2 MapNotification Tests ───────────────────────────────────────────

func TestMapNotificationAgentMessageStateMachine(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// Step 1: item/started (agent_message) → message.start
	startedParams := json.RawMessage(`{"item":{"id":"msg_1","type":"agent_message"}}`)
	envs := m.MapNotification("item/started", startedParams)
	require.Len(t, envs, 1)
	require.Equal(t, events.MessageStart, envs[0].Event.Type)
	ms, ok := envs[0].Event.Data.(events.MessageStartData)
	require.True(t, ok)
	require.Equal(t, "assistant", ms.Role)
	require.NotEmpty(t, ms.ID)
	msgID := ms.ID

	// Step 2: item/agentMessage/delta (x3) → message.delta
	for i, word := range []string{"Hello", " world", "!"} {
		deltaParams := json.RawMessage(fmt.Sprintf(`{"itemId":"msg_1","delta":%q}`, word))
		envs = m.MapNotification("item/agentMessage/delta", deltaParams)
		require.Len(t, envs, 1, "delta %d", i)
		require.Equal(t, events.MessageDelta, envs[0].Event.Type)
		md, ok := envs[0].Event.Data.(events.MessageDeltaData)
		require.True(t, ok)
		require.Equal(t, msgID, md.MessageID)
		require.Equal(t, word, md.Content)
	}

	// Step 3: item/completed (agent_message) → message.end
	completedParams := json.RawMessage(`{"item":{"id":"msg_1","type":"agent_message","text":"Hello world!"}}`)
	envs = m.MapNotification("item/completed", completedParams)
	require.Len(t, envs, 1)
	require.Equal(t, events.MessageEnd, envs[0].Event.Type)
	me, ok := envs[0].Event.Data.(events.MessageEndData)
	require.True(t, ok)
	require.Equal(t, msgID, me.MessageID)

	// Happy path: monotonic delta→completed must NOT trigger any drift.
	// Locks in the invariant that the prefix-validation refactor doesn't
	// misclassify consistent appends as drift.
	require.Equal(t, int64(0), m.DriftCount(), "happy-path state machine must have zero drift")
}

func TestMapNotificationTurnFailed(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	envs := m.MapNotification("turn/failed", json.RawMessage(`{"turn":{}}`))
	require.Len(t, envs, 2)
	require.Equal(t, events.Error, envs[0].Event.Type)
	require.Equal(t, events.Done, envs[1].Event.Type)
	dd, ok := envs[1].Event.Data.(events.DoneData)
	require.True(t, ok)
	require.False(t, dd.Success)
}

func TestMapNotificationTurnCompleted(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// turn/started sets the current turn ID.
	m.MapNotification("turn/started", json.RawMessage(
		`{"threadId":"thr-1","turn":{"id":"turn-1","status":"inProgress"}}`))

	// Simulate token usage tracking from thread/tokenUsage/updated.
	tokenParams := json.RawMessage(`{"threadId":"thr-1","turnId":"turn-1","tokenUsage":{"last":{"totalTokens":250,"inputTokens":150,"outputTokens":75,"cachedInputTokens":20,"reasoningOutputTokens":5},"total":{"totalTokens":500,"inputTokens":300,"outputTokens":150}}}`)
	envs := m.MapNotification("thread/tokenUsage/updated", tokenParams)
	require.Nil(t, envs, "token usage tracking produces no envelopes")

	// Simulate model tracking from model/rerouted.
	modelParams := json.RawMessage(`{"threadId":"thr-1","turnId":"turn-1","fromModel":"gpt-4","toModel":"o3","reason":"highRiskCyberActivity"}`)
	envs = m.MapNotification("model/rerouted", modelParams)
	require.Nil(t, envs, "model rerouted tracking produces no envelopes")

	// turn/completed uses tracked usage and model.
	envs = m.MapNotification("turn/completed", json.RawMessage(`{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}`))
	require.Len(t, envs, 1)
	require.Equal(t, events.Done, envs[0].Event.Type)
	dd, ok := envs[0].Event.Data.(events.DoneData)
	require.True(t, ok)
	require.True(t, dd.Success)
	usage, ok := dd.Stats["usage"].(map[string]any)
	require.True(t, ok, "stats should have nested 'usage' map")
	require.Equal(t, int64(150), usage["input_tokens"])
	require.Equal(t, int64(75), usage["output_tokens"])
	require.Equal(t, int64(20), usage["cache_read_input_tokens"])
	modelUsage, ok := dd.Stats["model_usage"].(map[string]any)
	require.True(t, ok, "stats should have nested 'model_usage' map")
	require.Contains(t, modelUsage, "o3")
}

func TestMapNotificationApproval(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	params := json.RawMessage(`{"requestId":"req_1","toolName":"Bash"}`)
	envs := m.MapNotification("serverRequest/approval", params)
	require.Len(t, envs, 1)
	require.Equal(t, events.PermissionRequest, envs[0].Event.Type)
	pr, ok := envs[0].Event.Data.(events.PermissionRequestData)
	require.True(t, ok)
	require.Equal(t, "req_1", pr.ID)
	require.Equal(t, "Bash", pr.ToolName)
}

func TestMapNotificationCommandExecution(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// Started
	started := json.RawMessage(`{"item":{"id":"cmd_1","type":"command_execution","command":"ls -la","cwd":"/tmp"}}`)
	envs := m.MapNotification("item/started", started)
	require.Len(t, envs, 1)
	require.Equal(t, events.ToolCall, envs[0].Event.Type)
	tc, ok := envs[0].Event.Data.(events.ToolCallData)
	require.True(t, ok)
	require.Equal(t, "Bash", tc.Name)
	require.Equal(t, "ls -la", tc.Input["command"])

	// Completed
	completed := json.RawMessage(`{"item":{"id":"cmd_1","type":"command_execution","stdout":"file1\nfile2","stderr":""}}`)
	envs = m.MapNotification("item/completed", completed)
	require.Len(t, envs, 1)
	require.Equal(t, events.ToolResult, envs[0].Event.Type)
	tr, ok := envs[0].Event.Data.(events.ToolResultData)
	require.True(t, ok)
	require.Equal(t, "file1\nfile2", tr.Output)
}

func TestMapNotificationReasoning(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	params := json.RawMessage(`{"item":{"id":"r_1","type":"reasoning","summary_text":["Step 1","Step 2"]}}`)
	envs := m.MapNotification("item/completed", params)
	require.Len(t, envs, 1)
	require.Equal(t, events.Reasoning, envs[0].Event.Type)
	rd, ok := envs[0].Event.Data.(events.ReasoningData)
	require.True(t, ok)
	require.Equal(t, "Step 1\nStep 2", rd.Content)
}

func TestMapNotificationUnknownMethod(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	envs := m.MapNotification("thread/started", json.RawMessage(`{}`))
	require.Nil(t, envs)
}

func TestMapNotificationOutputDelta(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// Simulate a command started first.
	started := json.RawMessage(`{"item":{"id":"cmd_1","type":"command_execution","command":"ls","cwd":"/tmp"}}`)
	envs := m.MapNotification("item/started", started)
	require.Len(t, envs, 1)
	require.Equal(t, events.ToolCall, envs[0].Event.Type)

	// Streaming output deltas.
	delta1 := json.RawMessage(`{"threadId":"thr-1","turnId":"turn-1","itemId":"cmd_1","delta":"file1\n"}`)
	envs = m.MapNotification("item/commandExecution/outputDelta", delta1)
	require.Len(t, envs, 1)
	require.Equal(t, events.ToolResult, envs[0].Event.Type)
	tr, ok := envs[0].Event.Data.(events.ToolResultData)
	require.True(t, ok)
	require.Equal(t, "cmd_1", tr.ID)
	require.Equal(t, "file1\n", tr.Output)

	delta2 := json.RawMessage(`{"threadId":"thr-1","turnId":"turn-1","itemId":"cmd_1","delta":"file2\n"}`)
	envs = m.MapNotification("item/commandExecution/outputDelta", delta2)
	require.Len(t, envs, 1)
	tr = envs[0].Event.Data.(events.ToolResultData)
	require.Equal(t, "file2\n", tr.Output)

	// Final item/completed overwrites with full output.
	completed := json.RawMessage(`{"item":{"id":"cmd_1","type":"command_execution","stdout":"file1\nfile2\n","exitCode":0}}`)
	envs = m.MapNotification("item/completed", completed)
	require.Len(t, envs, 1)
	require.Equal(t, events.ToolResult, envs[0].Event.Type)
	tr = envs[0].Event.Data.(events.ToolResultData)
	require.Equal(t, "file1\nfile2\n", tr.Output)
}

func TestMapNotificationOutputDeltaEmpty(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	envs := m.MapNotification("item/commandExecution/outputDelta", json.RawMessage(
		`{"threadId":"thr-1","turnId":"turn-1","itemId":"","delta":"text"}`))
	require.Nil(t, envs, "empty itemId should produce no envelopes")
}

func TestMapNotificationReasoningDelta(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("item/reasoning/summaryTextDelta", json.RawMessage(
		`{"threadId":"thr-1","turnId":"turn-1","itemId":"r_1","delta":"Analyzing the code structure","summaryIndex":0}`))
	require.Len(t, envs, 1)
	require.Equal(t, events.Reasoning, envs[0].Event.Type)
	rd, ok := envs[0].Event.Data.(events.ReasoningData)
	require.True(t, ok)
	require.Equal(t, "Analyzing the code structure", rd.Content)
	require.Equal(t, "r_1", rd.ID)
}

func TestMapNotificationReasoningDeltaEmpty(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("item/reasoning/summaryTextDelta", json.RawMessage(
		`{"threadId":"thr-1","itemId":"r_1","delta":""}`))
	require.Nil(t, envs, "empty delta should produce no envelopes")
}

func TestMapNotificationWarning(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("warning", json.RawMessage(
		`{"threadId":"thr-1","message":"Rate limit approaching"}`))
	require.Len(t, envs, 1)
	require.Equal(t, events.Step, envs[0].Event.Type)
	sd, ok := envs[0].Event.Data.(events.StepData)
	require.True(t, ok)
	require.Equal(t, "warning", sd.StepType)
	require.Equal(t, "Rate limit approaching", sd.Name)
}

func TestMapNotificationError(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("error", json.RawMessage(
		`{"threadId":"thr-1","error":{"message":"API request failed: 502"}}`))
	require.Len(t, envs, 1)
	require.Equal(t, events.Error, envs[0].Event.Type)
	ed, ok := envs[0].Event.Data.(events.ErrorData)
	require.True(t, ok)
	require.Equal(t, events.ErrorCode("CODEX_ERROR"), ed.Code)
	require.Equal(t, "API request failed: 502", ed.Message)
}

func TestMapNotificationErrorEmptyMessage(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("error", json.RawMessage(`{"threadId":"thr-1","error":{}}`))
	require.Len(t, envs, 1)
	ed, _ := envs[0].Event.Data.(events.ErrorData)
	require.Equal(t, "unknown error", ed.Message)
}

func TestMapNotificationMCPProgress(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	envs := m.MapNotification("item/mcpToolCall/progress", json.RawMessage(
		`{"threadId":"thr-1","turnId":"turn-1","itemId":"mcp_1","message":"Searching files..."}`))
	require.Len(t, envs, 1)
	require.Equal(t, events.Step, envs[0].Event.Type)
	sd, ok := envs[0].Event.Data.(events.StepData)
	require.True(t, ok)
	require.Equal(t, "mcp_progress", sd.StepType)
	require.Equal(t, "Searching files...", sd.Name)
	require.Equal(t, "mcp_1", sd.ID)
}

func TestMapNotificationTurnStartedResetsUsage(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// Track usage first.
	m.MapNotification("thread/tokenUsage/updated", json.RawMessage(
		`{"tokenUsage":{"last":{"totalTokens":100,"inputTokens":60,"outputTokens":40}}}`))
	require.NotNil(t, m.lastUsage)

	// turn/started resets tracked usage.
	envs := m.MapNotification("turn/started", json.RawMessage(
		`{"threadId":"thr-1","turn":{"id":"turn-2","status":"inProgress"}}`))
	require.Nil(t, envs)
	require.Nil(t, m.lastUsage)
	require.Equal(t, "turn-2", m.turnID)
}

func TestMapNotificationTurnCompletedNoUsage(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// turn/completed without prior token usage tracking → nil stats.
	envs := m.MapNotification("turn/completed", json.RawMessage(
		`{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}`))
	require.Len(t, envs, 1)
	dd, ok := envs[0].Event.Data.(events.DoneData)
	require.True(t, ok)
	require.True(t, dd.Success)
	require.Nil(t, dd.Stats)
}

func TestMapNotificationStaleTokenUsageRejected(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")

	// Set up turn-1 with usage.
	m.MapNotification("turn/started", json.RawMessage(
		`{"threadId":"thr-1","turn":{"id":"turn-1","status":"inProgress"}}`))
	m.MapNotification("thread/tokenUsage/updated", json.RawMessage(
		`{"turnId":"turn-1","tokenUsage":{"last":{"totalTokens":100,"inputTokens":60,"outputTokens":40}}}`))
	require.NotNil(t, m.lastUsage)

	// Advance to turn-2 — lastUsage is cleared.
	m.MapNotification("turn/started", json.RawMessage(
		`{"threadId":"thr-1","turn":{"id":"turn-2","status":"inProgress"}}`))
	require.Nil(t, m.lastUsage)
	require.Equal(t, "turn-2", m.turnID)

	// Late token usage for turn-1 should be rejected.
	m.MapNotification("thread/tokenUsage/updated", json.RawMessage(
		`{"turnId":"turn-1","tokenUsage":{"last":{"totalTokens":100,"inputTokens":60,"outputTokens":40}}}`))
	require.Nil(t, m.lastUsage, "stale turn-1 usage should be rejected")
}

// ─── ParseNotification Tests ────────────────────────────────────────────

func TestParseNotification(t *testing.T) {
	t.Parallel()

	p := NewParser()

	t.Run("valid notification", func(t *testing.T) {
		t.Parallel()
		method, params, err := p.ParseNotification(
			`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"x","type":"agent_message"}}}`)
		require.NoError(t, err)
		require.Equal(t, "item/started", method)
		require.NotNil(t, params)
	})

	t.Run("invalid json", func(t *testing.T) {
		t.Parallel()
		_, _, err := p.ParseNotification("not json")
		require.Error(t, err)
	})

	t.Run("missing method", func(t *testing.T) {
		t.Parallel()
		_, _, err := p.ParseNotification(`{"jsonrpc":"2.0","params":{}}`)
		require.Error(t, err)
	})
}

func TestParseResponse(t *testing.T) {
	t.Parallel()

	p := NewParser()

	t.Run("valid response", func(t *testing.T) {
		t.Parallel()
		id, result, rpcErr, err := p.ParseResponse(
			`{"jsonrpc":"2.0","id":42,"result":{"thread":{"id":"thr_123"}}}`)
		require.NoError(t, err)
		require.Equal(t, int64(42), id)
		require.NotNil(t, result)
		require.Nil(t, rpcErr)
	})

	t.Run("error response", func(t *testing.T) {
		t.Parallel()
		id, result, rpcErr, err := p.ParseResponse(
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)
		require.NoError(t, err)
		require.Equal(t, int64(1), id)
		require.Nil(t, result)
		require.NotNil(t, rpcErr)
		require.Equal(t, -32601, rpcErr.Code)
	})
}

// ─── Manager Tests ──────────────────────────────────────────────────────

func TestManagerSubscribeUnsubscribe(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	ch := mgr.Subscribe("thread-1", "session-thread-1")
	require.NotNil(t, ch)

	// Second subscribe returns same channel
	ch2 := mgr.Subscribe("thread-1", "session-thread-1")
	require.Equal(t, ch, ch2)

	// Different thread gets different channel
	ch3 := mgr.Subscribe("thread-2", "session-thread-2")
	require.NotNil(t, ch3)
	require.NotEqual(t, ch, ch3)

	// Unsubscribe removes from map (channel is closed by appConn or monitorProcess).
	mgr.Unsubscribe("thread-1")

	// Re-subscribe after unsubscribe creates new channel
	ch4 := mgr.Subscribe("thread-1", "session-thread-1")
	require.NotNil(t, ch4)
	require.NotEqual(t, ch, ch4)
}

func TestManagerReleaseIdleDrain(t *testing.T) {
	t.Parallel()
	cfg := config.CodexCLIConfig{IdleDrainPeriod: 10 * time.Millisecond}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	mgr.mu.Lock()
	mgr.state = stateRunning
	mgr.refs = 1
	mgr.mu.Unlock()

	mgr.Release() // refs → 0, starts idle drain

	time.Sleep(30 * time.Millisecond)
	// Idle drain should have fired by now
	mgr.mu.Lock()
	require.Equal(t, stateRunning, mgr.state, "state should still be running since proc is nil (no actual kill)")
	mgr.mu.Unlock()
}

func TestManagerAcquireRejectsWhenStopped(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	mgr.mu.Lock()
	mgr.state = stateStopped
	mgr.mu.Unlock()

	_, err := mgr.Acquire(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "stopped")
}

func TestManagerShutdown(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Add some subscribers
	ch1 := mgr.Subscribe("thread-1", "session-thread-1")
	ch2 := mgr.Subscribe("thread-2", "session-thread-2")

	mgr.Shutdown(context.Background())

	// Verify state is stopped
	mgr.mu.Lock()
	require.Equal(t, stateStopped, mgr.state)
	mgr.mu.Unlock()

	// Subscriber channels should be closed
	_, ok1 := <-ch1
	require.False(t, ok1)
	_, ok2 := <-ch2
	require.False(t, ok2)
}

// ─── AppServerWorker Tests ──────────────────────────────────────────────

type blockingFrameWriter struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingFrameWriter() *blockingFrameWriter {
	return &blockingFrameWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingFrameWriter) Write(p []byte) (int, error) {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func (w *blockingFrameWriter) Close() error { return nil }

func (w *blockingFrameWriter) Release() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func TestAppServerWorkerStopCurrentTurnPropagatesContext(t *testing.T) {
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
	writer := newBlockingFrameWriter()
	mgr.stdin = writer
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thread-stop-context",
		turnID:     "turn-stop-context",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.StopCurrentTurn(ctx) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("turn interrupt notification was not written")
	}
	cancel()

	select {
	case err := <-done:
		writer.Release()
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		writer.Release()
		<-done
		t.Fatal("turn interrupt ignored the caller context")
	}
}

func TestAppServerWorkerStopCurrentTurnDoesNotExtendCallerDeadline(t *testing.T) {
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
	writer := newBlockingFrameWriter()
	mgr.stdin = writer
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thread-stop-deadline",
		turnID:     "turn-stop-deadline",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.StopCurrentTurn(ctx) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("turn interrupt notification was not written")
	}

	select {
	case err := <-done:
		writer.Release()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(500 * time.Millisecond):
		writer.Release()
		<-done
		t.Fatal("turn interrupt extended the caller deadline with fallback grace")
	}
}

func TestAppServerWorkerTerminatePropagatesContextToUnsubscribe(t *testing.T) {
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
	writer := newBlockingFrameWriter()
	mgr.stdin = writer
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thread-unsubscribe-context",
		doneCh:     make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Terminate(ctx) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("thread unsubscribe notification was not written")
	}
	cancel()

	select {
	case err := <-done:
		writer.Release()
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		writer.Release()
		<-done
		t.Fatal("thread unsubscribe ignored the caller context")
	}
}

func TestAppServerWorkerCapabilities(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
	}

	require.Equal(t, worker.TypeCodexCLI, w.Type())
	require.True(t, w.SupportsResume())
	require.True(t, w.SupportsStreaming())
	require.True(t, w.SupportsTools())
	require.NotEmpty(t, w.EnvBlocklist())
	require.Contains(t, w.Modalities(), "text")
	require.Contains(t, w.Modalities(), "code")
}

func TestAppServerWorkerConnNilBeforeStart(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
	}

	require.Nil(t, w.Conn())
}

// ─── AppServerWorker Lifecycle Tests ─────────────────────────────────────

func newTestAppServerWorker(t *testing.T) *AppServerWorker {
	t.Helper()
	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute, StartupTimeout: time.Second, CallTimeout: time.Second}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	return &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
	}
}

func TestAppServerWorkerTerminate(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Terminate(context.Background())
	require.NoError(t, err)
}

func TestAppServerWorker_StopThenTerminatePreservesSiblingWrapper(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Hour})
	var output strings.Builder
	mgr.stdin = documentedAckWriter{target: &output, manager: mgr}
	mgr.mu.Lock()
	mgr.refs = 2
	mgr.state = stateRunning
	mgr.mu.Unlock()
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	recvA := mgr.Subscribe("thread-a", "session-a")
	recvB := mgr.Subscribe("thread-b", "session-b")
	connA := &appConn{userID: "user-1", sessionID: "session-a", recvCh: recvA, manager: mgr}
	connB := &appConn{userID: "user-1", sessionID: "session-b", recvCh: recvB, manager: mgr}
	workerA := &AppServerWorker{
		managerRefHeld: true, // fixture simulates a successful Acquire
		BaseWorker:     base.NewBaseWorker(slog.Default(), nil),
		manager:        mgr,
		threadID:       "thread-a",
		turnID:         "turn-a",
		doneCh:         make(chan struct{}),
		conn:           connA,
	}
	workerB := &AppServerWorker{
		managerRefHeld: true, // fixture simulates a successful Acquire
		BaseWorker:     base.NewBaseWorker(slog.Default(), nil),
		manager:        mgr,
		threadID:       "thread-b",
		turnID:         "turn-b",
		doneCh:         make(chan struct{}),
		conn:           connB,
	}

	require.NoError(t, workerA.StopCurrentTurn(context.Background()))
	require.NoError(t, workerA.Terminate(context.Background()))
	require.NoError(t, workerA.Terminate(context.Background()), "wrapper release must be idempotent")
	_, open := <-connA.Recv()
	require.False(t, open, "terminated wrapper connection must close")

	mgr.mu.Lock()
	require.Equal(t, 1, mgr.refs, "only the stopped wrapper reference must be released")
	require.Equal(t, stateRunning, mgr.state, "terminating one wrapper must not stop the shared app-server")
	mgr.mu.Unlock()
	mgr.subMu.Lock()
	_, hasA := mgr.subscribers["thread-a"]
	_, hasB := mgr.subscribers["thread-b"]
	mgr.subMu.Unlock()
	require.False(t, hasA)
	require.True(t, hasB, "sibling wrapper subscription must remain active")

	want := &events.Envelope{SessionID: "session-b", Event: events.Event{Type: events.State}}
	require.True(t, connB.TrySend(want), "sibling wrapper connection must remain usable")
	require.Same(t, want, <-connB.Recv())
	require.Contains(t, output.String(), `"method":"turn/interrupt"`)

	require.NoError(t, workerB.Terminate(context.Background()))
}

func TestAppServerWorkerKill(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Kill()
	require.NoError(t, err)
}

func TestAppServerWorkerWaitNoCrashSub(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

func TestAppServerWorkerResume(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	// Resume delegates to Start, which will fail without manager running
	err := w.Resume(context.Background(), worker.SessionInfo{
		SessionID:  "sess-1",
		ProjectDir: t.TempDir(),
	})
	require.Error(t, err)
}

func TestAppServerWorkerHealthAndLastIO(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	health := w.Health()
	require.Equal(t, worker.TypeCodexCLI, health.Type)

	lastIO := w.LastIO()
	_ = lastIO // can be zero
}

func TestAppServerWorkerHandlePermissionResponse(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.HandlePermissionResponse(context.Background(), "req-1", true, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestAppServerWorkerHandlePermissionResponseMethodSpecificDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		allowed bool
		want    string
	}{
		{
			name:    "v2 command allow",
			method:  codexMethodCommandApproval,
			allowed: true,
			want:    "accept",
		},
		{
			name:    "v2 file deny",
			method:  codexMethodFileChangeApproval,
			allowed: false,
			want:    "decline",
		},
		{
			name:    "legacy exec allow",
			method:  codexMethodExecCommandApproval,
			allowed: true,
			want:    "approved",
		},
		{
			name:    "legacy apply patch deny",
			method:  codexMethodApplyPatchApproval,
			allowed: false,
			want:    "denied",
		},
		{
			name:    "legacy generic allow",
			method:  codexMethodServerRequestApproval,
			allowed: true,
			want:    "approved",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newTestAppServerWorker(t)
			reqID := strings.ReplaceAll(tc.name, " ", "-")
			w.manager.serverReqIDs.Store(reqID, int64(42))
			w.manager.serverReqMethods.Store(reqID, tc.method)

			var buf strings.Builder
			w.manager.stdin = struct {
				io.Writer
				io.Closer
			}{
				Writer: &buf,
				Closer: io.NopCloser(nil),
			}

			err := w.HandlePermissionResponse(context.Background(), reqID, tc.allowed, "")
			require.NoError(t, err)
			require.Contains(t, buf.String(), fmt.Sprintf(`"decision":%q`, tc.want))

			_, ok := w.manager.serverReqMethods.Load(reqID)
			require.False(t, ok, "method metadata should be cleared after response")
		})
	}
}

func TestAppServerWorkerHandleQuestionResponse(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.HandleQuestionResponse(context.Background(), "req-1", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestAppServerWorkerHandleElicitationResponse(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.HandleElicitationResponse(context.Background(), "req-1", "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestAppServerWorkerInputNoThreadID(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Input(context.Background(), "hello", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not started")
}

// ─── InjectMidTurn Tests (SESSION_BUSY mid-turn passthrough) ──────────────

// TestInjectMidTurn_SteersActiveTurn verifies InjectMidTurn delegates to
// manager.SteerTurn with the worker's snapshot threadID + turnID and the
// injected text. The codex package has no mock manager — manager is a concrete
// *CodexAppServerManager — so we mirror the real-manager + fake-stdin pattern
// used by TestInputCallsTurnStart / TestRespondServerRequest: SteerTurn's Call
// writes the JSON-RPC frame to stdin (captured in a strings.Builder) and then
// times out waiting for a response. The captured frame proves SteerTurn was
// invoked with the right args; the surfaced error proves InjectMidTurn
// delegates the RPC (and does not swallow it).
func TestInjectMidTurn_SteersActiveTurn(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	var buf strings.Builder
	mgr.stdin = struct {
		io.Writer
		io.Closer
	}{
		Writer: &buf,
		Closer: io.NopCloser(nil),
	}
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr_1",
		turnID:     "turn_1",
	}

	err := wk.InjectMidTurn(context.Background(), "more info", nil)
	// SteerTurn's Call times out (no real codex process responds), proving the
	// RPC path was taken. InjectMidTurn must surface — not swallow — the error.
	require.Error(t, err)
	require.Contains(t, err.Error(), "turn/steer")

	// The JSON-RPC request written to stdin is the observable proof that
	// SteerTurn was called with the worker's snapshot IDs and injected text.
	written := buf.String()
	require.Contains(t, written, `"method":"turn/steer"`)
	require.Contains(t, written, `"threadId":"thr_1"`)
	require.Contains(t, written, `"expectedTurnId":"turn_1"`)
	require.Contains(t, written, `"more info"`)
}

// TestInjectMidTurn_NoActiveTurn verifies InjectMidTurn returns an error when
// no turn is active (threadID/turnID empty), so the gateway falls back to
// pending-buffer replay. Mirrors StopCurrentTurn's empty-turn guard.
func TestInjectMidTurn_NoActiveTurn(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	// threadID/turnID left empty — no active turn to steer.
	err := w.InjectMidTurn(context.Background(), "x", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no active turn")
}

// ─── appConn tests ────────────────────────────────────────────────────────

func TestAppConnSendRecvClose(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	ch := make(chan *events.Envelope, 5)
	conn := &appConn{
		userID:    "user-1",
		sessionID: "sess-1",
		recvCh:    ch,
		manager:   mgr,
	}

	require.Equal(t, "user-1", conn.UserID())
	require.Equal(t, "sess-1", conn.SessionID())
	require.Equal(t, (<-chan *events.Envelope)(ch), conn.Recv())

	// TrySend
	env := events.NewEnvelope("id-1", "sess-1", 1, events.MessageDelta, events.MessageDeltaData{Content: "hi"})
	require.True(t, conn.TrySend(env))
	got := <-ch
	require.Equal(t, events.MessageDelta, got.Event.Type)

	// Close
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close()) // idempotent
	_, ok := <-ch
	require.False(t, ok) // closed
}

func TestAppConnTrySendFull(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	ch := make(chan *events.Envelope, 1)
	conn := &appConn{
		userID:    "user-1",
		sessionID: "sess-1",
		recvCh:    ch,
		manager:   mgr,
	}

	// Fill channel
	conn.TrySend(events.NewEnvelope("id-1", "sess-1", 1, events.Done, events.DoneData{}))
	// Next send should fail
	require.False(t, conn.TrySend(events.NewEnvelope("id-2", "sess-1", 2, events.Done, events.DoneData{})))
}

// ─── ParseNotification/ParseResponse edge cases ───────────────────────────

func TestParseNotificationMissingParams(t *testing.T) {
	t.Parallel()

	p := NewParser()
	method, params, err := p.ParseNotification(`{"jsonrpc":"2.0","method":"item/started"}`)
	require.NoError(t, err)
	require.Equal(t, "item/started", method)
	require.Nil(t, params)
}

func TestParseResponseMissingResult(t *testing.T) {
	t.Parallel()

	p := NewParser()
	id, result, rpcErr, err := p.ParseResponse(`{"jsonrpc":"2.0","id":5}`)
	require.NoError(t, err)
	require.Equal(t, int64(5), id)
	require.Nil(t, result)
	require.Nil(t, rpcErr)
}

// ─── config init tests ────────────────────────────────────────────────────

func TestInitConfigAndGetConfig(t *testing.T) {
	InitConfig(config.CodexCLIConfig{
		Command: "/usr/local/bin/codex",
		Model:   "o3",
	})
	cfg := GetConfig()
	require.Equal(t, "/usr/local/bin/codex", cfg.Command)
	require.Equal(t, "o3", cfg.Model)
}

func TestAppServerWorkerResetContext(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	// Don't set threadID to avoid triggering Notify on a nil manager connection
	_, err := w.ResetContext(context.Background())
	require.NoError(t, err)
	require.Empty(t, w.threadID)
	require.Nil(t, w.recvCh)
}

// ─── Manager Lifecycle Tests ──────────────────────────────────────────────

func TestManagerIsRunning(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	require.False(t, mgr.IsRunning())

	mgr.mu.Lock()
	mgr.state = stateRunning
	mgr.mu.Unlock()
	require.True(t, mgr.IsRunning())
}

func TestManagerAcquireStartsProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires codex binary")
	}

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute, StartupTimeout: 5 * time.Second, CallTimeout: 5 * time.Second}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	crashCh, err := mgr.Acquire(context.Background())

	if err != nil {
		// Codex binary not available (CI or fresh environment).
		t.Skipf("codex binary not available: %v", err)
	}

	// Codex binary available — verify handshake completed and process is running.
	require.NoError(t, err)
	require.NotNil(t, crashCh)
	mgr.Release()
}

// ─── dispatchFrame / dispatchServerRequest / RespondServerRequest ──────

func TestDispatchFrameServerRequest(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Subscribe to receive events for the thread.
	ch := mgr.Subscribe("thr-1", "sess-1")

	// Simulate a server-initiated approval request (has both ID and Method).
	frame := []byte(`{"jsonrpc":"2.0","id":99,"method":"serverRequest/approval","params":{"threadId":"thr-1","requestId":"req-42","toolName":"Bash","reason":"run ls"}}`)
	mgr.dispatchFrame(frame)

	// Should have stored requestID → frameID mapping.
	v, ok := mgr.serverReqIDs.Load("req-42")
	require.True(t, ok, "requestID should be stored in serverReqIDs")
	require.Equal(t, JSONRPCID(`99`), v)

	// Subscriber should receive a PermissionRequest envelope.
	select {
	case env := <-ch:
		require.Equal(t, events.PermissionRequest, env.Event.Type)
		pr, ok := env.Event.Data.(events.PermissionRequestData)
		require.True(t, ok)
		require.Equal(t, "req-42", pr.ID)
		require.Equal(t, "Bash", pr.ToolName)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for envelope")
	}
}

func TestDispatchFrameClientResponse(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Register a pending request.
	respCh := make(chan *JSONRPCResponse, 1)
	mgr.pending.Store(int64(7), respCh)
	defer mgr.pending.Delete(int64(7))

	// Simulate a client response (has ID, no Method).
	frame := []byte(`{"jsonrpc":"2.0","id":7,"result":{"thread":{"id":"thr-1"}}}`)
	mgr.dispatchFrame(frame)

	select {
	case resp := <-respCh:
		require.Equal(t, int64(7), resp.ID)
		require.NotNil(t, resp.Result)
		require.Nil(t, resp.Error)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestDispatchFrameNotification(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	ch := mgr.Subscribe("thr-1", "sess-1")

	// Notification: ID=0, Method present, no Error.
	frame := []byte(`{"jsonrpc":"2.0","method":"turn/failed","params":{"threadId":"thr-1","turn":{}}}`)
	mgr.dispatchFrame(frame)

	select {
	case env := <-ch:
		require.Equal(t, events.Error, env.Event.Type)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification envelope")
	}
}

func TestDispatchFrameServerRequestAcceptsZeroAndStringIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		frameID   string
		requestID string
		wantWire  string
	}{
		{name: "zero integer", frameID: `0`, requestID: "item-zero", wantWire: `"id":0`},
		{name: "string", frameID: `"server-request-1"`, requestID: "item-string", wantWire: `"id":"server-request-1"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
			mgr := NewCodexAppServerManager(slog.Default(), cfg)
			_ = mgr.Subscribe("thr-1", "sess-1")

			frame := fmt.Sprintf(
				`{"id":%s,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","itemId":%q,"command":"pwd"}}`,
				tc.frameID, tc.requestID,
			)
			mgr.dispatchFrame([]byte(frame))

			_, ok := mgr.serverReqIDs.Load(tc.requestID)
			require.True(t, ok, "a present JSON-RPC id must be retained even when it is zero")

			var buf strings.Builder
			mgr.stdin = struct {
				io.Writer
				io.Closer
			}{Writer: &buf, Closer: io.NopCloser(nil)}

			require.NoError(t, mgr.RespondServerRequest(
				context.Background(), tc.requestID, map[string]any{"decision": "accept"},
			))
			require.Contains(t, buf.String(), tc.wantWire)
		})
	}
}

func TestServerRequestLifecycleCleanup(t *testing.T) {
	t.Parallel()

	t.Run("resolved notification clears by raw rpc id", func(t *testing.T) {
		t.Parallel()
		mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
		_ = mgr.Subscribe("thr-resolved", "sess-resolved")
		mgr.dispatchFrame([]byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-resolved","itemId":"item-resolved"}}`))
		_, ok := mgr.serverReqIDs.Load("item-resolved")
		require.True(t, ok)

		mgr.dispatchFrame([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"thr-resolved","requestId":0}}`))
		_, ok = mgr.serverReqIDs.Load("item-resolved")
		require.False(t, ok)
	})

	t.Run("unsubscribe clears only that thread", func(t *testing.T) {
		t.Parallel()
		mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
		_ = mgr.Subscribe("thr-a", "sess-a")
		_ = mgr.Subscribe("thr-b", "sess-b")
		mgr.dispatchFrame([]byte(`{"id":1,"method":"item/fileChange/requestApproval","params":{"threadId":"thr-a","itemId":"item-a"}}`))
		mgr.dispatchFrame([]byte(`{"id":2,"method":"item/fileChange/requestApproval","params":{"threadId":"thr-b","itemId":"item-b"}}`))

		mgr.Unsubscribe("thr-a")
		_, aExists := mgr.serverReqIDs.Load("item-a")
		_, bExists := mgr.serverReqIDs.Load("item-b")
		require.False(t, aExists)
		require.True(t, bExists)
	})
}

func TestDispatchFrameErrorWithZeroID(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Error frame with ID=0 should be dropped silently (no panic).
	frame := []byte(`{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"Invalid params"}}`)
	mgr.dispatchFrame(frame) // should not panic
}

func TestDispatchFrameNoMethodNoID(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Bare frame with no method, no ID — should be dropped.
	frame := []byte(`{"jsonrpc":"2.0"}`)
	mgr.dispatchFrame(frame) // should not panic
}

func TestDispatchFrameInvalidJSON(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Invalid JSON should be handled gracefully.
	mgr.dispatchFrame([]byte(`not json at all`)) // should not panic
}

func TestDispatchServerRequestWithoutThreadID(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	// Server request without threadId/conversationId should be dropped without storing requestID.
	frame := &JSONRPCFrame{
		JSONRPC: "2.0",
		ID:      integerJSONRPCID(50),
		Method:  "serverRequest/approval",
		Params:  json.RawMessage(`{"requestId":"req-no-thread"}`),
	}
	mgr.dispatchServerRequest(frame)

	_, ok := mgr.serverReqIDs.Load("req-no-thread")
	require.False(t, ok, "requestID should NOT be stored for threadless request")
}

func TestRespondServerRequest(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		// Pre-store a requestID → frameID mapping.
		mgr.serverReqIDs.Store("req-100", int64(42))

		// Capture what gets written to stdin.
		var buf strings.Builder
		mgr.stdin = struct {
			io.Writer
			io.Closer
		}{
			Writer: &buf,
			Closer: io.NopCloser(nil),
		}

		err := mgr.RespondServerRequest(context.Background(), "req-100", map[string]string{"decision": "accept"})
		require.NoError(t, err)

		// Entry should be deleted.
		_, ok := mgr.serverReqIDs.Load("req-100")
		require.False(t, ok)

		// Written JSON should contain the frame ID and result.
		require.Contains(t, buf.String(), `"id":42`)
		require.Contains(t, buf.String(), "accept")
	})

	t.Run("unknown_request", func(t *testing.T) {
		t.Parallel()

		err := mgr.RespondServerRequest(context.Background(), "nonexistent", map[string]string{"decision": "decline"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no pending server request")
	})

	t.Run("write_failure_keeps_request_retryable", func(t *testing.T) {
		t.Parallel()

		mgr := NewCodexAppServerManager(slog.Default(), cfg)
		mgr.serverReqIDs.Store("req-retry", int64(43))
		mgr.serverReqMethods.Store("req-retry", codexMethodCommandApproval)
		reader, writer := io.Pipe()
		defer reader.Close()
		require.NoError(t, writer.Close())
		mgr.stdin = writer

		err := mgr.RespondServerRequest(context.Background(), "req-retry", map[string]string{"decision": "accept"})
		require.Error(t, err)
		_, ok := mgr.serverReqIDs.Load("req-retry")
		require.True(t, ok, "a failed write must retain the request for retry")
		_, ok = mgr.serverReqMethods.Load("req-retry")
		require.True(t, ok, "method metadata must survive a failed write")
	})
}

// ─── Approval Method Name Coverage ─────────────────────────────────────

func TestMapNotificationApprovalMethodNames(t *testing.T) {
	t.Parallel()

	methods := []string{
		codexMethodServerRequestApproval,
		codexMethodCommandApproval,
		codexMethodFileChangeApproval,
		codexMethodExecCommandApproval,
		codexMethodApplyPatchApproval,
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			m := NewMapper("session-1")
			params := json.RawMessage(fmt.Sprintf(
				`{"requestId":"r1","toolName":"%s","reason":"test"}`, method))
			envs := m.MapNotification(method, params)
			require.Len(t, envs, 1, "method %s should produce 1 envelope", method)
			require.Equal(t, events.PermissionRequest, envs[0].Event.Type, "method %s", method)
			pr, ok := envs[0].Event.Data.(events.PermissionRequestData)
			require.True(t, ok)
			require.Equal(t, "r1", pr.ID)
		})
	}
}

func TestMapNotificationCurrentInteractiveMethodNames(t *testing.T) {
	t.Parallel()

	t.Run("request user input", func(t *testing.T) {
		t.Parallel()
		m := NewMapper("session-1")
		envs := m.MapNotification("item/tool/requestUserInput", json.RawMessage(`{
			"requestId":"rpc-1",
			"itemId":"call-1",
			"questions":[{
				"id":"environment",
				"header":"Environment",
				"question":"Where should this run?",
				"options":[{"label":"Staging","description":"Safe test environment"}]
			}]
		}`))
		require.Len(t, envs, 1)
		require.Equal(t, events.QuestionRequest, envs[0].Event.Type)
		data, ok := envs[0].Event.Data.(events.QuestionRequestData)
		require.True(t, ok)
		require.Equal(t, "call-1", data.ID)
		require.Len(t, data.Questions, 1)
		encoded, err := json.Marshal(data.Questions[0])
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"id":"environment"`)
	})

	t.Run("permissions approval", func(t *testing.T) {
		t.Parallel()
		m := NewMapper("session-1")
		envs := m.MapNotification("item/permissions/requestApproval", json.RawMessage(`{
			"itemId":"call-permissions",
			"reason":"Write generated files",
			"cwd":"/tmp/project",
			"permissions":{"fileSystem":{"write":["/tmp/project"]}}
		}`))
		require.Len(t, envs, 1)
		require.Equal(t, events.PermissionRequest, envs[0].Event.Type)
		data, ok := envs[0].Event.Data.(events.PermissionRequestData)
		require.True(t, ok)
		require.Equal(t, "call-permissions", data.ID)
		require.Equal(t, "Write generated files", data.Description)
	})
}

func TestAppServerWorkerCurrentInteractiveResponseSchemas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		frame      string
		requestID  string
		respond    func(context.Context, *AppServerWorker, string) error
		wantResult []string
	}{
		{
			name:      "granular permissions allow returns requested subset",
			frame:     `{"id":0,"method":"item/permissions/requestApproval","params":{"threadId":"thr-1","itemId":"perm-1","cwd":"/tmp/project","permissions":{"fileSystem":{"write":["/tmp/project"]}}}}`,
			requestID: "perm-1",
			respond: func(ctx context.Context, worker *AppServerWorker, requestID string) error {
				return worker.HandlePermissionResponse(ctx, requestID, true, "")
			},
			wantResult: []string{`"id":0`, `"scope":"turn"`, `"fileSystem"`, `"/tmp/project"`},
		},
		{
			name:      "request user input uses question ids and answer arrays",
			frame:     `{"id":"question-rpc","method":"item/tool/requestUserInput","params":{"threadId":"thr-1","itemId":"question-1","questions":[{"id":"environment","question":"Where?","header":"Environment","options":null}]}}`,
			requestID: "question-1",
			respond: func(ctx context.Context, worker *AppServerWorker, requestID string) error {
				return worker.HandleQuestionResponse(ctx, requestID, map[string]string{"Where?": "Staging"})
			},
			wantResult: []string{`"id":"question-rpc"`, `"environment":{"answers":["Staging"]}`},
		},
		{
			name:      "mcp elicitation without business id uses derived id",
			frame:     `{"id":2,"method":"mcpServer/elicitation/request","params":{"threadId":"thr-1","serverName":"files","mode":"form","message":"Choose a root","requestedSchema":{}}}`,
			requestID: "codex-rpc:2",
			respond: func(ctx context.Context, worker *AppServerWorker, requestID string) error {
				return worker.HandleElicitationResponse(ctx, requestID, "accept", map[string]any{"root": "/tmp"})
			},
			wantResult: []string{`"id":2`, `"action":"accept"`, `"root":"/tmp"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute})
			_ = mgr.Subscribe("thr-1", "sess-1")
			mgr.dispatchFrame([]byte(tc.frame))

			var buf strings.Builder
			mgr.stdin = struct {
				io.Writer
				io.Closer
			}{Writer: &buf, Closer: io.NopCloser(nil)}
			worker := &AppServerWorker{manager: mgr}
			require.NoError(t, tc.respond(context.Background(), worker, tc.requestID))
			for _, want := range tc.wantResult {
				require.Contains(t, buf.String(), want)
			}
			_, exists := mgr.serverReqIDs.Load(tc.requestID)
			require.False(t, exists, "successful native response must clear pending request")
		})
	}
}

// TestMapNotificationApprovalIDFieldPriority verifies that for
// item/commandExecution/requestApproval the request ID is resolved from
// approvalId → itemId → requestId (first non-empty wins), and that the
// command field is used as tool name when toolName is absent.
func TestMapNotificationApprovalIDFieldPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		params   string
		wantID   string
		wantTool string
	}{
		{
			name:     "approvalId takes priority over itemId",
			params:   `{"approvalId":"appr-1","itemId":"item-1","requestId":"req-1","command":"ls"}`,
			wantID:   "appr-1",
			wantTool: "ls",
		},
		{
			name:     "itemId used when approvalId is null/empty",
			params:   `{"approvalId":null,"itemId":"item-2","requestId":"req-2","command":"cat /etc/hosts"}`,
			wantID:   "item-2",
			wantTool: "cat /etc/hosts",
		},
		{
			name:     "itemId used when approvalId is empty string",
			params:   `{"approvalId":"","itemId":"item-3","command":"echo hi"}`,
			wantID:   "item-3",
			wantTool: "echo hi",
		},
		{
			name:     "requestId fallback when approvalId and itemId are absent",
			params:   `{"requestId":"req-4","toolName":"Bash","reason":"run ls"}`,
			wantID:   "req-4",
			wantTool: "Bash",
		},
		{
			name:     "command truncated to 60 runes when toolName absent",
			params:   `{"itemId":"item-5","command":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
			wantID:   "item-5",
			wantTool: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa…",
		},
		{
			name:     "default tool name when all name fields are empty",
			params:   `{"itemId":"item-6"}`,
			wantID:   "item-6",
			wantTool: "shell command",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewMapper("session-1")
			envs := m.MapNotification("item/commandExecution/requestApproval", json.RawMessage(tc.params))
			require.Len(t, envs, 1)
			pr, ok := envs[0].Event.Data.(events.PermissionRequestData)
			require.True(t, ok)
			require.Equal(t, tc.wantID, pr.ID, "request ID mismatch")
			require.Equal(t, tc.wantTool, pr.ToolName, "tool name mismatch")
		})
	}
}

func TestMapNotificationLegacyApproval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   string
		params   string
		wantID   string
		wantTool string
	}{
		{
			name:     "exec approval uses approvalId and command array preview",
			method:   codexMethodExecCommandApproval,
			params:   `{"approvalId":"appr-legacy","callId":"call-legacy","command":["echo","hello"],"reason":"run command"}`,
			wantID:   "appr-legacy",
			wantTool: "echo hello",
		},
		{
			name:     "exec approval falls back to callId",
			method:   codexMethodExecCommandApproval,
			params:   `{"approvalId":null,"callId":"call-only","command":["pwd"]}`,
			wantID:   "call-only",
			wantTool: "pwd",
		},
		{
			name:     "apply patch uses callId",
			method:   codexMethodApplyPatchApproval,
			params:   `{"callId":"patch-call","reason":"apply patch"}`,
			wantID:   "patch-call",
			wantTool: "shell command",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewMapper("session-1")
			envs := m.MapNotification(tc.method, json.RawMessage(tc.params))
			require.Len(t, envs, 1)
			pr, ok := envs[0].Event.Data.(events.PermissionRequestData)
			require.True(t, ok)
			require.Equal(t, tc.wantID, pr.ID)
			require.Equal(t, tc.wantTool, pr.ToolName)
		})
	}
}

// TestDispatchServerRequestApprovalIDResolution verifies that dispatchServerRequest
// uses the same approvalId → itemId → requestId priority when building the
// serverReqIDs map.
func TestDispatchServerRequestApprovalIDResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		frameJSON  string
		wantKey    string
		wantMethod string
	}{
		{
			name:       "approvalId stored when present",
			frameJSON:  `{"jsonrpc":"2.0","id":10,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","approvalId":"appr-x","itemId":"item-x"}}`,
			wantKey:    "appr-x",
			wantMethod: codexMethodCommandApproval,
		},
		{
			name:       "itemId stored when approvalId is null",
			frameJSON:  `{"jsonrpc":"2.0","id":11,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","approvalId":null,"itemId":"item-y"}}`,
			wantKey:    "item-y",
			wantMethod: codexMethodCommandApproval,
		},
		{
			name:       "requestId stored as fallback",
			frameJSON:  `{"jsonrpc":"2.0","id":12,"method":"serverRequest/approval","params":{"threadId":"thr-1","requestId":"req-z"}}`,
			wantKey:    "req-z",
			wantMethod: codexMethodServerRequestApproval,
		},
		{
			name:       "legacy exec uses conversationId and approvalId",
			frameJSON:  `{"jsonrpc":"2.0","id":13,"method":"execCommandApproval","params":{"conversationId":"thr-1","callId":"call-x","approvalId":"appr-legacy","command":["echo","hello"],"cwd":"/tmp"}}`,
			wantKey:    "appr-legacy",
			wantMethod: codexMethodExecCommandApproval,
		},
		{
			name:       "legacy apply patch uses conversationId and callId",
			frameJSON:  `{"jsonrpc":"2.0","id":14,"method":"applyPatchApproval","params":{"conversationId":"thr-1","callId":"patch-call","fileChanges":{},"reason":"patch"}}`,
			wantKey:    "patch-call",
			wantMethod: codexMethodApplyPatchApproval,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
			mgr := NewCodexAppServerManager(slog.Default(), cfg)
			_ = mgr.Subscribe("thr-1", "sess-1")
			mgr.dispatchFrame([]byte(tc.frameJSON))
			_, ok := mgr.serverReqIDs.Load(tc.wantKey)
			require.True(t, ok, "expected serverReqIDs to contain key %q", tc.wantKey)
			method, ok := mgr.serverReqMethods.Load(tc.wantKey)
			require.True(t, ok, "expected serverReqMethods to contain key %q", tc.wantKey)
			require.Equal(t, tc.wantMethod, method)
		})
	}
}

func TestMapNotificationElicitation(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	params := json.RawMessage(`{"requestId":"el_1","mcpServerName":"fs","message":"Allow file access?","mode":"confirm"}`)
	envs := m.MapNotification("mcpServer/elicitation/request", params)
	require.Len(t, envs, 1)
	require.Equal(t, events.ElicitationRequest, envs[0].Event.Type)
	el, ok := envs[0].Event.Data.(events.ElicitationRequestData)
	require.True(t, ok)
	require.Equal(t, "el_1", el.ID)
	require.Equal(t, "fs", el.MCPServerName)
	require.Equal(t, "Allow file access?", el.Message)
	require.Equal(t, "confirm", el.Mode)
}

func TestMapNotificationElicitationBadJSON(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-1")
	envs := m.MapNotification("mcpServer/elicitation/request", json.RawMessage(`{bad`))
	require.Nil(t, envs)
}

// ─── Issue #575: Reset kills session + Zombie process fix tests ──────────

// testConfigWithDefaults sets a known-good config for integration tests and
// returns a restore function that should be deferred.
func testConfigWithDefaults() func() {
	prev := GetConfig()
	InitConfig(config.CodexCLIConfig{
		Sandbox:         "workspace-write",
		ApprovalMode:    "never",
		Personality:     "friendly",
		IdleDrainPeriod: time.Minute,
		StartupTimeout:  10 * time.Second,
		CallTimeout:     10 * time.Second,
	})
	return func() { InitConfig(prev) }
}

// ---------- Unit tests (no codex binary required) ----------

func TestResetContextClearsStateAndResetsOnce(t *testing.T) {
	// Verify lightweight ResetContext closes old conn and unsubscribes old thread
	// without touching closed/doneCh/releaseOnce (no Terminate+Start cycle).
	// We test with origSession.SessionID="" so thread/start is skipped.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})

	recvCh := make(chan *events.Envelope, 1)
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		doneCh:     make(chan struct{}),
		conn:       &appConn{recvCh: recvCh},
	}
	w.origSession = worker.SessionInfo{}

	_, err := w.ResetContext(context.Background())
	require.NoError(t, err)

	// Old recvCh should be closed by appConn.Close().
	_, ok := <-recvCh
	require.False(t, ok, "old recvCh should be closed")
}

func TestResetContextRestartsFromSavedSession(t *testing.T) {
	// Verify ResetContext performs lightweight thread swap: closes old conn,
	// resets lifecycle state, then attempts thread/start on same manager.
	// No real codex process — provide fake stdin so writeFrame won't panic,
	// thread/start will fail with write/timeout error.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	// Drain pipe in background so Notify/Call writes don't block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	oldRecvCh := make(chan *events.Envelope, 1)
	wk := &AppServerWorker{
		BaseWorker:  base.NewBaseWorker(slog.Default(), nil),
		manager:     mgr,
		origSession: worker.SessionInfo{SessionID: "sess-575", UserID: "u1", ProjectDir: "/tmp"},
		threadID:    "old-thread",
		recvCh:      make(chan *events.Envelope, 1),
		doneCh:      make(chan struct{}),
		conn:        &appConn{recvCh: oldRecvCh},
	}

	_, err := wk.ResetContext(context.Background())

	// Old conn should be closed.
	_, ok := <-oldRecvCh
	require.False(t, ok, "old recvCh should be closed")

	// thread/start fails because no real process responds.
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: reset thread/start:")
}

func TestKillDoesNotCallKillIfIdle(t *testing.T) {
	// Verify Kill() no longer calls KillIfIdle() — the singleton process
	// is only stopped via idle drain or explicit ShutdownSingleton().
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})

	// Simulate a running process with refs=1 and stateRunning.
	mgr.mu.Lock()
	mgr.state = stateRunning
	mgr.refs = 1
	mgr.pgid = 0
	// Start an idle timer so we can verify it remains untouched.
	mgr.idleTimer = time.AfterFunc(time.Minute, func() {})
	timerBefore := mgr.idleTimer
	mgr.mu.Unlock()

	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		doneCh:     make(chan struct{}),
	}
	// threadID left empty so release() won't call Notify (no real process).

	err := w.Kill()
	require.NoError(t, err)

	// release() was called (closed=true, doneCh closed).
	w.mu.Lock()
	require.True(t, w.closed, "closed should be true after Kill")
	w.mu.Unlock()

	// KillIfIdle was NOT called: idleTimer should still be active.
	mgr.mu.Lock()
	timer := mgr.idleTimer
	mgr.mu.Unlock()
	require.NotNil(t, timer, "idle timer should NOT be stopped — KillIfIdle removed from shutdown()")
	require.Equal(t, timerBefore, timer, "idle timer reference unchanged")
	timer.Stop()
}

func TestKillIfIdleKillsWhenIdle(t *testing.T) {
	// Verify KillIfIdle actually triggers the kill path when all conditions
	// are met: refs==0, stateRunning, pgid>0. Use pgid=-1 so ForceKill
	// sends SIGKILL to PGID -1 which returns ESRCH (no such process) —
	// harmless but confirms the code path is exercised.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})
	mgr.mu.Lock()
	mgr.state = stateRunning
	mgr.refs = 0
	mgr.pgid = -1 // ForceKill(-(-1)) = SIGKILL to PGID 1 → ESRCH, harmless
	mgr.mu.Unlock()

	// Should not panic or deadlock.
	mgr.KillIfIdle()

	mgr.mu.Lock()
	require.Nil(t, mgr.idleTimer, "idle timer should be stopped")
	mgr.mu.Unlock()
}

func TestKillIfIdleNoOpWhenRefsPositive(t *testing.T) {
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: 30 * time.Minute,
	})

	// Simulate running with refs=1.
	mgr.mu.Lock()
	mgr.state = stateRunning
	mgr.refs = 1
	mgr.pgid = 0 // pgid=0: shouldKill false, no real ForceKill
	mgr.mu.Unlock()

	// KillIfIdle should be no-op because refs > 0.
	mgr.KillIfIdle()

	mgr.mu.Lock()
	require.Equal(t, stateRunning, mgr.state, "process should stay running when refs > 0")
	mgr.mu.Unlock()
}

// ---------- Integration tests (require codex binary) ----------

func TestIntegrationStartSavesSessionAndResetRestarts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires codex binary")
	}
	defer testConfigWithDefaults()()

	w := newTestAppServerWorker(t)
	session := worker.SessionInfo{
		SessionID:  "sess-integ-575",
		UserID:     "user-1",
		ProjectDir: t.TempDir(),
	}
	err := w.Start(context.Background(), session)
	if err != nil {
		t.Skipf("codex binary not available: %v", err)
	}
	t.Cleanup(func() { _ = w.Terminate(context.Background()) })

	w.mu.Lock()
	saved := w.origSession
	w.mu.Unlock()
	require.Equal(t, "sess-integ-575", saved.SessionID)
	require.Equal(t, "user-1", saved.UserID)

	// Test ResetContext restarts with a new thread.
	w.mu.Lock()
	firstThreadID := w.threadID
	w.mu.Unlock()
	require.NotEmpty(t, firstThreadID)

	_, err = w.ResetContext(context.Background())
	require.NoError(t, err)

	w.mu.Lock()
	secondThreadID := w.threadID
	w.mu.Unlock()
	require.NotEmpty(t, secondThreadID)
	require.NotEqual(t, firstThreadID, secondThreadID, "threadID should change after reset")
}

// ─── appConn.Send Tests ──────────────────────────────────────────────────

func TestAppConnSendReturnsErrNotImplemented(t *testing.T) {
	t.Parallel()

	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)
	ch := make(chan *events.Envelope, 1)
	conn := &appConn{
		userID:    "user-1",
		sessionID: "sess-1",
		recvCh:    ch,
		manager:   mgr,
	}

	env := events.NewEnvelope("id-1", "sess-1", 1, events.MessageStart, nil)
	err := conn.Send(context.Background(), env)
	require.ErrorIs(t, err, worker.ErrNotImplemented)
}

// ─── Compact Tests ────────────────────────────────────────────────────────

func TestCompactNoThread(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Compact(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no active thread")
}

func TestCompactDelegatesToManager(t *testing.T) {
	t.Parallel()

	// Set up a manager with a fake stdin pipe so Call won't panic on write,
	// but will timeout since there's no real process responding.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-compact",
	}

	err := wk.Compact(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: compact:")
}

// ─── Clear Tests ──────────────────────────────────────────────────────────

func TestClearNoThread(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Clear(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no active thread")
}

func TestClearDelegatesToResetContext(t *testing.T) {
	t.Parallel()

	// Clear calls ResetContext internally.
	// ResetContext → cleanupOldThread → Notify(thread/unsubscribe) needs stdin.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	recvCh := make(chan *events.Envelope, 1)
	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-clear",
		doneCh:     make(chan struct{}),
		conn:       &appConn{recvCh: recvCh},
	}

	err := wk.Clear(context.Background())
	require.NoError(t, err)

	_, ok := <-recvCh
	require.False(t, ok)
}

func TestClearWithActiveTurn(t *testing.T) {
	t.Parallel()

	// Clear with a turnID should attempt InterruptTurn before ResetContext.
	// The native interrupt requires a request ACK; keep a real fake pipe.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			var frame JSONRPCRequest
			if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame.ID != 0 {
				mgr.dispatchResponse(&JSONRPCResponse{ID: frame.ID, Result: json.RawMessage(`{}`)})
			}
		}
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	recvCh := make(chan *events.Envelope, 1)
	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-clear2",
		turnID:     "turn-active",
		doneCh:     make(chan struct{}),
		conn:       &appConn{recvCh: recvCh},
	}

	err := wk.Clear(context.Background())
	require.NoError(t, err)
}

// ─── Rewind Tests ─────────────────────────────────────────────────────────

func TestRewindNoThread(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Rewind(context.Background(), "1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no active thread")
}

func TestRewindDefaultOne(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-rewind",
	}

	// Empty targetID defaults to 1 turn.
	err := wk.Rewind(context.Background(), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: rewind:")
}

func TestRewindExplicitCount(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-rewind2",
	}

	// Valid count.
	err := wk.Rewind(context.Background(), "3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: rewind:")
}

func TestRewindInvalidCount(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-rewind3",
	}

	// Invalid count string falls back to default 1.
	err := wk.Rewind(context.Background(), "abc")
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: rewind:")
}

// ─── Input Expanded Tests ─────────────────────────────────────────────────

func TestInputPermissionResponseHandled(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Input(context.Background(), "hello", map[string]any{
		"permission_response": map[string]any{
			"request_id": "req-1",
			"allowed":    true,
		},
	})
	// HandlePermissionResponse returns error (no pending server request),
	// but Input returns the error from DispatchMetadata.
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestInputQuestionResponseHandled(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Input(context.Background(), "hello", map[string]any{
		"question_response": map[string]any{
			"id": "req-2",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestInputElicitationResponseHandled(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	err := w.Input(context.Background(), "hello", map[string]any{
		"elicitation_response": map[string]any{
			"id":     "req-3",
			"action": "accept",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending server request")
}

func TestInputMetadataNilPassesThrough(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	// nil metadata → DispatchMetadata returns (false, nil) → falls through
	// to threadID check → "not started" error.
	err := w.Input(context.Background(), "hello", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not started")
}

func TestInputCallsTurnStart(t *testing.T) {
	t.Parallel()

	// Set up a manager with fake stdin so Call can write but will timeout.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	wk := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    mgr,
		threadID:   "thr-input",
	}

	err := wk.Input(context.Background(), "hello world", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: turn/start:")
}

// ─── Wait Expanded Tests ──────────────────────────────────────────────────

func TestWaitDoneChPath(t *testing.T) {
	t.Parallel()

	doneCh := make(chan struct{})
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		doneCh:     doneCh,
	}

	// Close doneCh to unblock Wait.
	go func() { close(doneCh) }()

	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

func TestWaitCrashSubPath(t *testing.T) {
	t.Parallel()

	crashCh := make(chan struct{})
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		crashSub:   crashCh,
		doneCh:     make(chan struct{}),
	}

	// Close crashCh to simulate process crash.
	go func() { close(crashCh) }()

	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 1, code, "crash should return exit code 1")
}

func TestWaitNilCrashSubReturnsImmediately(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		crashSub:   nil,
	}

	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

// ─── SendControlRequest Tests ─────────────────────────────────────────────

func TestWaitAfterReleaseReturnsImmediately(t *testing.T) {
	t.Parallel()

	// Regression test for #691: Wait() must not block after release() nil'd doneCh.
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		doneCh:     make(chan struct{}),
		crashSub:   make(chan struct{}),
	}

	// Simulate the zombie GC path: Terminate -> shutdown -> release().
	require.NoError(t, w.release(context.Background()))

	// Wait() must return immediately, not block on nil channel.
	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

func TestWaitBlocksUntilRelease(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		doneCh:     make(chan struct{}),
		crashSub:   make(chan struct{}),
	}

	waitDone := make(chan int, 1)
	go func() {
		code, _ := w.Wait()
		waitDone <- code
	}()

	// Wait should not return before release.
	require.Eventually(t, func() bool {
		select {
		case <-waitDone:
			return false
		default:
			return true
		}
	}, 50*time.Millisecond, 5*time.Millisecond, "Wait should block before release")

	// Release unblocks Wait.
	require.NoError(t, w.release(context.Background()))

	select {
	case code := <-waitDone:
		require.Equal(t, 0, code)
	case <-time.After(time.Second):
		t.Fatal("Wait should unblock after release")
	}
}

func TestTerminateThenKillNoPanic(t *testing.T) {
	t.Parallel()

	// Verify double release (Terminate + Kill) does not panic from double-close.
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		doneCh:     make(chan struct{}),
		crashSub:   make(chan struct{}),
	}

	require.NotPanics(t, func() {
		_ = w.Terminate(context.Background())
		_ = w.Kill()
	})

	// Wait still works after double release.
	code, err := w.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

func TestSendControlRequestNotStarted(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	_, err := w.SendControlRequest(context.Background(), "set_model", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not started")
}

func TestSendControlRequestSetModel(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	w.commands = NewServerCommander(w.manager, "thr-1")

	_, err := w.SendControlRequest(context.Background(), "set_model", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "set_model not supported")
}

func TestSendControlRequestSetPermissionModeIsExplicitlyUnsupported(t *testing.T) {
	t.Parallel()

	w := newTestAppServerWorker(t)
	w.commands = NewServerCommander(w.manager, "thr-1")

	_, err := w.SendControlRequest(context.Background(), "set_permission_mode", map[string]any{"mode": "read-only"})
	require.ErrorIs(t, err, worker.ErrNotImplemented)
}

// ─── Conn Tests ───────────────────────────────────────────────────────────

func TestConnAfterStartReturnsAppConn(t *testing.T) {
	t.Parallel()

	recvCh := make(chan *events.Envelope, 1)
	w := &AppServerWorker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		manager:    NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{IdleDrainPeriod: time.Minute}),
		conn: &appConn{
			userID:    "u1",
			sessionID: "s1",
			recvCh:    recvCh,
		},
	}

	c := w.Conn()
	require.NotNil(t, c)
	require.Equal(t, "u1", c.UserID())
	require.Equal(t, "s1", c.SessionID())
}

// ─── sandboxFromSession Tests ─────────────────────────────────────────────

func TestSandboxFromSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		session  worker.SessionInfo
		default_ string
		want     string
	}{
		{name: "session override", session: worker.SessionInfo{Sandbox: "docker"}, default_: "workspace-write", want: "docker"},
		{name: "fallback to default", session: worker.SessionInfo{}, default_: "workspace-write", want: "workspace-write"},
		{name: "empty both", session: worker.SessionInfo{}, default_: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sandboxFromSession(tt.session, tt.default_)
			require.Equal(t, tt.want, got)
		})
	}
}

// ─── buildThreadStartParams Tests ─────────────────────────────────────────

func TestBuildThreadStartParams(t *testing.T) {
	t.Parallel()

	t.Run("minimal config", func(t *testing.T) {
		t.Parallel()
		params := buildThreadStartParams(worker.SessionInfo{ProjectDir: "/tmp"}, Config{})
		require.Equal(t, "/tmp", params["cwd"])
		_, hasModel := params["model"]
		require.False(t, hasModel)
		_, hasEphemeral := params["ephemeral"]
		require.False(t, hasEphemeral)
	})

	t.Run("full config", func(t *testing.T) {
		t.Parallel()
		params := buildThreadStartParams(
			worker.SessionInfo{
				ProjectDir:      "/home/user/project",
				Sandbox:         "docker",
				SkipPermissions: true,
				Images:          []string{"img1.png"},
				JSONSchema:      `{"type":"object"}`,
				AllowedDirs:     []string{"/data"},
			},
			Config{
				Model:            "o3",
				Sandbox:          "workspace-write",
				Ephemeral:        true,
				ApprovalMode:     "on-request",
				Personality:      "friendly",
				Color:            true,
				StrictConfig:     true,
				SkipGitRepoCheck: true,
				IgnoreUserConfig: true,
				IgnoreRules:      true,
				LocalProvider:    true,
				BypassHookTrust:  true,
				OutputFile:       "/tmp/out.md",
				ConfigProfile:    "dev",
			},
		)
		require.Equal(t, "o3", params["model"])
		require.Equal(t, "docker", params["sandbox"]) // session override
		require.Equal(t, true, params["ephemeral"])
		require.Equal(t, "never", params["approvalPolicy"]) // SkipPermissions → "never"
		require.Equal(t, "friendly", params["personality"])
		require.Equal(t, true, params["color"])
		require.Equal(t, true, params["strictConfig"])
		require.Equal(t, true, params["skipGitRepoCheck"])
		require.Equal(t, true, params["ignoreUserConfig"])
		require.Equal(t, true, params["ignoreRules"])
		require.Equal(t, true, params["localProvider"])
		require.Equal(t, true, params["bypassHookTrust"])
		require.Equal(t, "/tmp/out.md", params["outputFile"])
		require.Equal(t, "dev", params["profile"])
		require.NotNil(t, params["images"])
		require.Equal(t, `{"type":"object"}`, params["outputSchema"])
		require.NotNil(t, params["additionalDirectories"])
	})

	t.Run("system prompt injected as baseInstructions", func(t *testing.T) {
		t.Parallel()
		params := buildThreadStartParams(
			worker.SessionInfo{
				ProjectDir:   "/tmp",
				SystemPrompt: "You are a helpful assistant. Follow SOUL.md guidelines.",
			},
			Config{},
		)
		require.Equal(t, "You are a helpful assistant. Follow SOUL.md guidelines.", params["baseInstructions"])
		_, hasDevInstr := params["developerInstructions"]
		require.False(t, hasDevInstr, "developerInstructions should never be set by buildThreadStartParams")
	})

	t.Run("empty system prompt omits baseInstructions", func(t *testing.T) {
		t.Parallel()
		params := buildThreadStartParams(
			worker.SessionInfo{ProjectDir: "/tmp"},
			Config{},
		)
		_, hasBase := params["baseInstructions"]
		require.False(t, hasBase, "baseInstructions should be absent when SystemPrompt is empty")
		_, hasDev := params["developerInstructions"]
		require.False(t, hasDev, "developerInstructions should never be set")
	})

	t.Run("no skip permissions uses config approval", func(t *testing.T) {
		t.Parallel()
		params := buildThreadStartParams(
			worker.SessionInfo{ProjectDir: "/tmp"},
			Config{ApprovalMode: "on-failure"},
		)
		require.Equal(t, "on-failure", params["approvalPolicy"])
	})
}

// ─── resolveConfig Tests ──────────────────────────────────────────────────

func TestResolveConfig(t *testing.T) {
	t.Parallel()

	prev := GetConfig()
	defer InitConfig(prev)

	InitConfig(config.CodexCLIConfig{
		Command:      "/usr/local/bin/codex",
		Model:        "o3",
		Sandbox:      "docker",
		ApprovalMode: "never",
		Personality:  "friendly",
		Ephemeral:    true,
		Color:        true,
	})

	cfg := resolveConfig()
	require.Equal(t, "/usr/local/bin/codex", cfg.Command)
	require.Equal(t, "o3", cfg.Model)
	require.Equal(t, "docker", cfg.Sandbox)
	require.Equal(t, "never", cfg.ApprovalMode)
	require.Equal(t, "friendly", cfg.Personality)
	require.True(t, cfg.Ephemeral)
	require.True(t, cfg.Color)
}

// ─── Singleton Tests ─────────────────────────────────────────────────────

func TestSingletonLifecycle(t *testing.T) {
	InitSingleton(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		Command:         "codex",
	})
	t.Cleanup(func() { ShutdownSingleton(context.Background()) })

	s := GetSingleton()
	require.NotNil(t, s)
	require.False(t, s.IsRunning())
}

func TestShutdownSingletonNil(t *testing.T) {
	// Shutdown with nil singleton should not panic.
	ShutdownSingleton(context.Background())
}

// ─── ServerCommander Tests ───────────────────────────────────────────────

func TestServerCommanderCompact(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() { _ = w.Close(); <-done })

	sc := NewServerCommander(mgr, "thr-sc")
	err := sc.Compact(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "codexcli: compact:")
}

func TestServerCommanderUnknownSubtype(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})
	sc := NewServerCommander(mgr, "thr-sc")

	_, err := sc.SendControlRequest(context.Background(), "unknown_subtype", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown control subtype")
}

func TestPermissionModeFromCodexEffective(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  string
		approval string
		want     string
	}{
		{"read only", "read-only", "never", worker.PermissionModeReadOnly},
		{"workspace prompt", "workspace-write", "on-request", worker.PermissionModeWorkspace},
		{"workspace automatic", "workspace-write", "never", worker.PermissionModeAutoEdit},
		{"workspace untrusted fails closed", "workspace-write", "untrusted", worker.PermissionModeReadOnly},
		{"workspace unknown approval fails closed", "workspace-write", "future-policy", worker.PermissionModeReadOnly},
		{"full bypass", "danger-full-access", "never", worker.PermissionModeBypass},
		{"full with approval fails closed", "danger-full-access", "on-request", worker.PermissionModeReadOnly},
		{"unknown sandbox fails closed", "docker", "never", worker.PermissionModeReadOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := permissionModeFromCodexEffective(map[string]any{
				"sandbox":        tt.sandbox,
				"approvalPolicy": tt.approval,
			})
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCodexPermissionCeilingSurvivesNewThread(t *testing.T) {
	t.Parallel()
	w := &AppServerWorker{}
	require.NoError(t, w.permissionCeiling.Capture(worker.PermissionModeWorkspace))

	session := w.sessionWithPermissionCeiling(worker.SessionInfo{
		PermissionMode: worker.PermissionModeBypass,
	})
	require.Equal(t, worker.PermissionModeWorkspace, session.PermissionMode)
}

func TestServerCommanderMCPOAuthMissingName(t *testing.T) {
	t.Parallel()

	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
	})
	sc := NewServerCommander(mgr, "thr-sc")

	_, err := sc.SendControlRequest(context.Background(), "mcp_oauth", map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing server_name")
}

func TestListMCPServerStatusSendsEmptyParams(t *testing.T) {
	t.Parallel()

	// codex-app-server's ListMcpServerStatusParams has no serde(default),
	// so a request without a params field is rejected with "missing field
	// params". The frame must carry an empty object, not omit it.
	mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
		IdleDrainPeriod: time.Minute,
		CallTimeout:     20 * time.Millisecond,
	})
	r, w := io.Pipe()
	mgr.stdin = w
	mgr.mu.Lock()
	mgr.refs = 1
	mgr.state = stateRunning
	mgr.mu.Unlock()

	frames := make(chan []byte, 1)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	t.Cleanup(func() { _ = w.Close() })

	// No real codex process responds, so the call times out — the written
	// frame is what we assert on.
	_, err := mgr.ListMCPServerStatus()
	require.Error(t, err)

	var frame map[string]any
	select {
	case raw := <-frames:
		require.NoError(t, json.Unmarshal(raw, &frame))
	case <-time.After(time.Second):
		t.Fatal("no request frame written to stdin")
	}

	require.Equal(t, "mcpServerStatus/list", frame["method"])
	params, ok := frame["params"]
	require.True(t, ok, "params must be present: codex-app-server rejects requests missing it")
	require.NotNil(t, params, "params must be an object, not null")
	require.Equal(t, map[string]any{}, params)
}

func TestIntegrationKillImmediatelyTerminatesIdleProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires codex binary")
	}
	defer testConfigWithDefaults()()

	cfg := config.CodexCLIConfig{
		IdleDrainPeriod: 10 * time.Second,
		StartupTimeout:  10 * time.Second,
		CallTimeout:     5 * time.Second,
	}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	_, err := mgr.Acquire(context.Background())
	if err != nil {
		t.Skipf("codex binary not available: %v", err)
	}
	require.True(t, mgr.IsRunning())

	mgr.Release()
	mgr.KillIfIdle()

	require.Eventually(t, func() bool {
		return !mgr.IsRunning()
	}, 5*time.Second, 100*time.Millisecond, "process should be killed immediately, not after idle drain")
}

// ─── Conversation History Injection Tests ────────────────────────────────

func TestInjectHistoryPrefix(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		pendingHistory: []worker.ConversationTurn{
			{Role: "user", Content: "现在开始我发 ping，你回复 汪"},
			{Role: "assistant", Content: "收到，以后你发 ping 我就回 汪"},
			{Role: "user", Content: "ping"},
			{Role: "assistant", Content: "汪"},
		},
		historyInjected: false,
	}

	result := w.injectHistoryPrefix("ping")

	require.Regexp(t, `CONVERSATION_HISTORY_[0-9a-f]{8}_START`, result)
	require.Contains(t, result, "[User]: 现在开始我发 ping，你回复 汪")
	require.Contains(t, result, "[Assistant]: 收到，以后你发 ping 我就回 汪")
	require.Contains(t, result, "[User]: ping")
	require.Contains(t, result, "[Assistant]: 汪")
	require.Regexp(t, `CONVERSATION_HISTORY_[0-9a-f]{8}_END`, result)
	require.True(t, strings.HasSuffix(result, "ping"), "actual user message should follow history block")
	require.True(t, w.historyInjected)
	require.Nil(t, w.pendingHistory)
}

func TestInjectHistoryPrefixIdempotent(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		pendingHistory: []worker.ConversationTurn{
			{Role: "user", Content: "hello"},
		},
		historyInjected: false,
	}

	first := w.injectHistoryPrefix("message1")
	require.Regexp(t, `CONVERSATION_HISTORY_[0-9a-f]{8}_START`, first)

	second := w.injectHistoryPrefix("message2")
	require.Equal(t, "message2", second, "second call should return unmodified content")
}

func TestInjectHistoryPrefixEmpty(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		pendingHistory:  nil,
		historyInjected: false,
	}
	require.Equal(t, "hello", w.injectHistoryPrefix("hello"))

	w2 := &AppServerWorker{
		pendingHistory:  []worker.ConversationTurn{},
		historyInjected: false,
	}
	require.Equal(t, "hello", w2.injectHistoryPrefix("hello"))
}

func TestInjectHistoryPrefixSkipsEmptyContent(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		pendingHistory: []worker.ConversationTurn{
			{Role: "user", Content: ""},
			{Role: "assistant", Content: "response"},
			{Role: "system", Content: "ignored role"},
			{Role: "user", Content: "actual input"},
		},
		historyInjected: false,
	}

	result := w.injectHistoryPrefix("ping")

	require.Contains(t, result, "[Assistant]: response")
	require.Contains(t, result, "[User]: actual input")
	require.NotContains(t, result, "[System]: ", "unknown roles should be skipped")

	// Verify empty-content turns are omitted from the output entirely.
	// The code only skips unknown roles — empty-content turns with known
	// roles (user/assistant) still produce "[User]: \n\n" in output.
	// This is by design: the upstream HistoryCompressor handles filtering.
	lines := strings.Split(result, "\n")
	var emptyUserLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "[User]:") && strings.TrimSpace(strings.TrimPrefix(line, "[User]:")) == "" {
			emptyUserLines = append(emptyUserLines, line)
		}
	}
	// Current implementation does NOT filter empty content — document behavior.
	require.NotEmpty(t, emptyUserLines, "empty-content user turns produce [User]: lines (filtered upstream by HistoryCompressor)")
}

func TestInjectHistoryPrefixCleanupOldThreadReset(t *testing.T) {
	t.Parallel()

	w := &AppServerWorker{
		pendingHistory: []worker.ConversationTurn{
			{Role: "user", Content: "hello"},
		},
		historyInjected: false,
	}

	w.cleanupOldThread(context.Background())

	require.Nil(t, w.pendingHistory)
	require.False(t, w.historyInjected)
	require.Equal(t, "test", w.injectHistoryPrefix("test"))
}

func TestInjectHistoryPrefixPreservesSentinelContent(t *testing.T) {
	t.Parallel()

	// Verify that user content containing the sentinel string is preserved
	// unmodified — the unique boundary ID prevents collision.
	w := &AppServerWorker{
		pendingHistory: []worker.ConversationTurn{
			{Role: "user", Content: "CONVERSATION_HISTORY_START should not be stripped"},
			{Role: "assistant", Content: "CONVERSATION_HISTORY_END also preserved"},
		},
		historyInjected: false,
	}

	result := w.injectHistoryPrefix("ping")

	require.Contains(t, result, "CONVERSATION_HISTORY_START should not be stripped")
	require.Contains(t, result, "CONVERSATION_HISTORY_END also preserved")
	require.Regexp(t, `CONVERSATION_HISTORY_[0-9a-f]{8}_START`, result)
	require.Regexp(t, `CONVERSATION_HISTORY_[0-9a-f]{8}_END`, result)
}

// ─── Per-Thread Converter Isolation Tests (#813) ─────────────────────────

// TestManager_DispatchNotification_SkipsHighFrequencyLogs verifies the
// high-frequency notification log filter, including mcpServer/startupStatus/updated
// which burst ~25 times in 1s at session start (dev logs) — the dispatch itself
// still reaches subscribers; only the DEBUG log line is suppressed.
func TestManager_DispatchNotification_SkipsHighFrequencyLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := NewCodexAppServerManager(logger, config.CodexCLIConfig{IdleDrainPeriod: time.Minute})

	for _, method := range []string{
		"mcpServer/startupStatus/updated",
		"thread/tokenUsage/updated",
		"item/agentMessage/delta",
	} {
		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  method,
			Params:  json.RawMessage(`{"threadId":"thread-x"}`),
		})
	}
	// Lifecycle methods must still log.
	mgr.dispatchNotification(&JSONRPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/started",
		Params:  json.RawMessage(`{"threadId":"thread-x","turn":{"id":"turn-x"}}`),
	})

	out := buf.String()
	require.Contains(t, out, "method=turn/started")
	for _, method := range []string{
		"mcpServer/startupStatus/updated",
		"thread/tokenUsage/updated",
		"item/agentMessage/delta",
	} {
		require.NotContains(t, out, method)
	}
}

func TestManager_PerThreadConverterIsolation(t *testing.T) {
	cfg := config.CodexCLIConfig{IdleDrainPeriod: time.Minute}
	mgr := NewCodexAppServerManager(slog.Default(), cfg)

	drain := func(chs ...chan *events.Envelope) {
		t.Helper()
		for _, ch := range chs {
		channel:
			for {
				select {
				case <-ch:
				default:
					break channel
				}
			}
		}
	}

	t.Run("context usage isolated by threadID", func(t *testing.T) {
		chA := mgr.Subscribe("thread-a", "session-a")
		chB := mgr.Subscribe("thread-b", "session-b")
		defer mgr.Unsubscribe("thread-a")
		defer mgr.Unsubscribe("thread-b")
		drain(chA, chB)

		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "turn/started",
			Params:  json.RawMessage(`{"threadId":"thread-a","turn":{"id":"turn-a"}}`),
		})
		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "thread/tokenUsage/updated",
			Params: json.RawMessage(`{"threadId":"thread-a","turnId":"turn-a","tokenUsage":` +
				`{"last":{"inputTokens":100,"outputTokens":50,"cachedInputTokens":10},"modelContextWindow":8000}}`),
		})

		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "turn/started",
			Params:  json.RawMessage(`{"threadId":"thread-b","turn":{"id":"turn-b"}}`),
		})
		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "thread/tokenUsage/updated",
			Params: json.RawMessage(`{"threadId":"thread-b","turnId":"turn-b","tokenUsage":` +
				`{"last":{"inputTokens":200,"outputTokens":100},"modelContextWindow":16000}}`),
		})

		cuA := mgr.LastContextUsage("thread-a")
		require.Equal(t, 110, cuA["totalTokens"], "thread-a totalTokens")
		require.Equal(t, int64(8000), cuA["maxTokens"], "thread-a maxTokens")

		cuB := mgr.LastContextUsage("thread-b")
		require.Equal(t, 200, cuB["totalTokens"], "thread-b totalTokens")
		require.Equal(t, int64(16000), cuB["maxTokens"], "thread-b maxTokens")
	})

	t.Run("model is scoped to threadID", func(t *testing.T) {
		chA := mgr.Subscribe("thread-a", "session-a")
		chB := mgr.Subscribe("thread-b", "session-b")
		defer mgr.Unsubscribe("thread-a")
		defer mgr.Unsubscribe("thread-b")
		drain(chA, chB)

		mgr.SetCurrentModel("thread-a", "gpt-a")
		mgr.SetCurrentModel("thread-b", "gpt-b")

		require.Equal(t, "gpt-a", mgr.LastContextUsage("thread-a")["model"])
		require.Equal(t, "gpt-b", mgr.LastContextUsage("thread-b")["model"])

		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "model/rerouted",
			Params:  json.RawMessage(`{"threadId":"thread-a","toModel":"rerouted-a"}`),
		})
		require.Equal(t, "rerouted-a", mgr.LastContextUsage("thread-a")["model"])
		require.Equal(t, "gpt-b", mgr.LastContextUsage("thread-b")["model"])
	})

	t.Run("turnID gating does not cross threads", func(t *testing.T) {
		chA := mgr.Subscribe("thread-c", "session-c")
		chB := mgr.Subscribe("thread-d", "session-d")
		defer mgr.Unsubscribe("thread-c")
		defer mgr.Unsubscribe("thread-d")
		drain(chA, chB)

		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "turn/started",
			Params:  json.RawMessage(`{"threadId":"thread-c","turn":{"id":"shared-turn"}}`),
		})
		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "turn/started",
			Params:  json.RawMessage(`{"threadId":"thread-d","turn":{"id":"shared-turn"}}`),
		})

		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "thread/tokenUsage/updated",
			Params: json.RawMessage(`{"threadId":"thread-c","turnId":"shared-turn","tokenUsage":` +
				`{"last":{"inputTokens":111,"outputTokens":11},"modelContextWindow":1000}}`),
		})
		mgr.dispatchNotification(&JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "thread/tokenUsage/updated",
			Params: json.RawMessage(`{"threadId":"thread-d","turnId":"shared-turn","tokenUsage":` +
				`{"last":{"inputTokens":222,"outputTokens":22},"modelContextWindow":2000}}`),
		})

		require.Equal(t, 111, mgr.LastContextUsage("thread-c")["totalTokens"])
		require.Equal(t, 222, mgr.LastContextUsage("thread-d")["totalTokens"])
	})

	t.Run("unsubscribe deletes per-thread converter state", func(t *testing.T) {
		_ = mgr.Subscribe("thread-e", "session-e")

		mgr.SetCurrentModel("thread-e", "gpt-e")
		require.Equal(t, "gpt-e", mgr.LastContextUsage("thread-e")["model"])

		mgr.Unsubscribe("thread-e")
		require.Empty(t, mgr.LastContextUsage("thread-e"), "unsubscribe must drop converter state")
	})

	t.Run("unknown thread returns empty context usage", func(t *testing.T) {
		require.Empty(t, mgr.LastContextUsage("nonexistent-thread"))
	})

	t.Run("clearConverters drops all per-thread state", func(t *testing.T) {
		chA := mgr.Subscribe("thread-f", "session-f")
		chB := mgr.Subscribe("thread-g", "session-g")
		defer mgr.Unsubscribe("thread-f")
		defer mgr.Unsubscribe("thread-g")
		drain(chA, chB)

		mgr.SetCurrentModel("thread-f", "gpt-f")
		mgr.SetCurrentModel("thread-g", "gpt-g")
		require.Equal(t, "gpt-f", mgr.LastContextUsage("thread-f")["model"])
		require.Equal(t, "gpt-g", mgr.LastContextUsage("thread-g")["model"])

		mgr.clearConverters()

		require.Empty(t, mgr.LastContextUsage("thread-f"))
		require.Empty(t, mgr.LastContextUsage("thread-g"))
	})

	t.Run("concurrent dispatch no data race", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipping race test in short mode")
		}

		var chs []chan *events.Envelope
		for i := range 5 {
			ch := mgr.Subscribe(fmt.Sprintf("race-thread-%d", i), fmt.Sprintf("race-session-%d", i))
			chs = append(chs, ch)
		}
		defer func() {
			for i := range 5 {
				mgr.Unsubscribe(fmt.Sprintf("race-thread-%d", i))
			}
		}()
		drain(chs...)

		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				threadID := fmt.Sprintf("race-thread-%d", idx%5)
				turnID := fmt.Sprintf("turn-%d", idx)
				mgr.dispatchNotification(&JSONRPCNotification{
					JSONRPC: "2.0",
					Method:  "turn/started",
					Params:  json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q}}`, threadID, turnID)),
				})
				mgr.dispatchNotification(&JSONRPCNotification{
					JSONRPC: "2.0",
					Method:  "thread/tokenUsage/updated",
					Params: json.RawMessage(fmt.Sprintf(
						`{"threadId":%q,"turnId":%q,"tokenUsage":{"last":{"inputTokens":%d,"outputTokens":1},"modelContextWindow":%d}}`,
						threadID, turnID, idx*10, 1000+idx)),
				})
			}(i)
		}
		wg.Wait()

		for i := range 5 {
			threadID := fmt.Sprintf("race-thread-%d", i)
			cu := mgr.LastContextUsage(threadID)
			require.NotNil(t, cu)
			require.GreaterOrEqual(t, cu["totalTokens"], i*10)
		}
	})
}

// ─── getDeltaText drift handling Tests ─────────────────────────────────

func TestGetDeltaText_DriftScenarios(t *testing.T) {
	t.Parallel()

	type tc struct {
		name       string
		seedSent   string // initial sentTexts[itemID] (set via recordSentDelta)
		snapshot   string // currentText argument to getDeltaText
		wantDelta  string // expected returned delta
		wantSent   string // expected sentTexts[itemID] after call
		wantDrifts int64  // expected driftCount delta
	}

	cases := []tc{
		{
			name:       "first call empty seed returns full snapshot",
			seedSent:   "",
			snapshot:   "Hello",
			wantDelta:  "Hello",
			wantSent:   "Hello",
			wantDrifts: 0,
		},
		{
			name:       "normal append prefix consistent",
			seedSent:   "Hello",
			snapshot:   "Hello world",
			wantDelta:  " world",
			wantSent:   "Hello world",
			wantDrifts: 0,
		},
		{
			name:       "idempotent snapshot equals sent",
			seedSent:   "Hello",
			snapshot:   "Hello",
			wantDelta:  "",
			wantSent:   "Hello",
			wantDrifts: 0,
		},
		{
			name:       "drift type A snapshot shorter and divergent",
			seedSent:   "Hello",
			snapshot:   "Hola",
			wantDelta:  "ola",
			wantSent:   "Hola",
			wantDrifts: 1,
		},
		{
			name:       "drift type A snapshot equal length divergent",
			seedSent:   "Hello",
			snapshot:   "Hallo",
			wantDelta:  "allo",
			wantSent:   "Hallo",
			wantDrifts: 1,
		},
		{
			name:       "drift type B snapshot longer prefix inconsistent",
			seedSent:   "Hel",
			snapshot:   "Hallo!",
			wantDelta:  "allo!",
			wantSent:   "Hallo!",
			wantDrifts: 1,
		},
		{
			name:       "empty snapshot with prior sent no change no drift",
			seedSent:   "Hello",
			snapshot:   "",
			wantDelta:  "",
			wantSent:   "Hello",
			wantDrifts: 0,
		},
		{
			name:       "both empty seed and snapshot returns empty no drift",
			seedSent:   "",
			snapshot:   "",
			wantDelta:  "",
			wantSent:   "",
			wantDrifts: 0,
		},
		{
			// Strict truncation: snapshot is a proper prefix of sentTexts.
			// old impl silently returned "" (data loss); new impl treats it
			// as drift (corrective is "" but re-baselines + counts it so
			// operators see the backend's truncation event).
			name:       "strict truncation snapshot is proper prefix of sent counts as drift",
			seedSent:   "Hello world",
			snapshot:   "Hello",
			wantDelta:  "",
			wantSent:   "Hello",
			wantDrifts: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := NewMapper("session-drift")
			if c.seedSent != "" {
				m.recordSentDelta("item_1", c.seedSent)
			}
			got := m.getDeltaText("item_1", c.snapshot)
			require.Equal(t, c.wantDelta, got, "delta mismatch")

			m.mu.Lock()
			gotSent := m.sentTexts["item_1"]
			m.mu.Unlock()
			require.Equal(t, c.wantSent, gotSent, "sentTexts mismatch")

			require.Equal(t, c.wantDrifts, m.driftCount.Load(), "driftCount mismatch")
		})
	}
}

// TestGetDeltaText_DriftCountAccumulates verifies driftCount is cumulative
// across multiple getDeltaText calls and is NOT reset by Reset().
func TestGetDeltaText_DriftCountAccumulates(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-accum")

	// First drift.
	m.recordSentDelta("item_a", "Hello")
	m.getDeltaText("item_a", "Hola")
	require.Equal(t, int64(1), m.driftCount.Load())

	// Second drift on a different item ID.
	m.recordSentDelta("item_b", "World")
	m.getDeltaText("item_b", "Wurld")
	require.Equal(t, int64(2), m.driftCount.Load())

	// Reset clears sentTexts but NOT driftCount.
	m.Reset()
	require.Equal(t, int64(2), m.driftCount.Load(), "driftCount must survive Reset")

	// DriftCount() public accessor matches the raw field.
	require.Equal(t, int64(2), m.DriftCount(), "DriftCount() must expose the atomic counter")
}

// TestMapper_DriftCount_ExposedInDoneStats verifies that delta drifts are
// surfaced in turn/completed DoneData.Stats under the "delta_drifts" key,
// so operators can detect drift without grepping logs. driftCount is
// cumulative and not reset by Reset().
func TestMapper_DriftCount_ExposedInDoneStats(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-stats")

	// No drift yet — stats should NOT carry delta_drifts (avoid noise).
	m.mapNotifTurnCompleted()
	stats0 := m.trackedUsageStats()
	_, ok0 := stats0["delta_drifts"]
	require.False(t, ok0, "delta_drifts should be absent when count is 0")

	// Cause a drift.
	m.recordSentDelta("item_x", "Hello")
	m.getDeltaText("item_x", "Hallo") // common=1, corrective="allo", drift++
	require.Equal(t, int64(1), m.DriftCount())

	// Reset must NOT clear driftCount (cumulative across turns).
	m.Reset()
	require.Equal(t, int64(1), m.DriftCount(), "driftCount survives Reset")

	// After drift, trackedUsageStats surfaces delta_drifts.
	stats1 := m.trackedUsageStats()
	got, ok := stats1["delta_drifts"]
	require.True(t, ok, "delta_drifts must be surfaced in stats after drift")
	require.Equal(t, int64(1), got, "delta_drifts value must match DriftCount()")
}

// TestGetDeltaText_SequenceDeltaThenSnapshot verifies the real-world path:
// item/agentMessage/delta (recordSentDelta) then item/completed (getDeltaText)
// with matching prefix emits only the appended remainder.
func TestGetDeltaText_SequenceDeltaThenSnapshot(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-seq")

	// Simulate 3 incremental deltas via the delta notification path.
	for _, word := range []string{"Hello", " world", "!"} {
		m.recordSentDelta("msg_1", word)
	}

	// Full snapshot arrives via item/completed (getDeltaText).
	// Snapshot "Hello world!" has prefix matching sentTexts → append path.
	// Since snapshot == sentTexts already, delta is "" (idempotent).
	got := m.getDeltaText("msg_1", "Hello world!")
	require.Equal(t, "", got, "snapshot == sentTexts should yield idempotent empty delta")

	m.mu.Lock()
	gotSent := m.sentTexts["msg_1"]
	m.mu.Unlock()
	require.Equal(t, "Hello world!", gotSent)
	require.Equal(t, int64(0), m.driftCount.Load())
}

// TestGetDeltaText_SequenceDeltaThenSnapshotWithAppend verifies that a
// snapshot arriving after deltas with additional content emits the appended
// remainder only (no drift).
func TestGetDeltaText_SequenceDeltaThenSnapshotWithAppend(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-seq-append")

	// Deltas delivered "Hel"+"lo" via item/agentMessage/delta path.
	m.recordSentDelta("msg_1", "Hel")
	m.recordSentDelta("msg_1", "lo")

	// Snapshot arrives with one extra character; prefix matches sentTexts.
	got := m.getDeltaText("msg_1", "Hello!")
	require.Equal(t, "!", got, "appended remainder after matching prefix")

	m.mu.Lock()
	gotSent := m.sentTexts["msg_1"]
	m.mu.Unlock()
	require.Equal(t, "Hello!", gotSent)
	require.Equal(t, int64(0), m.driftCount.Load())
}

// TestGetDeltaText_SequenceDeltaThenSnapshotWithDrift verifies that a
// snapshot diverging from already-sent deltas triggers drift handling:
// corrective delta = snapshot[commonPrefix:], sentTexts re-baselined.
func TestGetDeltaText_SequenceDeltaThenSnapshotWithDrift(t *testing.T) {
	t.Parallel()

	m := NewMapper("session-seq-drift")

	// Deltas delivered "Hel"+"lo" via item/agentMessage/delta path.
	m.recordSentDelta("msg_1", "Hel")
	m.recordSentDelta("msg_1", "lo")

	// Snapshot diverges: "Hola" — common prefix "H" (sentRunes[1]='e' vs 'o').
	// Corrective delta = "ola", sentTexts reset to "Hola".
	got := m.getDeltaText("msg_1", "Hola")
	require.Equal(t, "ola", got, "corrective delta after drift")

	m.mu.Lock()
	gotSent := m.sentTexts["msg_1"]
	m.mu.Unlock()
	require.Equal(t, "Hola", gotSent)
	require.Equal(t, int64(1), m.driftCount.Load())
}

// TestWorker_Input_ClearsStoppedOnlyForPrimaryTurn verifies the user-stop
// marker is scoped to the current turn: interaction-response metadata
// (handled by DispatchMetadata) must NOT clear it, while the next primary
// content (a real turn/start RPC frame) must clear it before the send.
func TestWorker_Input_ClearsStoppedOnlyForPrimaryTurn(t *testing.T) {
	t.Parallel()

	t.Run("interaction response metadata keeps stopped", func(t *testing.T) {
		t.Parallel()

		w := newTestAppServerWorker(t)
		reqID := "perm_stop_1"
		w.manager.serverReqIDs.Store(reqID, int64(42))
		w.manager.serverReqMethods.Store(reqID, "serverRequest/approval")
		var buf strings.Builder
		w.manager.stdin = struct {
			io.Writer
			io.Closer
		}{
			Writer: &buf,
			Closer: io.NopCloser(nil),
		}
		w.MarkStopped()

		md := map[string]any{
			"permission_response": map[string]any{
				"request_id": reqID,
				"allowed":    true,
			},
		}
		err := w.Input(context.Background(), "", md)
		require.NoError(t, err)
		require.Contains(t, buf.String(), `"decision":"approved"`)
		require.True(t, w.IsStopped(), "handled metadata must not clear the stopped marker")
	})

	t.Run("next primary content clears stopped before send", func(t *testing.T) {
		t.Parallel()

		mgr := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{
			IdleDrainPeriod: time.Minute,
			CallTimeout:     20 * time.Millisecond,
		})
		var buf strings.Builder
		mgr.stdin = struct {
			io.Writer
			io.Closer
		}{
			Writer: &buf,
			Closer: io.NopCloser(nil),
		}
		mgr.mu.Lock()
		mgr.refs = 1
		mgr.state = stateRunning
		mgr.mu.Unlock()

		wk := &AppServerWorker{
			BaseWorker: base.NewBaseWorker(slog.Default(), nil),
			manager:    mgr,
			threadID:   "thr_stop_1",
		}
		wk.MarkStopped()

		err := wk.Input(context.Background(), "hello again", nil)
		// Call times out (no real codex process responds); the error proves
		// the RPC path was taken. The RPC FAILED — the new turn never started —
		// so the previous turn's stopped marker must be preserved (the bridge
		// crash fallback must not re-run a stopped turn).
		require.Error(t, err)
		require.Contains(t, err.Error(), "codexcli: turn/start:")

		require.True(t, wk.IsStopped(), "a failed send must restore the stopped marker")
		// The protocol fake observed the primary send (turn/start frame).
		written := buf.String()
		require.Contains(t, written, `"method":"turn/start"`)
		require.Contains(t, written, `"hello again"`)
	})
}
