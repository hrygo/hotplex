package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/agentconfig"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// ─── Test NewBridge ───────────────────────────────────────────────────────────

func TestNewBridge(t *testing.T) {
	log := slog.Default()
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	b := NewBridge(BridgeDeps{Log: log, Hub: hub, SM: sm})

	require.NotNil(t, b)
	assert.Same(t, sm, b.sm)
	assert.Equal(t, hub, b.hub)
}

// ─── Test Bridge SetWorkerFactory ─────────────────────────────────────────────

func TestBridge_SetWorkerFactory(t *testing.T) {
	log := slog.Default()
	sm := new(mockBridgeSM)
	b := &Bridge{log: log, sm: sm, wf: defaultWorkerFactory{}}

	wf := &mockBridgeWorkerFactory{}
	b.SetWorkerFactory(wf)
	assert.Same(t, wf, b.wf)
}

func TestBridge_StartFreshWorker_IsolatesOldRunAndStartsWithoutResume(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-fenced-fresh"
	agentConfigDir := t.TempDir()
	writeAgentConfigFile(t, agentConfigDir, "SOUL.md", "fresh-start persona")
	oldWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	freshEvents := make(chan *events.Envelope)
	freshWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: freshEvents},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateRunning, WorkerSessionID: "provider-session-old",
		Platform: "slack", BotID: "bot-1", BotName: "primary",
		PermissionCeiling: "workspace", WorkDir: "/private/work",
		PlatformKey: map[string]string{"channel_id": "C-private"},
	}, nil).Once()
	sm.On("GetWorker", sessionID).Return(oldWorker).Once()
	sm.On("DetachWorkerIf", sessionID, oldWorker).Return(true).Once()
	sm.On("AttachWorker", sessionID, freshWorker).Return(nil).Once()
	sm.On("GetWorker", sessionID).Return(freshWorker).Once()
	sm.On("DetachWorkerIf", sessionID, freshWorker).Return(true).Maybe()

	b := NewBridge(BridgeDeps{
		Log:            testLogger(t),
		Hub:            newTestHub(t),
		SM:             sm,
		AgentConfigDir: agentConfigDir,
	})
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{freshWorker}})
	runID, err := b.StartFreshWorker(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, runID)
	require.True(t, oldWorker.terminated.Load(), "old fenced worker must be terminated before fresh start")
	currentRunID, ok := b.CurrentWorkerRunID(sessionID)
	require.True(t, ok)
	require.Equal(t, runID, currentRunID)
	require.False(t, freshWorker.terminated.Load())
	require.Empty(t, freshWorker.startInfo.WorkerSessionID, "fresh start must not load the fenced provider session")
	require.Contains(t, freshWorker.startInfo.SystemPrompt, `<runtime-facts format="application/json" schema-version="1">`)
	require.Contains(t, freshWorker.startInfo.SystemPrompt, `"platform":"slack"`)
	require.Contains(t, freshWorker.startInfo.SystemPrompt, `"worker_type":"claude_code"`)
	require.Contains(t, freshWorker.startInfo.SystemPrompt, `"scope_kind":"bot"`)
	require.Contains(t, freshWorker.startInfo.SystemPrompt, `"declared_permission_mode":"workspace"`)
	require.NotContains(t, freshWorker.startInfo.SystemPrompt, "private/work")
	require.NotContains(t, freshWorker.startInfo.SystemPrompt, "C-private")
	require.NotContains(t, freshWorker.startInfo.SystemPrompt, "bot-1")
	sm.AssertExpectations(t)

	b.closed.Store(true)
	close(freshEvents)
	b.WaitForwarders(context.Background())
}

func TestBridge_ClearWorkerRunDoesNotDeleteReplacement(t *testing.T) {
	t.Parallel()
	b := &Bridge{}
	w := &mockBridgeWorker{}
	b.workerRuns.Store("session-binding", workerRunBinding{worker: w, id: "run-new"})

	b.clearWorkerRun("session-binding", w, "run-old")
	value, loaded := b.workerRuns.Load("session-binding")
	require.True(t, loaded)
	require.Equal(t, "run-new", value.(workerRunBinding).id)

	b.clearWorkerRun("session-binding", w, "run-new")
	_, loaded = b.workerRuns.Load("session-binding")
	require.False(t, loaded)
}

// ─── Test Shutdown ────────────────────────────────────────────────────────────

func TestBridge_Shutdown(t *testing.T) {
	log := slog.Default()
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	b := NewBridge(BridgeDeps{Log: log, Hub: hub, SM: sm})

	b.Shutdown(context.Background())
	assert.True(t, b.closed.Load())

	// Idempotent.
	b.Shutdown(context.Background())
	assert.True(t, b.closed.Load())
}

func TestBridge_Shutdown_RejectNewSession(t *testing.T) {
	log := slog.Default()
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	b := NewBridge(BridgeDeps{Log: log, Hub: hub, SM: sm})

	b.Shutdown(context.Background())

	// After shutdown, StartSession should be rejected.
	err := b.StartSession(context.Background(), worker.SessionStartParams{ID: "sess-closed", UserID: "u", BotID: "b", WorkerType: worker.TypeClaudeCode})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown")
}

// ─── Test buildNotifyEnvelope ────────────────────────────────────────────────

