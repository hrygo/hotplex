package cron

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

// mockBridge implements BridgeStarter for testing.
type mockBridge struct {
	startErr   error
	lastStart  worker.SessionStartParams // captures params from the most recent StartSession call
	startCount int                       // number of StartSession calls, to prove a duplicate trigger ran once
}

func (m *mockBridge) StartSession(_ context.Context, p worker.SessionStartParams) error {
	m.lastStart = p
	m.startCount++
	return m.startErr
}

// mockSessionStateChecker implements SessionStateChecker for testing.
type mockSessionStateChecker struct {
	sessions       map[string]*session.SessionInfo
	workers        map[string]worker.Worker
	defaultWorker  worker.Worker
	defaultSession *session.SessionInfo
}

func (m *mockSessionStateChecker) Get(_ context.Context, id string) (*session.SessionInfo, error) {
	if si, ok := m.sessions[id]; ok {
		return si, nil
	}
	if m.defaultSession != nil {
		return m.defaultSession, nil
	}
	return nil, errTestNotFound
}

func (m *mockSessionStateChecker) GetWorker(id string) worker.Worker {
	if w, ok := m.workers[id]; ok {
		return w
	}
	return m.defaultWorker
}

func (m *mockSessionStateChecker) Transition(_ context.Context, _ string, _ events.SessionState) error {
	return nil
}

// mockWorker implements worker.Worker for testing with minimal stubs.
type mockWorker struct {
	inputErr  error
	lastInput string // captures the prompt sent via Input
}

func (m *mockWorker) Type() worker.WorkerType                             { return worker.TypeClaudeCode }
func (m *mockWorker) SupportsResume() bool                                { return false }
func (m *mockWorker) CanResumeTerminated() bool                           { return false }
func (m *mockWorker) SupportsStreaming() bool                             { return true }
func (m *mockWorker) SupportsTools() bool                                 { return true }
func (m *mockWorker) EnvBlocklist() []string                              { return nil }
func (m *mockWorker) SessionStoreDir() string                             { return "" }
func (m *mockWorker) MaxTurns() int                                       { return 0 }
func (m *mockWorker) Modalities() []string                                { return []string{"text"} }
func (m *mockWorker) Start(_ context.Context, _ worker.SessionInfo) error { return nil }
func (m *mockWorker) Input(_ context.Context, prompt string, _ map[string]any) error {
	m.lastInput = prompt
	return m.inputErr
}
func (m *mockWorker) Resume(_ context.Context, _ worker.SessionInfo) error { return nil }
func (m *mockWorker) Terminate(_ context.Context) error                    { return nil }
func (m *mockWorker) Kill() error                                          { return nil }
func (m *mockWorker) Wait() (int, error)                                   { return 0, nil }
func (m *mockWorker) Conn() worker.SessionConn                             { return nil }
func (m *mockWorker) Health() worker.WorkerHealth                          { return worker.WorkerHealth{} }
func (m *mockWorker) LastIO() time.Time                                    { return time.Time{} }
func (m *mockWorker) ResetContext(_ context.Context) (worker.ResetResult, error) {
	return worker.ResetResult{}, nil
}
func (m *mockWorker) StopCurrentTurn(_ context.Context) error { return nil }
func (m *mockWorker) IsStopped() bool                         { return false }

var errTestNotFound = context.DeadlineExceeded

func testJob() *CronJob {
	return &CronJob{
		ID:      "cron_test",
		Name:    "test",
		OwnerID: "user1",
		BotID:   "bot1",
		WorkDir: "/tmp",
		Payload: CronPayload{Kind: PayloadIsolatedSession, Message: "hello"},
	}
}

// testTrigger returns a stable manual trigger identity for executor tests.
func testTrigger() TriggerIdentity {
	return TriggerIdentity{Kind: TriggerManual, JobID: "cron_test", Nonce: "nonce-1"}
}

// newTestExecutor builds an executor with no occurrence store, for tests that
// exercise dispatch behavior rather than occurrence persistence.
func newTestExecutor(bridge BridgeStarter, sm SessionStateChecker) *Executor {
	return NewExecutor(slog.Default(), bridge, sm, "", nil)
}

func TestExecutor_Execute_StartFails(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{startErr: errTestNotFound}
	sm := &mockSessionStateChecker{workers: map[string]worker.Worker{}}

	e := newTestExecutor(bridge, sm)
	_, err := e.Execute(context.Background(), testJob(), testTrigger(), 5*time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "start cron session")
}

func TestExecutor_Execute_WorkerNotFound(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{workers: map[string]worker.Worker{}}

	e := newTestExecutor(bridge, sm)
	_, err := e.Execute(context.Background(), testJob(), testTrigger(), 5*time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "worker not found")
}