func TestBuildNotifyEnvelope(t *testing.T) {
	env := buildNotifyEnvelope("sess-1", "hello world", 42)

	require.NotNil(t, env)
	assert.NotEmpty(t, env.ID)
	assert.Equal(t, "sess-1", env.SessionID)
	assert.Equal(t, int64(42), env.Seq)
	assert.Equal(t, events.Message, env.Event.Type)

	data, ok := env.Event.Data.(map[string]any)
	require.True(t, ok, "expected map[string]any, got %T", env.Event.Data)
	assert.Equal(t, "hello world", data["content"])
}

func TestBuildNotifyEnvelope_EmptyMessage(t *testing.T) {
	env := buildNotifyEnvelope("sid", "", 1)
	assert.NotNil(t, env)
	assert.Equal(t, "sid", env.SessionID)
}

// ─── Test sanitizeLastInput ──────────────────────────────────────────────────

func TestSanitizeLastInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"plain text preserved", "Hello, how are you?", "Hello, how are you?"},
		{"control command /gc removed", "/gc", ""},
		{"control command /reset removed", "/reset", ""},
		{"control command /park removed", "/park", ""},
		{"control command /new removed", "/new", ""},
		{"dollar-prefix $gc removed", "$gc", ""},
		{"dollar-prefix $reset removed", "$reset", ""},
		{"mixed control line removed", "Hello\n/gc\nWorld", "Hello\nWorld"},
		{"all control lines removed", "/reset\n/park\n/new", ""},
		{"multiline user input preserved", "Here is my code:\nfunc main() {}\nPlease review", "Here is my code:\nfunc main() {}\nPlease review"},
		{"mixed control and user content", "$gc\nActual question: how do I fix this?", "Actual question: how do I fix this?"},
		{"leading control removed", "/gc\nImportant message", "Important message"},
		{"trailing control removed", "Important message\n/gc", "Important message"},
		{"whitespace line preserved (not a control cmd)", "/gc\n   \n/park", "   "},
		{"cd command removed", "/cd /tmp/project", ""},
		{"dollar cd removed", "$cd /tmp", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeLastInput(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ─── Test sessionAccumulator helpers (not covered by session_stats_test.go) ───

func TestSessionAccumulator_ComputePerTurnDeltas(t *testing.T) {
	acc := &sessionAccumulator{
		StartedAt:     time.Now(),
		TotalInput:    100,
		TotalOutput:   50,
		TotalCostUSD:  0.01,
		PrevTotalIn:   0,
		PrevTotalOut:  0,
		PrevTotalCost: 0,
	}
	acc.computePerTurnDeltas()

	assert.Equal(t, int64(100), acc.PerTurnInput)
	assert.Equal(t, int64(50), acc.PerTurnOutput)
	assert.Equal(t, 0.01, acc.PerTurnCost)

	// Second call without resetPerTurn: deltas remain the same (no baseline advance).
	acc.computePerTurnDeltas()
	assert.Equal(t, int64(100), acc.PerTurnInput)
}

func TestSessionAccumulator_ResetPerTurn(t *testing.T) {
	acc := &sessionAccumulator{
		StartedAt:     time.Now(),
		PrevTotalIn:   100,
		PrevTotalOut:  50,
		PrevTotalCost: 0.01,
		ToolNames:     map[string]int{"Read": 2},
	}
	acc.ToolCallCount.Store(5)
	acc.PerTurnInput = 100
	acc.PerTurnOutput = 50
	acc.PerTurnCost = 0.01

	acc.resetPerTurn()

	assert.Equal(t, int64(0), acc.PerTurnInput)
	assert.Equal(t, int64(0), acc.PerTurnOutput)
	assert.Equal(t, 0.0, acc.PerTurnCost)
	assert.Nil(t, acc.ToolNames)
	assert.Equal(t, int32(0), acc.ToolCallCount.Load())
}

func TestSessionAccumulator_NegativeDeltasClamped(t *testing.T) {
	// If TotalInput drops below PrevTotalIn (shouldn't happen but guard against it).
	acc := &sessionAccumulator{
		TotalInput:  10,
		PrevTotalIn: 100,
	}
	acc.computePerTurnDeltas()
	assert.Equal(t, int64(0), acc.PerTurnInput)
}

func TestComputeContextPct_ZeroWindow(t *testing.T) {
	acc := &sessionAccumulator{ContextWindow: 0, TotalInput: 50000}
	assert.Equal(t, 0.0, acc.computeContextPct())
}

// ─── Test getOrInitAccum ─────────────────────────────────────────────────────

func TestGetOrInitAccum(t *testing.T) {
	log := slog.Default()
	sm := new(mockBridgeSM)
	b := &Bridge{
		log:   log,
		sm:    sm,
		accum: make(map[string]*sessionAccumulator),
	}

	acc1 := b.getOrInitAccum("sess-1", "", time.Now())
	require.NotNil(t, acc1)

	acc2 := b.getOrInitAccum("sess-1", "", time.Now())
	assert.Same(t, acc1, acc2)

	acc3 := b.getOrInitAccum("sess-2", "", time.Now())
	assert.NotSame(t, acc1, acc3)
}

func TestGetOrInitAccum_LazyUpdate(t *testing.T) {
	t.Parallel()
	log := slog.Default()
	b := &Bridge{
		log:   log,
		sm:    new(mockBridgeSM),
		accum: make(map[string]*sessionAccumulator),
	}

	// First call creates accumulator with empty workDir.
	acc := b.getOrInitAccum("sess-1", "", time.Now())
	require.NotNil(t, acc)
	assert.Equal(t, "", acc.WorkDir)
	assert.Equal(t, "", acc.GitBranch)

	// Second call with workDir lazily updates the existing accumulator.
	same := b.getOrInitAccum("sess-1", "/tmp/project", time.Now())
	assert.Same(t, acc, same)
	assert.Equal(t, "/tmp/project", acc.WorkDir)

	// Third call with different workDir does NOT overwrite (already set).
	b.getOrInitAccum("sess-1", "/other", time.Now())
	assert.Equal(t, "/tmp/project", acc.WorkDir)
}

func TestGetOrInitAccum_EmptyWorkDirNoOp(t *testing.T) {
	t.Parallel()
	log := slog.Default()
	b := &Bridge{
		log:   log,
		sm:    new(mockBridgeSM),
		accum: make(map[string]*sessionAccumulator),
	}

	acc := b.getOrInitAccum("sess-1", "", time.Now())
	require.NotNil(t, acc)

	// Calling again with empty workDir should not change anything.
	same := b.getOrInitAccum("sess-1", "", time.Now())
	assert.Same(t, acc, same)
	assert.Equal(t, "", acc.WorkDir)
}

// TestBridge_SwitchWorkDir_WorkspaceBoundRejected verifies the bridge-level guard
// that closes the WS /cd bypass (review P1-2): a workspace-bound session is
// rejected before any worker terminate / transition, regardless of caller
// (REST already guards at api.go; this is the backstop for the WS path).
func TestBridge_SwitchWorkDir_WorkspaceBoundRejected(t *testing.T) {
	t.Parallel()
	sm := new(mockBridgeSM)
	sm.On("Get", "sess-1").Return(&session.SessionInfo{
		ID: "sess-1", State: events.StateRunning, WorkspaceID: "ws-1",
	}, nil)
	b := &Bridge{log: slog.Default(), sm: sm}

	_, err := b.SwitchWorkDir(context.Background(), "sess-1", "/tmp/new")
	require.ErrorIs(t, err, ErrWorkDirImmutable)
	// Guard fires before GetWorker/Terminate/Transition — none should be touched.
	sm.AssertNotCalled(t, "GetWorker")
	sm.AssertNotCalled(t, "Transition")
}

// ─── Test injectSessionStats ─────────────────────────────────────────────────

func TestInjectSessionStats(t *testing.T) {
	log := slog.Default()
	sm := new(mockBridgeSM)
	hub := newTestHub(t)
	b := NewBridge(BridgeDeps{Log: log, Hub: hub, SM: sm})

	acc := b.getOrInitAccum("sess-1", "", time.Now())
	acc.ToolCallCount.Store(4)

	env := &events.Envelope{
		Event: events.Event{
			Type: events.Done,
			Data: events.DoneData{Success: true},
		},
	}

	b.injectSessionStats(env, acc)

	dd, ok := env.Event.Data.(events.DoneData)
	require.True(t, ok)
	require.NotNil(t, dd.Stats)
	_, ok = dd.Stats["_session"]
	assert.True(t, ok)
}

func TestInjectSessionStats_NonDoneData(t *testing.T) {
	log := slog.Default()
	sm := new(mockBridgeSM)
	hub := newTestHub(t)
	b := NewBridge(BridgeDeps{Log: log, Hub: hub, SM: sm})

	acc := b.getOrInitAccum("sess-1", "", time.Now())
	env := &events.Envelope{
		Event: events.Event{
			Type: events.Message,
			Data: "hello",
		},
	}

	// Should be a no-op — no panic, data unchanged.
	b.injectSessionStats(env, acc)
	assert.Equal(t, "hello", env.Event.Data)
}

// ─── PBAC-015: injectAgentConfig BotID Resolution ─────────────────────────────

func writeAgentConfigFile(t *testing.T, dir, name, content string) {
	t.Helper()
	fullPath := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
}

func TestBridge_InjectAgentConfig_BotNameResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(t *testing.T) string // returns config dir
		platform    string
		botID       string
		botName     string
		wantContain string
		wantEmpty   bool
	}{
		{
			name: "bot-level overrides platform",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeAgentConfigFile(t, dir, "webchat/SOUL.md", "Platform soul.")
				writeAgentConfigFile(t, dir, "webchat/my-bot/SOUL.md", "Bot soul.")
				return dir
			},
			platform:    "webchat",
			botName:     "my-bot",
			wantContain: "Bot soul.",
		},
		{
			name: "empty botName skips bot-level",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeAgentConfigFile(t, dir, "webchat/SOUL.md", "Platform soul.")
				writeAgentConfigFile(t, dir, "webchat/orphaned-bot/SOUL.md", "Orphaned soul.")
				return dir
			},
			platform:    "webchat",
			botID:       "orphaned-bot",
			botName:     "",
			wantContain: "Platform soul.",
		},
		{
			name: "empty bot uses platform",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeAgentConfigFile(t, dir, "SOUL.md", "Global soul.")
				writeAgentConfigFile(t, dir, "webchat/SOUL.md", "Platform soul.")
				return dir
			},
			platform:    "webchat",
			botID:       "",
			wantContain: "Platform soul.",
		},
		{
			name: "falls to global",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeAgentConfigFile(t, dir, "SOUL.md", "Global soul.")
				return dir
			},
			platform:    "webchat",
			botID:       "some-bot",
			wantContain: "Global soul.",
		},
		{
			name: "empty external config still injects metacognition",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			platform:    "webchat",
			botID:       "bot",
			wantContain: "<hotplex>",
		},
		{
			name: "path traversal rejected via botName",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeAgentConfigFile(t, dir, "webchat/SOUL.md", "Platform soul.")
				return dir
			},
			platform:  "webchat",
			botID:     "some-bot",
			botName:   "../etc",
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := tt.setup(t)
			hub := newTestHub(t)
			sm := new(mockBridgeSM)
			b := NewBridge(BridgeDeps{
				Log:            slog.Default(),
				Hub:            hub,
				SM:             sm,
				AgentConfigDir: dir,
			})

			info := &worker.SessionInfo{}
			b.injectAgentConfig(info, agentconfig.RuntimeFacts{
				SchemaVersion: agentconfig.RuntimeFactsSchemaVersion,
				Platform:      tt.platform,
				ScopeKind:     agentconfig.RuntimeScopeBot,
				WorkerType:    agentconfig.RuntimeWorkerClaudeCode,
			}, tt.platform, tt.botName, tt.botID, nil, nil)

			if tt.wantEmpty {
				assert.Empty(t, info.SystemPrompt)
			} else {
				assert.Contains(t, info.SystemPrompt, tt.wantContain)
			}
		})
	}
}

// ─── Test injectGatewayContext ────────────────────────────────────────────────

func TestInjectGatewayContext(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		platform    string
		botID       string
		botName     string
		userID      string
		platformKey map[string]string
		sessionID   string
		workDir     string
		want        map[string]string
	}{
		{
			name:     "slack full fields",
			platform: "slack",
			botID:    "B123",
			userID:   "U456",
			platformKey: map[string]string{
				"channel_id": "C789",
				"thread_ts":  "1234.56",
				"team_id":    "T999",
			},
			sessionID: "sess-abc",
			workDir:   "/tmp/work",
			want: map[string]string{
				"GATEWAY_PLATFORM":   "slack",
				"GATEWAY_BOT_ID":     "B123",
				"GATEWAY_USER_ID":    "U456",
				"GATEWAY_CHANNEL_ID": "C789",
				"GATEWAY_THREAD_ID":  "1234.56",
				"GATEWAY_TEAM_ID":    "T999",
				"GATEWAY_SESSION_ID": "sess-abc",
				"GATEWAY_WORK_DIR":   "/tmp/work",
			},
		},
		{
			name:     "feishu maps chat_id to channel_id",
			platform: "feishu",
			botID:    "ou_bot123",
			userID:   "ou_user456",
			platformKey: map[string]string{
				"chat_id":    "oc_chat789",
				"message_id": "om_msg001",
			},
			sessionID: "sess-def",
			workDir:   "/tmp/feishu",
			want: map[string]string{
				"GATEWAY_PLATFORM":   "feishu",
				"GATEWAY_BOT_ID":     "ou_bot123",
				"GATEWAY_USER_ID":    "ou_user456",
				"GATEWAY_CHANNEL_ID": "oc_chat789",
				"GATEWAY_THREAD_ID":  "om_msg001",
				"GATEWAY_SESSION_ID": "sess-def",
				"GATEWAY_WORK_DIR":   "/tmp/feishu",
			},
		},
		{
			name:        "nil env gets initialized",
			env:         nil,
			platform:    "slack",
			botID:       "B1",
			userID:      "U1",
			platformKey: nil,
			sessionID:   "sess-nil",
			workDir:     "/tmp",
			want: map[string]string{
				"GATEWAY_PLATFORM":   "slack",
				"GATEWAY_BOT_ID":     "B1",
				"GATEWAY_USER_ID":    "U1",
				"GATEWAY_SESSION_ID": "sess-nil",
				"GATEWAY_WORK_DIR":   "/tmp",
			},
		},
		{
			name:        "empty fields omitted",
			env:         map[string]string{},
			platform:    "slack",
			botID:       "B1",
			userID:      "U1",
			platformKey: map[string]string{},
			sessionID:   "sess-empty",
			workDir:     "",
			want: map[string]string{
				"GATEWAY_PLATFORM":   "slack",
				"GATEWAY_BOT_ID":     "B1",
				"GATEWAY_USER_ID":    "U1",
				"GATEWAY_SESSION_ID": "sess-empty",
			},
		},
		{
			name:     "preserves existing env",
			env:      map[string]string{"EXISTING": "kept"},
			platform: "slack",
			botID:    "B1",
			userID:   "U1",
			platformKey: map[string]string{
				"channel_id": "C1",
			},
			sessionID: "sess-preserve",
			workDir:   "/tmp",
			want: map[string]string{
				"EXISTING":           "kept",
				"GATEWAY_PLATFORM":   "slack",
				"GATEWAY_BOT_ID":     "B1",
				"GATEWAY_USER_ID":    "U1",
				"GATEWAY_CHANNEL_ID": "C1",
				"GATEWAY_SESSION_ID": "sess-preserve",
				"GATEWAY_WORK_DIR":   "/tmp",
			},
		},
		{
			name:     "channel_id takes priority over chat_id",
			platform: "slack",
			botID:    "B1",
			userID:   "U1",
			platformKey: map[string]string{
				"channel_id": "C_PRIORITY",
				"chat_id":    "oc_lower",
			},
			sessionID: "sess-pri",
			workDir:   "/tmp",
			want: map[string]string{
				"GATEWAY_CHANNEL_ID": "C_PRIORITY",
			},
		},
		{
			name:     "pr_number mapped to TARGET_PR",
			platform: "cron",
			botID:    "B1",
			userID:   "U1",
			platformKey: map[string]string{
				"pr_number": "42",
				"trigger":   "webhook",
			},
			sessionID: "sess-webhook",
			workDir:   "/tmp",
			want: map[string]string{
				"TARGET_PR": "42",
			},
		},
		{
			name:     "botName injected as GATEWAY_BOT_NAME",
			platform: "feishu",
			botID:    "ou_bot123",
			botName:  "my-bot",
			userID:   "U1",
			platformKey: map[string]string{
				"chat_id": "oc_chat",
			},
			sessionID: "sess-botname",
			workDir:   "/tmp",
			want: map[string]string{
				"GATEWAY_BOT_NAME":   "my-bot",
				"GATEWAY_PLATFORM":   "feishu",
				"GATEWAY_BOT_ID":     "ou_bot123",
				"GATEWAY_USER_ID":    "U1",
				"GATEWAY_CHANNEL_ID": "oc_chat",
				"GATEWAY_SESSION_ID": "sess-botname",
				"GATEWAY_WORK_DIR":   "/tmp",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Nil env test: function should initialize the map.
			if tt.env == nil {
				tt.env = injectGatewayContext(tt.env, tt.platform, tt.botID, tt.botName, tt.userID, tt.platformKey, tt.sessionID, tt.workDir)
				require.NotNil(t, tt.env, "env should be initialized")
				for k, v := range tt.want {
					assert.Equal(t, v, tt.env[k], "env[%q]", k)
				}
				return
			}

			tt.env = injectGatewayContext(tt.env, tt.platform, tt.botID, tt.botName, tt.userID, tt.platformKey, tt.sessionID, tt.workDir)

			for k, v := range tt.want {
				assert.Equal(t, v, tt.env[k], "env[%q]", k)
			}
			// Verify omitted fields are absent.
			if _, ok := tt.want["GATEWAY_WORK_DIR"]; !ok {
				_, exists := tt.env["GATEWAY_WORK_DIR"]
				assert.False(t, exists, "GATEWAY_WORK_DIR should not be set")
			}
			if _, ok := tt.want["GATEWAY_TEAM_ID"]; !ok {
				_, exists := tt.env["GATEWAY_TEAM_ID"]
				assert.False(t, exists, "GATEWAY_TEAM_ID should not be set")
			}
			if _, ok := tt.want["GATEWAY_CHANNEL_ID"]; !ok {
				_, exists := tt.env["GATEWAY_CHANNEL_ID"]
				assert.False(t, exists, "GATEWAY_CHANNEL_ID should not be set")
			}
			if _, ok := tt.want["GATEWAY_THREAD_ID"]; !ok {
				_, exists := tt.env["GATEWAY_THREAD_ID"]
				assert.False(t, exists, "GATEWAY_THREAD_ID should not be set")
			}
		})
	}
}