func TestExecutor_Execute_InputFails(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultWorker: &mockWorker{inputErr: errTestNotFound},
	}

	e := newTestExecutor(bridge, sm)
	_, err := e.Execute(context.Background(), testJob(), testTrigger(), 5*time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input prompt")
}

func TestExecutor_Execute_TimeoutWaiting(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "running"},
		defaultWorker:  &mockWorker{},
	}

	e := newTestExecutor(bridge, sm)
	_, err := e.Execute(context.Background(), testJob(), testTrigger(), 100*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout")
}

func TestExecutor_Execute_Success(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	// Session already completed (terminated) — waitForCompletion returns immediately.
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}

	e := newTestExecutor(bridge, sm)

	gotKey, err := e.Execute(context.Background(), testJob(), testTrigger(), 5*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, gotKey)
}

// TestExecutor_Execute_NormalizesEmptyPlatformToCron verifies that a pure
// scheduled task (no messaging delivery target) records the "cron" platform on
// the session it starts, so the session.create audit row is populated instead
// of carrying an empty platform. See CronJob.SessionKey, which already derives
// under "cron".
func TestExecutor_Execute_NormalizesEmptyPlatformToCron(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newTestExecutor(bridge, sm)

	job := testJob() // Platform is empty
	_, err := e.Execute(context.Background(), job, testTrigger(), 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "cron", bridge.lastStart.Platform,
		"empty-platform cron job must be normalized to the cron platform for audit/metrics")
}

// TestExecutor_Execute_PreservesDeliveryPlatform verifies that a job with a
// messaging delivery target keeps its real platform (needed for agent-config
// fallback resolution) and is NOT clobbered by the empty-platform normalization.
func TestExecutor_Execute_PreservesDeliveryPlatform(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  &mockWorker{},
	}
	e := newTestExecutor(bridge, sm)

	job := testJob()
	job.Platform = "feishu"
	job.PlatformKey = map[string]string{"chat_id": "oc_123"}
	_, err := e.Execute(context.Background(), job, testTrigger(), 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "feishu", bridge.lastStart.Platform)
}

func TestBuildDeliverySuffix_SilentJob(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.Silent = true
	job.Platform = "feishu"
	job.PlatformKey = map[string]string{"chat_id": "oc_123"}
	require.Empty(t, buildDeliverySuffix(job))
}

func TestBuildDeliverySuffix_NonSilentWithPlatform(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.Platform = "feishu"
	job.PlatformKey = map[string]string{"chat_id": "oc_123"}
	require.NotEmpty(t, buildDeliverySuffix(job))
}

func TestBuildWebhookPrefix_WebhookTrigger(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.PlatformKey = map[string]string{"trigger": "webhook", "pr_number": "642"}

	prefix := buildWebhookPrefix(job)
	require.Contains(t, prefix, "PR #642")
	require.Contains(t, prefix, "TARGET_PR=642")
	require.Contains(t, prefix, "WEBHOOK")
	require.Contains(t, prefix, "仅审查")
	require.Contains(t, prefix, "不要枚举")
}

func TestBuildWebhookPrefix_CronTrigger(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.PlatformKey = nil

	require.Empty(t, buildWebhookPrefix(job))
}

func TestBuildWebhookPrefix_WebhookWithoutPrNumber(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.PlatformKey = map[string]string{"trigger": "webhook"}

	require.Empty(t, buildWebhookPrefix(job))
}

func TestBuildWebhookPrefix_PrNumberWithoutTrigger(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.PlatformKey = map[string]string{"pr_number": "642"}

	require.Empty(t, buildWebhookPrefix(job))
}

func TestBuildWebhookPrefix_NonNumericPrNumber(t *testing.T) {
	t.Parallel()

	job := testJob()
	job.PlatformKey = map[string]string{"trigger": "webhook", "pr_number": "642; rm -rf /"}

	require.Empty(t, buildWebhookPrefix(job))
}

func TestExecutor_Execute_WebhookPromptContainsPrefix(t *testing.T) {
	t.Parallel()

	bridge := &mockBridge{}
	mw := &mockWorker{}
	sm := &mockSessionStateChecker{
		defaultSession: &session.SessionInfo{State: "terminated"},
		defaultWorker:  mw,
	}

	e := newTestExecutor(bridge, sm)

	job := testJob()
	job.PlatformKey = map[string]string{"trigger": "webhook", "pr_number": "642"}

	_, err := e.Execute(context.Background(), job, testTrigger(), 5*time.Second)
	require.NoError(t, err)
	require.Contains(t, mw.lastInput, "WEBHOOK")
	require.Contains(t, mw.lastInput, "PR #642")
	require.Contains(t, mw.lastInput, "TARGET_PR=642")
}