// ─── Test buildWorkerInfo MCP Injection ────────────────────────────────────────

func TestBuildWorkerInfo_MCPInjection(t *testing.T) {
	t.Parallel()

	mcpJSON := `{"mcpServers":{"test":{"command":"echo"}}}`

	tests := []struct {
		name          string
		mcpConfigJSON string
		platform      string
		platformKey   map[string]string
		wantMCP       string
		wantStrict    bool
	}{
		{
			name:          "cron session always suppresses MCP",
			mcpConfigJSON: mcpJSON,
			platform:      "feishu",
			platformKey:   map[string]string{"cron_job_id": "job-1"},
			wantMCP:       `{"mcpServers":{}}`,
			wantStrict:    true,
		},
		{
			name:          "configured MCP with non-cron platform",
			mcpConfigJSON: mcpJSON,
			platform:      "slack",
			platformKey:   nil,
			wantMCP:       mcpJSON,
			wantStrict:    true,
		},
		{
			name:          "empty platform with configured MCP",
			mcpConfigJSON: mcpJSON,
			platform:      "",
			platformKey:   nil,
			wantMCP:       mcpJSON,
			wantStrict:    true,
		},
		{
			name:          "no config and non-cron platform uses default discovery",
			mcpConfigJSON: "",
			platform:      "slack",
			platformKey:   nil,
			wantMCP:       "",
			wantStrict:    false,
		},
		{
			name:          "cron session wins over empty config",
			mcpConfigJSON: "",
			platform:      "feishu",
			platformKey:   map[string]string{"cron_job_id": "job-2"},
			wantMCP:       `{"mcpServers":{}}`,
			wantStrict:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := slog.Default()
			hub := newTestHub(t)
			sm := new(mockBridgeSM)
			b := NewBridge(BridgeDeps{
				Log:           log,
				Hub:           hub,
				SM:            sm,
				MCPConfigJSON: tt.mcpConfigJSON,
			})

			si := &session.SessionInfo{
				Platform:    tt.platform,
				PlatformKey: tt.platformKey,
			}
			info := b.buildWorkerInfo("session-1", "user-1", "/tmp", si)
			assert.Equal(t, tt.wantMCP, info.MCPConfig, "MCPConfig mismatch")
			assert.Equal(t, tt.wantStrict, info.StrictMCPConfig, "StrictMCPConfig mismatch")
		})
	}
}

func TestHandleInternalReset(t *testing.T) {
	t.Parallel()

	gen := int64(5)
	tests := []struct {
		name       string
		data       any
		wantGenSet bool
	}{
		{
			name:       "typed InternalResetData",
			data:       events.InternalResetData{Generation: gen},
			wantGenSet: true,
		},
		{
			name:       "map with int64 generation",
			data:       map[string]any{"generation": int64(5)},
			wantGenSet: true,
		},
		{
			name:       "map with float64 generation",
			data:       map[string]any{"generation": float64(5)},
			wantGenSet: true,
		},
		{
			name:       "map with json.Number generation",
			data:       map[string]any{"generation": json.Number("5")},
			wantGenSet: true,
		},
		{
			name:       "map without generation key",
			data:       map[string]any{"other": "value"},
			wantGenSet: false,
		},
		{
			name:       "map with string generation",
			data:       map[string]any{"generation": "not-a-number"},
			wantGenSet: false,
		},
		{
			name:       "unknown type",
			data:       "invalid",
			wantGenSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := newTestHub(t)
			b := &Bridge{
				log:   slog.Default(),
				sm:    new(mockBridgeSM),
				hub:   hub,
				accum: make(map[string]*sessionAccumulator),
			}

			env := &events.Envelope{
				Event: events.Event{
					Type: events.KindInternalReset,
					Data: tt.data,
				},
			}

			fc := &forwardContext{
				sessionID: "sess-test",
			}

			acc := b.getOrInitAccum("sess-test", "", time.Now())
			acc.Generation.Store(10)

			b.handleInternalReset(env, "sess-test", fc)

			if tt.wantGenSet {
				assert.Equal(t, int32(0), acc.TurnCount.Load())
				assert.Equal(t, int64(11), acc.Generation.Load())
			} else {
				assert.Equal(t, int32(0), acc.TurnCount.Load())
				assert.Equal(t, int64(10), acc.Generation.Load())
			}
		})
	}
}

// ─── Test ResetSession Reloads Agent Config ──────────────────────────────────

// mockPromptUpdater is a mockBridgeWorker that also implements SystemPromptUpdater.
type mockPromptUpdater struct {
	mockBridgeWorker
	updatedPrompt string
}

func (m *mockPromptUpdater) UpdateSystemPrompt(prompt string) {
	m.updatedPrompt = prompt
}

var _ worker.SystemPromptUpdater = (*mockPromptUpdater)(nil)

type blockingResetWorker struct {
	mockBridgeWorker
	started chan struct{}
	release chan struct{}
	result  worker.ResetResult
}

func (w *blockingResetWorker) ResetContext(context.Context) (worker.ResetResult, error) {
	close(w.started)
	<-w.release
	return w.result, nil
}

func TestResetSession_SuspendsRunBindingDuringConnectionReset(t *testing.T) {
	t.Parallel()
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	w := &blockingResetWorker{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	sid := "session-reset-binding"
	sm.On("GetWorker", sid).Return(w)
	sm.On("Get", sid).Return(&session.SessionInfo{ID: sid, Platform: "webchat"}, nil)
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: hub, SM: sm})
	b.workerRuns.Store(sid, workerRunBinding{worker: w, id: "run-before-reset"})

	done := make(chan error, 1)
	go func() { done <- b.ResetSession(context.Background(), sid) }()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("reset did not start")
	}
	_, ok := b.CurrentWorkerRunID(sid)
	require.False(t, ok, "input must fail closed while ResetContext may replace the connection")
	close(w.release)
	require.NoError(t, <-done)
	runID, ok := b.CurrentWorkerRunID(sid)
	require.True(t, ok)
	require.Equal(t, "run-before-reset", runID, "in-place reset must restore the original run binding")
}

func TestResetSession_ClearsPendingSupplements(t *testing.T) {
	t.Parallel()
	sid := "session-reset-pending"
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	w := &mockBridgeWorker{}
	sm.On("GetWorker", sid).Return(w)
	sm.On("Get", sid).Return(&session.SessionInfo{ID: sid, Platform: "webchat"}, nil)
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: hub, SM: sm})
	b.pending.Append(sid, "旧上下文追问", newInputEnvelope(t, sid, "旧上下文追问"))

	require.NoError(t, b.ResetSession(context.Background(), sid))
	_, _, ok := b.pending.DrainAndMerge(sid)
	require.False(t, ok, "reset must discard supplements from the old context")
}

// recordingQueueDispatcher records what the Bridge asks of the durable queue.
type recordingQueueDispatcher struct {
	mu         sync.Mutex
	cleared    []string
	dispatched []string
	clearErr   error
}

func (d *recordingQueueDispatcher) DispatchQueued(_ context.Context, sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dispatched = append(d.dispatched, sessionID)
}

func (d *recordingQueueDispatcher) ClearSessionQueue(
	_ context.Context, sessionID string,
) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cleared = append(d.cleared, sessionID)
	return 1, d.clearErr
}

func (d *recordingQueueDispatcher) clearedSessions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.cleared...)
}

// A reset establishes a NEW conversation context under the same session id.
// An input still sitting undispatched belongs to the abandoned context and must
// never be dispatched into the new one.
//
// This is asserted through ResetSession rather than against ClearSessionQueue
// directly: the unit tests of the queue all invoke the store method themselves,
// so nothing else would notice if the reset path stopped calling it at all.
func TestResetSession_SettlesTheUndispatchedQueue(t *testing.T) {
	t.Parallel()

	sid := "session-reset-queue"
	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	w := &mockBridgeWorker{}
	sm.On("GetWorker", sid).Return(w)
	sm.On("Get", sid).Return(&session.SessionInfo{ID: sid, Platform: "webchat"}, nil)
	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: hub, SM: sm})

	dispatcher := &recordingQueueDispatcher{}
	b.SetQueueDispatcher(dispatcher)

	require.NoError(t, b.ResetSession(context.Background(), sid))
	require.Equal(t, []string{sid}, dispatcher.clearedSessions(),
		"reset must settle the undispatched queue, or an old input dispatches into the new context")
}

func TestResetSession_ReloadsAgentConfig(t *testing.T) {
	t.Parallel()

	// Set up agent config dir with a SOUL.md
	dir := t.TempDir()
	writeAgentConfigFile(t, dir, "SOUL.md", "Updated persona v2.")

	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	b := NewBridge(BridgeDeps{
		Log:            slog.Default(),
		Hub:            hub,
		SM:             sm,
		AgentConfigDir: dir,
	})

	sid := "test-reset-reload-session"
	mw := &mockPromptUpdater{mockBridgeWorker: mockBridgeWorker{workerType: worker.TypeClaudeCode}}

	sm.On("GetWorker", sid).Return(mw)
	sm.On("Get", sid).Return(&session.SessionInfo{
		ID:                sid,
		UserID:            "user-1",
		Platform:          "webchat",
		BotID:             "bot-1",
		PermissionCeiling: "read-only",
		WorkDir:           "/private/work",
		PlatformKey:       map[string]string{"channel_id": "chat-private"},
	}, nil)

	err := b.ResetSession(context.Background(), sid)
	require.NoError(t, err)

	assert.Contains(t, mw.updatedPrompt, "Updated persona v2.")
	assert.Contains(t, mw.updatedPrompt, `<runtime-facts format="application/json" schema-version="1">`)
	assert.Contains(t, mw.updatedPrompt, `"platform":"webchat"`)
	assert.Contains(t, mw.updatedPrompt, `"worker_type":"claude_code"`)
	assert.Contains(t, mw.updatedPrompt, `"scope_kind":"bot"`)
	assert.Contains(t, mw.updatedPrompt, `"declared_permission_mode":"read-only"`)
	assert.NotContains(t, mw.updatedPrompt, "user-1")
	assert.NotContains(t, mw.updatedPrompt, "bot-1")
	assert.NotContains(t, mw.updatedPrompt, "private/work")
	assert.NotContains(t, mw.updatedPrompt, "chat-private")
	sm.AssertExpectations(t)
}

func TestResetSession_WebChatWorkspaceOverrides(t *testing.T) {
	t.Parallel()

	// Team default on disk; workspace override must win after reset (spec ②).
	// Regression guard: ResetSession must pass resolveWorkspaceOverrides(si.WorkspaceID),
	// not nil — otherwise WebChat /reset silently drops workspace config and falls back
	// to team defaults.
	dir := t.TempDir()
	writeAgentConfigFile(t, dir, "SOUL.md", "team-soul")

	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	ws := &session.Workspace{ID: "ws-reset", AgentConfigOverrides: `{"SOUL.md":"ws-soul"}`}
	b := NewBridge(BridgeDeps{
		Log:            slog.Default(),
		Hub:            hub,
		SM:             sm,
		AgentConfigDir: dir,
		WSStore:        &stubWSStore{ws: ws},
	})

	sid := "test-reset-webchat-overrides"
	mw := &mockPromptUpdater{}

	sm.On("GetWorker", sid).Return(mw)
	sm.On("Get", sid).Return(&session.SessionInfo{
		ID:          sid,
		Platform:    "webchat",
		WorkspaceID: "ws-reset",
	}, nil)

	err := b.ResetSession(context.Background(), sid)
	require.NoError(t, err)

	// Workspace override wins over team default — proves ResetSession resolves
	// overrides for WebChat sessions, not hardcoded nil.
	assert.Contains(t, mw.updatedPrompt, "ws-soul")
	assert.NotContains(t, mw.updatedPrompt, "team-soul")
	sm.AssertExpectations(t)
}

func TestResetSession_NoUpdater_NoReload(t *testing.T) {
	t.Parallel()

	// Set up agent config dir so we can verify it's NOT used when worker lacks SystemPromptUpdater.
	dir := t.TempDir()
	writeAgentConfigFile(t, dir, "SOUL.md", "Should not appear.")

	hub := newTestHub(t)
	sm := new(mockBridgeSM)
	b := NewBridge(BridgeDeps{
		Log:            slog.Default(),
		Hub:            hub,
		SM:             sm,
		AgentConfigDir: dir,
	})

	sid := "test-no-updater"
	mw := &mockBridgeWorker{} // does NOT implement SystemPromptUpdater

	sm.On("GetWorker", sid).Return(mw)
	sm.On("Get", sid).Return(&session.SessionInfo{
		ID:       sid,
		Platform: "webchat",
	}, nil)

	err := b.ResetSession(context.Background(), sid)
	require.NoError(t, err)
	// No crash, no panic — worker without SystemPromptUpdater is silently skipped.
	sm.AssertExpectations(t)
}

// ─── Tests for the worker-binding guard in handleWorkerExit ──────────────────

// TestBridge_HandleWorkerExit_StaleForwarderAfterRecreate verifies that when a
// session is deleted and re-created with a new worker (same deterministic ID),
// the old forwarder's handleWorkerExit returns early without producing side
// effects (error events, synthetic events, worker cleanup) that would otherwise
// use the new session's seqGen and cause UNIQUE constraint violations on the
// events table.
func TestBridge_HandleWorkerExit_StaleForwarderAfterRecreate(t *testing.T) {
	t.Parallel()

	const sessionID = "sess_recreated"

	oldWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		exitCode:   1,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope, 1)},
	}
	oldWorker.stopped.Store(false)

	newWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}

	sm := new(mockBridgeSM)
	sm.Test(t)

	// Session exists (re-created) and is running, not terminated.
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, State: events.StateRunning,
	}, nil)
	// GetWorker returns the NEW worker, not the old crashed one.
	sm.On("GetWorker", sessionID).Return(newWorker)

	h := newTestHub(t)
	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: h, SM: sm})

	params := workerExitParams{
		sessionID:    sessionID,
		workerType:   worker.TypeClaudeCode,
		doneReceived: false,
		startTime:    time.Now(),
		flog:         slog.Default(),
		opts:         forwardOpts{retryDepth: 2}, // Skip fallback for cleaner test
	}

	// Must not panic, and must skip sendError/captureSyntheticEvent/cleanup.
	b.handleWorkerExit(oldWorker, params)

	sm.AssertExpectations(t)

	// Verify no accumulator was created for the stale forwarder.
	b.accumMu.RLock()
	_, hasAccum := b.accum[sessionID]
	b.accumMu.RUnlock()
	assert.False(t, hasAccum, "stale forwarder must not create accumulator")
}

// TestBridge_HandleWorkerExit_StaleForwarderAfterDelete verifies that when a
// session has been physically deleted (no re-creation), the old forwarder's
// handleWorkerExit returns early without side effects.
func TestBridge_HandleWorkerExit_StaleForwarderAfterDelete(t *testing.T) {
	t.Parallel()

	const sessionID = "sess_deleted"

	oldWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		exitCode:   1,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope, 1)},
	}
	oldWorker.stopped.Store(false)

	sm := new(mockBridgeSM)
	sm.Test(t)

	// Session was deleted — Get returns error.
	sm.On("Get", sessionID).Return(nil, session.ErrSessionNotFound)
	// GetWorker returns nil because session no longer exists in the map.
	sm.On("GetWorker", sessionID).Return(nil)

	h := newTestHub(t)
	b := NewBridge(BridgeDeps{Log: slog.Default(), Hub: h, SM: sm})

	params := workerExitParams{
		sessionID:    sessionID,
		workerType:   worker.TypeClaudeCode,
		doneReceived: false,
		startTime:    time.Now(),
		flog:         slog.Default(),
		opts:         forwardOpts{retryDepth: 2},
	}

	// Must not panic, and must skip error/capture/cleanup.
	b.handleWorkerExit(oldWorker, params)

	sm.AssertExpectations(t)
}

// TestBridge_ResumeSession_HydratesSeqFromEventstore verifies that resuming a
// platform (messaging) session hydrates SeqGen from the eventstore before the
// first NextSeq. Platform sessions reach resume via StartPlatformSession →
// orphan resume, never traversing the WebSocket performInit path that hydrates
// in conn.go. Without this hydrate, a session whose counter was released
// (RuntimeRelease → ReleaseSeq) restarts near 0 and collides with durable
// history, causing recurring UNIQUE constraint failures on
// events(session_id, seq_guard_id, seq) — the issue #879 regression that
// survived #900 and 0231fc74 because both only hardened the WS path.
func TestBridge_ResumeSession_HydratesSeqFromEventstore(t *testing.T) {
	t.Parallel()
	const sessionID = "sess-resume-hydrate"

	// Persisted history claims 9899 events; resume must seed the counter at
	// 9899 so the next seq is 9900, not 1.
	hub := newTestHub(t)
	hub.SetSeqHydrator(&mockSeqHydrator{seq: 9899})
	require.False(t, hub.seqGen.IsHydrated(sessionID), "counter must not be hydrated before resume")

	resumedWorker := &mockBridgeWorker{
		workerType: worker.TypeClaudeCode,
		conn:       &fakeWorkerConn{ch: make(chan *events.Envelope)},
	}
	sm := new(mockBridgeSM)
	sm.On("Get", sessionID).Return(&session.SessionInfo{
		ID: sessionID, UserID: "u1", WorkerType: worker.TypeClaudeCode,
		State: events.StateTerminated, // orphan terminated → resume (bridge.go:758)
	}, nil).Once()
	sm.On("GetWorker", sessionID).Return(nil).Once()
	sm.On("Transition", mock.Anything, sessionID, events.StateRunning).Return(nil).Once()
	sm.On("AttachWorker", sessionID, mock.Anything).Return(nil).Once()
	sm.On("ResetExpiry", mock.Anything, sessionID).Return(nil).Once()
	// forwardEvents tears down via crash cleanup when the fake conn closes;
	// report the worker as already-replaced so cleanup short-circuits.
	sm.On("DetachWorkerIf", sessionID, mock.Anything).Return(false).Maybe()

	b := NewBridge(BridgeDeps{Log: testLogger(t), Hub: hub, SM: sm})
	b.SetWorkerFactory(&mockBridgeWorkerFactory{workers: []*mockBridgeWorker{resumedWorker}})

	err := b.ResumeSession(context.Background(), sessionID, "")
	require.NoError(t, err)

	// Core regression assertion: resume hydrated SeqGen from the eventstore, so
	// the next seq continues monotonically (9900+) instead of restarting near 0
	// and colliding with persisted history.
	require.True(t, hub.seqGen.IsHydrated(sessionID), "resume must hydrate SeqGen from eventstore")
	require.GreaterOrEqual(t, hub.NextSeqPeek(sessionID), int64(9899),
		"resume must seed counter at persisted max seq; without hydrate it restarts at 0 and collides (issue #879)")

	sm.AssertExpectations(t)

	// Tear down the forwardEvents goroutine spawned by createAndLaunchWorker.
	b.closed.Store(true)
	close(resumedWorker.conn.ch)
	b.WaitForwarders(context.Background())
}
