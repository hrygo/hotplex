// Package opencodeserver implements the OpenCode Server worker adapter.
//
// OpenCode Server runs as a persistent HTTP server process (opencode serve) that
// manages multiple sessions. Unlike CLI-based workers that use stdio, this adapter
// communicates via HTTP REST API for commands and Server-Sent Events (SSE) for
// streaming responses.
//
// # Architecture
//
//	Gateway (main process)
//	    ↓ creates Worker instances (one per session)
//	OpenCode Server Worker (this adapter, thin session adapter)
//	    ↓ shares SingletonProcessManager (one process for all sessions)
//	OpenCode Server Process (independent HTTP server, lazy-started)
//	    ↕ HTTP REST API + SSE
//	Worker ↔ Server communication
//
// # Key Features
//
//   - Resume Support: Can reconnect to existing server sessions
//   - Multi-session: Shared singleton server process handles all sessions
//   - SSE Streaming: Real-time event stream via text/event-stream
//   - Process Isolation: PGID-based process group for clean termination
//   - Lazy Startup: Server process starts on first session, stops after idle drain
//
// # Protocol
//
// # AEP v1 (Agent Event Protocol) over NDJSON
//
// See docs/specs/Worker-OpenCode-Server-Spec.md for full specification.
package opencodeserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// Compile-time interface compliance checks.
var (
	_ worker.Worker               = (*Worker)(nil)
	_ worker.SessionConn          = (*conn)(nil)
	_ worker.ControlRequester     = (*Worker)(nil)
	_ worker.WorkerCommander      = (*Worker)(nil)
	_ worker.SkillInvoker         = (*Worker)(nil)
	_ worker.SkillCatalogProvider = (*Worker)(nil)
	_ worker.SystemPromptUpdater  = (*Worker)(nil)
)

// Env blocklist for OpenCode Server worker.
// All os.Environ() vars are passed through by default, except those listed here.
var openCodeSrvEnvBlocklist = []string{
	// Nested agent detection.
	"CLAUDECODE",
	// Gateway-internal secrets (prefix match).
	"HOTPLEX_",
	// Claude Code specific vars — not relevant for OCS worker.
	"CLAUDE_",
	"ANTHROPIC_",
}

const (
	// recvChannelSize is the buffer size for SSE event channel.
	recvChannelSize = 256

	// httpClientTimeout is the timeout for general HTTP client operations.
	httpClientTimeout = 30 * time.Second

	// sendTimeout bounds acknowledgement of an asynchronous OCS prompt. The
	// model turn itself continues over SSE and is not covered by this timeout.
	sendTimeout = httpClientTimeout
)

// Worker implements the OpenCode Server worker adapter.
//
// Each Worker instance is a thin session adapter that does NOT own a process.
// All Workers share a SingletonProcessManager that manages one `opencode serve`
// process lazily started on first use.
//
// # Lifecycle
//
//  1. Start() acquires a ref from singleton, creates HTTP session, starts SSE reader
//  2. Input() sends user messages via HTTP POST
//  3. Terminate()/Kill() releases the ref and closes the SSE connection (not the process)
//  4. Wait() reports exit code based on crash notification from singleton
//
// # Concurrency Model
//
//   - Single owner: Worker is owned by one session.Manager
//   - Thread-safe: All public methods are safe for concurrent use
//   - Goroutines: forwardBusEvents runs in a separate goroutine, receives from EventBus channel
//   - Backpressure: recvCh has 256 buffer, drops messages when full
type Worker struct {
	*base.BaseWorker

	singleton *SingletonProcessManager
	httpConn  *conn
	httpAddr  string
	client    *http.Client
	cmd       *ServerCommander
	crashSub  <-chan struct{} // closed when singleton process crashes

	// sseCancel is used to cancel the SSE request context on Terminate/Kill.
	sseCancel context.CancelFunc

	// releaseOnce ensures singleton.Release() is called exactly once,
	// regardless of which method triggers it (Terminate, Kill, or Wait).
	releaseOnce sync.Once

	// workerSessionID atomically stores the worker-internal session ID.
	workerSessionID atomic.Value // string

	// permissionCeiling is immutable for this Worker session and survives
	// in-place reset/clear operations.
	permissionCeiling worker.PermissionCeiling

	// nativeDispatchAccepted bridges the blocking /command response with the
	// earlier session.status(busy) acceptance signal from the SSE event stream.
	nativeDispatchMu       sync.Mutex
	nativeDispatchGen      uint64
	nativeDispatchAccepted func()
}

var (
	_ worker.WorkerSessionIDHandler            = (*Worker)(nil)
	_ worker.PermissionCeilingReporter         = (*Worker)(nil)
	_ worker.NativeCommandDispatchAcknowledger = (*Worker)(nil)
	_ base.MetadataHandler                     = (*Worker)(nil)
	_ base.MultiAnswerQuestionResponseHandler  = (*Worker)(nil)
)

func (w *Worker) GetWorkerSessionID() string {
	w.Mu.Lock()
	conn := w.httpConn
	w.Mu.Unlock()
	if conn != nil {
		conn.mu.Lock()
		sid := conn.sessionID
		conn.mu.Unlock()
		return sid
	}
	if v := w.workerSessionID.Load(); v != nil {
		if sid, ok := v.(string); ok {
			return sid
		}
	}
	return ""
}

func (w *Worker) SetWorkerSessionID(id string) {
	w.workerSessionID.Store(id)
	w.Mu.Lock()
	conn := w.httpConn
	w.Mu.Unlock()
	if conn != nil {
		conn.mu.Lock()
		conn.sessionID = id
		conn.mu.Unlock()
	}
}

// New creates a new OpenCode Server worker instance.
func New() *Worker {
	return &Worker{
		BaseWorker: base.NewBaseWorker(slog.Default(), nil),
		singleton:  singleton.Load(),
		client:     newHTTPClient(),
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpClientTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// newSendClient creates the HTTP client used for asynchronous prompt delivery.
// The response is only an acceptance acknowledgement; turn output uses SSE.
func newSendClient() *http.Client {
	return &http.Client{
		Timeout: sendTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// ─── Capabilities ─────────────────────────────────────────────────────────────

func (w *Worker) Type() worker.WorkerType   { return worker.TypeOpenCodeSrv }
func (w *Worker) SupportsResume() bool      { return true }
func (w *Worker) CanResumeTerminated() bool { return true }
func (w *Worker) SupportsStreaming() bool   { return true }
func (w *Worker) SupportsTools() bool       { return true }
func (w *Worker) EnvBlocklist() []string    { return openCodeSrvEnvBlocklist }
func (w *Worker) SessionStoreDir() string   { return "" }

// MaxTurns returns 0 (unlimited).
// B3-1 note: OCS does not support dynamic per-session steps limits.
// The underlying OpenCode agent may have a predefined steps config,
// but it cannot be set via the HTTP session API.
func (w *Worker) MaxTurns() int        { return 0 }
func (w *Worker) Modalities() []string { return []string{"text", "code", "image"} }

func (w *Worker) PermissionCeiling() (string, bool) {
	return w.permissionCeiling.Mode()
}

// ─── Worker Lifecycle ─────────────────────────────────────────────────────────

// Start acquires the singleton server, creates a new HTTP session, and starts
// the SSE reader goroutine.
func (w *Worker) Start(ctx context.Context, session worker.SessionInfo) error {
	if err := w.checkNotStarted(); err != nil {
		return err
	}

	// Acquire ref from singleton (starts process if first session)
	w.Log.Debug("opencodeserver: start step 1 - acquiring server")
	if err := w.acquireServer(ctx); err != nil {
		return err
	}
	w.Log.Debug("opencodeserver: start step 2 - server acquired", "addr", w.httpAddr)

	// Create new session via HTTP API
	w.Log.Debug("opencodeserver: start step 3 - creating session", "dir", session.ProjectDir)
	sessionID, err := w.createSession(ctx, session.ProjectDir)
	if err != nil {
		w.releaseOnce.Do(func() { w.singleton.Release() })
		return fmt.Errorf("opencodeserver: create session: %w", err)
	}
	w.Log.Debug("opencodeserver: start step 4 - session created", "ocs_session_id", sessionID)

	if err := w.initSessionConn(ctx, sessionID, session); err != nil {
		w.releaseOnce.Do(func() { w.singleton.Release() })
		return fmt.Errorf("opencodeserver: initialize session: %w", err)
	}
	// Persist OCS session ID immediately so resume can find it even if
	// release() races with the persistWorkerSessionID in bridge.
	w.SetWorkerSessionID(sessionID)
	w.startSSE(sessionID)
	w.Log.Debug("opencodeserver: start completed")
	return nil
}

// Input sends a user message to the OpenCode server.
func (w *Worker) Input(ctx context.Context, content string, metadata map[string]any) error {
	w.Mu.Lock()
	conn := w.httpConn
	w.Mu.Unlock()

	if conn == nil {
		return fmt.Errorf("opencodeserver: worker not started")
	}

	handled, err := base.DispatchMetadata(ctx, metadata, w)
	if err != nil {
		return err
	}
	if handled {
		w.SetLastIO(time.Now())
		return nil
	}

	// A new primary turn begins here. Clear the stopped marker before dispatch;
	// prompt_async returns once the server accepts the turn while output and the
	// terminal continue over SSE. Restore the previous marker if delivery fails.
	wasStopped := w.IsStopped()
	w.BeginTurn()

	msg := events.NewEnvelope(
		aep.NewID(),
		conn.getSessionID(),
		0,
		events.Input,
		events.InputData{
			Content:  content,
			Metadata: metadata,
		},
	)

	if err := conn.Send(ctx, msg); err != nil {
		if wasStopped {
			w.MarkStopped()
		}
		return fmt.Errorf("opencodeserver: send input: %w", err)
	}

	w.SetLastIO(time.Now())
	return nil
}

// InvokeSkill sends a resolved Skill through OpenCode's native command API.
// It intentionally does not route through conn.Send, whose contract is the
// ordinary /prompt_async input path.
func (w *Worker) InvokeSkill(ctx context.Context, invocation worker.SkillInvocation) error {
	return w.invokeSkill(ctx, invocation, nil)
}

func (w *Worker) InvokeNativeCommandWithDispatchAccepted(
	ctx context.Context,
	invocation worker.NativeCommandInvocation,
	accepted func(),
) error {
	return w.invokeSkill(ctx, worker.SkillInvocation(invocation), accepted)
}

func (w *Worker) invokeSkill(ctx context.Context, invocation worker.SkillInvocation, accepted func()) error {
	w.Mu.Lock()
	commander := w.cmd
	conn := w.httpConn
	w.Mu.Unlock()
	if commander == nil {
		return fmt.Errorf("opencodeserver: worker not started")
	}

	wasStopped := w.IsStopped()
	w.BeginTurn()
	if conn != nil {
		conn.setSkillReplay(worker.NativeInvocationFromSkill(invocation))
	}
	dispatchGen, err := w.armNativeDispatchAccepted(accepted)
	if err != nil {
		if wasStopped {
			w.MarkStopped()
		}
		return err
	}
	defer w.disarmNativeDispatchAccepted(dispatchGen)
	if err := commander.InvokeSkill(ctx, invocation); err != nil {
		if wasStopped {
			w.MarkStopped()
		}
		return err
	}
	w.SetLastIO(time.Now())
	return nil
}

func (w *Worker) armNativeDispatchAccepted(accepted func()) (uint64, error) {
	if accepted == nil {
		return 0, nil
	}
	w.nativeDispatchMu.Lock()
	defer w.nativeDispatchMu.Unlock()
	if w.nativeDispatchAccepted != nil {
		return 0, fmt.Errorf("opencodeserver: native command dispatch already pending")
	}
	w.nativeDispatchGen++
	w.nativeDispatchAccepted = accepted
	return w.nativeDispatchGen, nil
}

func (w *Worker) disarmNativeDispatchAccepted(generation uint64) {
	if generation == 0 {
		return
	}
	w.nativeDispatchMu.Lock()
	defer w.nativeDispatchMu.Unlock()
	if w.nativeDispatchGen == generation {
		w.nativeDispatchAccepted = nil
	}
}

func (w *Worker) acceptNativeDispatch() {
	w.nativeDispatchMu.Lock()
	accepted := w.nativeDispatchAccepted
	w.nativeDispatchAccepted = nil
	w.nativeDispatchMu.Unlock()
	if accepted != nil {
		accepted()
	}
}

// ListInvokableSkills delegates the OpenCode command catalog query to the
// ServerCommander. The commander is snapshotted under w.Mu like InvokeSkill; a
// nil commander means the worker is not started and no catalog can be
// confirmed.
func (w *Worker) ListInvokableSkills(ctx context.Context, workDir string) ([]worker.SkillDescriptor, error) {
	w.Mu.Lock()
	commander := w.cmd
	w.Mu.Unlock()
	if commander == nil {
		return nil, fmt.Errorf("opencodeserver: worker not started")
	}
	return commander.ListInvokableSkills(ctx, workDir)
}

func (w *Worker) HandlePermissionResponse(ctx context.Context, reqID string, allowed bool, _ string) error {
	reply := "once"
	if !allowed {
		reply = "reject"
	}
	return w.httpPost(ctx, fmt.Sprintf("/permission/%s/reply", url.PathEscape(reqID)),
		map[string]string{"reply": reply})
}

func (w *Worker) HandleQuestionResponse(ctx context.Context, reqID string, answers map[string]string) error {
	return w.HandleQuestionResponseWithOrder(ctx, reqID, answers, nil)
}

func (w *Worker) HandleQuestionResponseWithOrder(ctx context.Context, reqID string, answers map[string]string, questionOrder []string) error {
	return w.httpPost(ctx, fmt.Sprintf("/question/%s/reply", url.PathEscape(reqID)),
		map[string][][]string{"answers": answersToOrderedArrays(answers, questionOrder)})
}

func (w *Worker) HandleQuestionResponseOptions(ctx context.Context, reqID string, answers map[string][]string, questionOrder []string) error {
	return w.httpPost(ctx, fmt.Sprintf("/question/%s/reply", url.PathEscape(reqID)),
		map[string][][]string{"answers": answerOptionsToOrderedArrays(answers, questionOrder)})
}

func (w *Worker) HandleElicitationResponse(ctx context.Context, reqID, action string, content map[string]any) error {
	payload := map[string]any{"action": action}
	if content != nil {
		payload["content"] = content
	}
	return w.httpPost(ctx, fmt.Sprintf("/elicitation/%s/reply", url.PathEscape(reqID)), payload)
}

const resumeFreshStartNotice = "⚠️ OpenCode 原会话不可用，历史上下文未恢复，已创建新会话。"

// Resume reconnects to an existing session on the shared OpenCode server.
// If a WorkerSessionID from a previous Start is available in session.WorkerSessionID,
// it attempts to reuse that OCS session. Otherwise, it creates a fresh OCS session
// and reports the loss of native context to the Gateway.
func (w *Worker) Resume(ctx context.Context, session worker.SessionInfo) error {
	if err := w.checkNotStarted(); err != nil {
		return err
	}

	w.Log.Debug("opencodeserver: resume step 1 - acquiring server")
	if err := w.acquireServer(ctx); err != nil {
		return err
	}
	w.Log.Debug("opencodeserver: resume step 2 - server acquired", "addr", w.httpAddr)

	// Try to reuse the OCS-internal session if we have one from a previous Start.
	ocsSessionID := session.WorkerSessionID
	freshStart := ocsSessionID == ""
	if ocsSessionID != "" {
		w.Log.Debug("opencodeserver: resume step 3 - checking session existence", "ocs_session_id", ocsSessionID)
		exists, err := w.ocsSessionExists(ctx, ocsSessionID)
		if err != nil {
			w.release()
			return err
		}
		if exists {
			w.Log.Info("opencodeserver: resuming existing OCS session",
				"ocs_session_id", ocsSessionID, "hotplex_session_id", session.SessionID)
			if err := w.initSessionConn(ctx, ocsSessionID, session); err != nil {
				w.release()
				return fmt.Errorf("opencodeserver: initialize resumed session: %w", err)
			}
			w.SetWorkerSessionID(ocsSessionID)
			w.startSSE(ocsSessionID)
			w.Log.Debug("opencodeserver: resume completed (reused session)")
			return nil
		}
		w.Log.Info("opencodeserver: OCS session not found, creating fresh",
			"stale_ocs_session_id", ocsSessionID, "hotplex_session_id", session.SessionID)
		freshStart = true
	}

	// No valid OCS session - create a new one (conversation context is lost).
	w.Log.Debug("opencodeserver: resume step 3 - creating fresh session", "dir", session.ProjectDir)
	newSessionID, err := w.createSession(ctx, session.ProjectDir)
	if err != nil {
		w.releaseOnce.Do(func() { w.singleton.Release() })
		return fmt.Errorf("opencodeserver: resume create session: %w", err)
	}

	if err := w.initSessionConn(ctx, newSessionID, session); err != nil {
		w.releaseOnce.Do(func() { w.singleton.Release() })
		return fmt.Errorf("opencodeserver: initialize resumed session: %w", err)
	}
	w.SetWorkerSessionID(newSessionID)
	w.startSSE(newSessionID)
	if freshStart {
		noticeID := aep.NewID()
		w.httpConn.Inject(events.NewEnvelope(noticeID, session.SessionID, 0, events.Message,
			events.MessageData{ID: noticeID, Role: "system", Content: resumeFreshStartNotice}))
		w.Log.Info("opencodeserver: resume fell back to fresh session",
			"ocs_session_id", newSessionID, "hotplex_session_id", session.SessionID)
		return worker.ErrFellBackToFreshStart
	}
	w.Log.Debug("opencodeserver: resume completed (fresh session)")
	return nil
}

// Terminate closes the SSE connection and releases the singleton ref.
// Does NOT kill the shared server process.
func (w *Worker) Terminate(_ context.Context) error {
	w.release()
	return nil
}

// StopCurrentTurn aborts the current turn on the OCS server in place while
// retaining the httpConn, SSE subscription, singleton ref, and worker session ID
// so the session stays resumable. The singleton ref is released exclusively by
// Terminate/Kill/Wait — an in-place stop is not a termination.
func (w *Worker) StopCurrentTurn(ctx context.Context) error {
	// Snapshot conn/addr/client/projectDir/sessionID under w.Mu only; never
	// hold the lock during the network call.
	w.Mu.Lock()
	conn := w.httpConn
	addr := w.httpAddr
	client := w.client
	var sessionID, projectDir string
	if conn != nil {
		sessionID = conn.getSessionID()
		projectDir = conn.projectDir
	}
	w.Mu.Unlock()

	w.MarkStopped()
	if conn == nil || sessionID == "" || client == nil {
		return nil
	}

	// Bound the abort to 2s; WithTimeout preserves an earlier caller deadline.
	abortCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := abortOCSSession(abortCtx, sessionID, addr, projectDir, client); err != nil {
		// The abort never took effect — the turn is still running and the
		// gateway rolls back its stop fence. Unmark so the turn's legitimate
		// Done is not suppressed forever (the OCS server resumes/emits idle
		// on its own and may publish a Done for the interrupted turn).
		w.ClearStopped()
		return err
	}
	return nil
}

// Kill closes the SSE connection and releases the singleton ref.
// Does NOT kill the shared server process.
func (w *Worker) Kill() error {
	w.release()
	return nil
}

// Wait reports exit code based on whether the singleton process crashed.
// 0 = normal session end, 1 = process crashed.
// It checks for crash signal without blocking indefinitely; Terminate/Kill
// should already have been called before Wait, so the release path is handled
// by those methods.
func (w *Worker) Wait() (int, error) {
	if w.singleton == nil {
		return 0, fmt.Errorf("opencodeserver: not started")
	}

	// Ensure release happens if Terminate/Kill was never called (defensive).
	// Call release() directly — it uses releaseOnce internally for the
	// singleton.Release() call, so this is safe even if already released.
	w.release()

	// Non-blocking crash check: if the process crashed, crashSub is already
	// closed. If not, we return immediately — no need to block 2 seconds.
	// If a subsequent Acquire already recovered the singleton (new process),
	// IsRunning() returns true and we report a clean exit.
	select {
	case <-w.crashSub:
		if w.singleton.IsRunning() {
			return 0, nil // recovered by subsequent Acquire
		}
		return 1, nil // process crashed, not yet recovered
	default:
		return 0, nil
	}
}

// RawExitCode reports the singleton process's raw OS exit code for Bridge
// crash diagnostics. Implements worker.RawExitCoder. See Wait() for why the
// normalized code (0/1) and the raw code differ for the shared OCS singleton.
func (w *Worker) RawExitCode() (int, bool) {
	if w.singleton == nil {
		return 0, false
	}
	return w.singleton.LastExitCode()
}

// Conn returns the HTTP-based session connection.
func (w *Worker) Conn() worker.SessionConn {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.httpConn == nil {
		return nil
	}
	return w.httpConn
}

// Health returns a snapshot of the worker's runtime health.
func (w *Worker) Health() worker.WorkerHealth {
	health := worker.WorkerHealth{
		Type:    worker.TypeOpenCodeSrv,
		Healthy: true,
	}
	if w.singleton != nil {
		health.Running = w.singleton.IsRunning()
		health.Healthy = w.singleton.IsRunning()
	}

	w.Mu.Lock()
	if w.httpConn != nil {
		health.SessionID = w.httpConn.getSessionID()
	}
	if !w.StartTime.IsZero() {
		health.Uptime = time.Since(w.StartTime).Round(time.Second).String()
	}
	w.Mu.Unlock()

	return health
}

// LastIO returns the time of last I/O activity.
func (w *Worker) LastIO() time.Time {
	w.Mu.Lock()
	started := w.httpConn != nil
	w.Mu.Unlock()
	if !started {
		return time.Time{}
	}
	return w.BaseWorker.LastIO()
}

// ResetContext clears the worker runtime context in-place via HTTP API.
func (w *Worker) ResetContext(ctx context.Context) (worker.ResetResult, error) {
	w.Mu.Lock()
	sessionID := ""
	if w.httpConn != nil {
		sessionID = w.httpConn.getSessionID()
	}
	httpAddr := w.httpAddr
	client := w.client
	w.Mu.Unlock()

	if sessionID == "" || httpAddr == "" {
		return worker.ResetResult{}, fmt.Errorf("opencodeserver: reset: worker not started")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", httpAddr+"/session/"+url.PathEscape(sessionID)+"/reset", http.NoBody)
	if err != nil {
		return worker.ResetResult{}, fmt.Errorf("opencodeserver: reset: new request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return worker.ResetResult{}, fmt.Errorf("opencodeserver: reset: http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return worker.ResetResult{}, fmt.Errorf("opencodeserver: reset: status %d: %s", resp.StatusCode, string(body))
	}

	w.IncResetGeneration()
	w.Mu.Lock()
	c := w.httpConn
	w.Mu.Unlock()
	if c != nil {
		c.Inject(&events.Envelope{
			Event: events.Event{
				Type: events.KindInternalReset,
				Data: events.InternalResetData{Generation: w.LoadResetGeneration()},
			},
		})
	}

	return worker.ResetResult{ConnReplaced: false}, nil
}

func (w *Worker) SendControlRequest(ctx context.Context, subtype string, body map[string]any) (map[string]any, error) {
	if subtype == "set_permission_mode" {
		var err error
		body, err = w.preparePermissionModeRequest(body)
		if err != nil {
			return nil, err
		}
	}

	w.Mu.Lock()
	cmd := w.cmd
	w.Mu.Unlock()
	if cmd == nil {
		return nil, fmt.Errorf("opencode server: commander not initialized")
	}
	result, err := cmd.SendControlRequest(ctx, subtype, body)
	if err != nil {
		return result, err
	}
	// Propagate model + variant from set_model to conn for subsequent messages.
	if subtype == "set_model" {
		w.Mu.Lock()
		conn := w.httpConn
		w.Mu.Unlock()
		if conn != nil {
			conn.mu.Lock()
			if pm := cmd.PendingModel(); pm != nil {
				conn.allowedModel = &ocsModelRef{ProviderID: pm.ProviderID, ModelID: pm.ModelID}
			}
			if v, ok := body["variant"].(string); ok && v != "" {
				conn.variant = v
			}
			conn.mu.Unlock()
		}
	}
	return result, nil
}

func (w *Worker) preparePermissionModeRequest(body map[string]any) (map[string]any, error) {
	requested, _ := body["mode"].(string)
	canonical, err := w.permissionCeiling.Check(requested)
	if err != nil {
		if w.BaseWorker != nil && w.Log != nil {
			w.Log.Warn("opencodeserver: permission mode change rejected",
				"security_event", "permission_mode_change_rejected",
				"worker_type", worker.TypeOpenCodeSrv,
				"session_id", w.GetWorkerSessionID(),
				"reason", worker.PermissionRejectionReason(err),
			)
		}
		return nil, fmt.Errorf("opencodeserver: set permission mode: %w", err)
	}

	request := make(map[string]any, len(body)+2)
	for key, value := range body {
		request[key] = value
	}
	request["mode"] = permissionModeToOCS(canonical)
	request["permission_tier"] = canonical
	return request, nil
}

func (w *Worker) Compact(ctx context.Context, args map[string]any) error {
	if w.cmd == nil {
		return fmt.Errorf("opencode server: commander not initialized")
	}
	return w.cmd.Compact(ctx, args)
}

func (w *Worker) Clear(ctx context.Context) error {
	if w.cmd == nil {
		return fmt.Errorf("opencode server: commander not initialized")
	}
	oldID := w.cmd.SessionID()
	if err := w.cmd.Clear(ctx); err != nil {
		return err
	}
	newID := w.cmd.SessionID()
	if newID == oldID || newID == "" {
		return nil
	}
	// Cancel old SSE goroutine and unsubscribe from old session.
	w.Mu.Lock()
	if w.sseCancel != nil {
		w.sseCancel()
	}
	w.Mu.Unlock()
	w.singleton.Unsubscribe(oldID)
	// Propagate new session ID to conn + atomic store.
	w.SetWorkerSessionID(newID)
	// Re-subscribe EventBus for the new session.
	w.startSSE(newID)
	return nil
}

func (w *Worker) Rewind(ctx context.Context, targetID string) error {
	if w.cmd == nil {
		return fmt.Errorf("opencode server: commander not initialized")
	}
	return w.cmd.Rewind(ctx, targetID)
}

// UpdateSystemPrompt updates the stored system prompt on the active HTTP connection.
// This is called by bridge after ResetContext to push a refreshed system prompt
// without requiring a full worker restart. OCS sends the system prompt per-message,
// so updating it here ensures subsequent messages carry the new prompt.
func (w *Worker) UpdateSystemPrompt(prompt string) {
	w.Mu.Lock()
	conn := w.httpConn
	w.Mu.Unlock()

	if conn == nil {
		return
	}
	conn.mu.Lock()
	conn.systemPrompt = prompt
	conn.mu.Unlock()
}

// ─── Internal Methods ─────────────────────────────────────────────────────────

// permissionModeToOCS maps a PermissionMode tier to OCS's native permission mode string.
// workspace and auto-edit both map to acceptEdits (OCS has no finer tier between them).
// Empty/unknown → bypassPermissions (the default blast radius, issue #789).
func permissionModeToOCS(mode string) string {
	switch mode {
	case worker.PermissionModeReadOnly:
		return "plan"
	case worker.PermissionModeWorkspace, worker.PermissionModeAutoEdit:
		return "acceptEdits"
	case worker.PermissionModeBypass:
		return "bypassPermissions"
	default:
		return "bypassPermissions"
	}
}

func (w *Worker) applyPermissions(ctx context.Context, session worker.SessionInfo) error {
	w.Mu.Lock()
	cmd := w.cmd
	w.Mu.Unlock()

	if cmd == nil {
		return fmt.Errorf("commander not initialized")
	}

	ceiling, ok := w.permissionCeiling.Mode()
	if !ok {
		return worker.ErrPermissionCeilingUnset
	}
	mode := permissionModeToOCS(ceiling)

	body := map[string]any{
		"mode":            mode,
		"permission_tier": ceiling,
	}

	// B3-2 绕行: pass AllowedTools so setPermissionMode can generate
	// per-tool allow rules. Semantics differ from CC --allowed-tools:
	// OCS permission rules are session-scoped and pattern-based.
	allowedTools := allowedToolsForOCSPermissionMode(ceiling, session.AllowedTools)
	if len(allowedTools) > 0 {
		body["allowed_tools"] = allowedTools
	}

	_, err := cmd.SendControlRequest(ctx, "set_permission_mode", body)
	return err
}

func allowedToolsForOCSPermissionMode(mode string, tools []string) []string {
	switch mode {
	case worker.PermissionModeReadOnly, worker.PermissionModeWorkspace:
		return nil
	default:
		return tools
	}
}

func (w *Worker) createSession(ctx context.Context, projectDir string) (string, error) {
	// Ensure the project directory exists before binding a session to it.
	// opencode serve is a shared singleton started without any session context,
	// so the workdir is only known here, per session. If it does not exist,
	// OCS receives a non-existent directory and later message handling fails
	// (observed as HTTP 500 "Unexpected server error" on Windows).
	if projectDir != "" {
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			return "", fmt.Errorf("create session workdir: %w", err)
		}
	}

	// OpenCode ≥1.17 honors only the `directory` query param; a `project_dir`
	// JSON body field is ignored and the session falls back to `serve`s cwd.
	// For older versions (<1.17), we also send `project_dir` in the JSON body.
	createURL, err := url.Parse(w.httpAddr + "/session")
	if err != nil {
		return "", fmt.Errorf("create request: parse url: %w", err)
	}
	if projectDir != "" {
		q := createURL.Query()
		q.Set("directory", projectDir)
		createURL.RawQuery = q.Encode()
	}

	body := map[string]any{}
	if projectDir != "" {
		body["project_dir"] = projectDir
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", createURL.String(), bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		// Classify connection failures (e.g. the singleton was idle-drained
		// between Acquire and this call) as Unavailable so the gateway maps
		// them to ErrCodeSessionTerminated and runs crash cleanup — matching
		// the conn.Send path. Without this, a bare wrapped error falls into
		// the default ErrCodeInternalError bucket (see #836 review).
		if isUnreachableError(err) {
			return "", &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: "opencodeserver: server unreachable", Cause: err}
		}
		return "", fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("create session failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.ID, nil
}

func (w *Worker) initHTTPConn(userID, sessionID, systemPrompt string, session worker.SessionInfo) {
	c := &conn{
		userID:       userID,
		sessionID:    sessionID,
		httpAddr:     w.httpAddr,
		client:       w.client,
		sendClient:   newSendClient(),
		recvCh:       make(chan *events.Envelope, recvChannelSize),
		log:          w.Log,
		systemPrompt: systemPrompt,
		projectDir:   session.ProjectDir,
	}

	// Parse AllowedModels[0] → "provider/model" or plain "model".
	if len(session.AllowedModels) > 0 && session.AllowedModels[0] != "" {
		parts := strings.SplitN(session.AllowedModels[0], "/", 2)
		ref := &ocsModelRef{ModelID: parts[0]}
		if len(parts) == 2 {
			ref.ProviderID = parts[0]
			ref.ModelID = parts[1]
		}
		c.allowedModel = ref
	}

	// Parse JSONSchema string → map for PromptInput.format.
	if session.JSONSchema != "" {
		var schema map[string]any
		if err := json.Unmarshal([]byte(session.JSONSchema), &schema); err == nil {
			c.jsonSchema = schema
		} else {
			w.Log.Warn("opencodeserver: invalid JSONSchema, ignoring", "err", err)
		}
	}

	w.httpConn = c
}

func (w *Worker) initSessionConn(ctx context.Context, serverSessionID string, session worker.SessionInfo) error {
	w.cmd = &ServerCommander{
		client:        w.client,
		baseURL:       w.httpAddr,
		sessionID:     serverSessionID,
		contextWindow: w.singleton.cfg.ContextWindow,
		projectDir:    session.ProjectDir,
	}
	effectiveMode := session.PermissionMode
	if effectiveMode == "" {
		effectiveMode = worker.PermissionModeBypass
	}
	if err := w.permissionCeiling.Capture(effectiveMode); err != nil {
		w.cmd = nil
		return fmt.Errorf("capture permission ceiling: %w", err)
	}
	if err := w.applyPermissions(ctx, session); err != nil {
		w.cmd = nil
		return fmt.Errorf("apply session permissions: %w", err)
	}
	w.initHTTPConn(session.UserID, serverSessionID, session.SystemPrompt, session)
	w.Mu.Lock()
	w.StartTime = time.Now()
	w.SetLastIO(w.StartTime)
	w.Mu.Unlock()
	return nil
}

// ocsSessionExists checks whether a session exists on the OpenCode server
// by issuing a lightweight GET to the session messages endpoint.
func (w *Worker) ocsSessionExists(ctx context.Context, sessionID string) (bool, error) {
	checkURL := fmt.Sprintf("%s/session/%s/message?limit=1", w.httpAddr, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("%w: build OCS session request: %w", worker.ErrResumeCheckFailed, err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: query OCS session: %w", worker.ErrResumeCheckFailed, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: OCS session query returned status %d: %s", worker.ErrResumeCheckFailed, resp.StatusCode, string(body))
	}
	return true, nil
}

// ─── Shared Lifecycle Helpers ──────────────────────────────────────────────────

// checkNotStarted validates the singleton is ready and the worker is not
// already running. Shared by Start and Resume.
func (w *Worker) checkNotStarted() error {
	if w.singleton == nil {
		return fmt.Errorf("opencodeserver: singleton not initialized (call InitSingleton first)")
	}
	w.Mu.Lock()
	started := w.httpConn != nil
	w.Mu.Unlock()
	if started {
		return fmt.Errorf("opencodeserver: already started")
	}
	return nil
}

// acquireServer acquires a ref from the singleton process manager.
func (w *Worker) acquireServer(ctx context.Context) error {
	httpAddr, client, _, crashSub, err := w.singleton.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("opencodeserver: acquire server: %w", err)
	}
	w.httpAddr = httpAddr
	w.client = client
	w.crashSub = crashSub
	return nil
}

// startSSE subscribes to the singleton EventBus and starts the forwarder goroutine.
func (w *Worker) startSSE(sessionID string) {
	sseCtx, sseCancel := context.WithCancel(context.Background())
	w.Mu.Lock()
	w.sseCancel = sseCancel
	w.Mu.Unlock()

	// Subscribe to global EventBus instead of opening own SSE connection.
	busCh := w.singleton.Subscribe(sessionID)
	go w.forwardBusEvents(sseCtx, sessionID, busCh)
}

func (w *Worker) forwardBusEvents(ctx context.Context, sessionID string, busCh chan *events.Envelope) {
	defer func() {
		if r := recover(); r != nil {
			w.Log.Error("opencodeserver: forwardBusEvents panic", "session_id", sessionID, "panic", r, "stack", string(debug.Stack()))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-busCh:
			if !ok {
				return
			}

			w.SetLastIO(time.Now())
			if isRunningStateEnvelope(env) {
				w.acceptNativeDispatch()
			}

			w.Mu.Lock()
			ch := w.httpConn
			var recvCh chan *events.Envelope
			if ch != nil {
				ch.mu.Lock()
				if ch.closed {
					ch.mu.Unlock()
					w.Mu.Unlock()
					return
				}
				recvCh = ch.recvCh
				ch.mu.Unlock()
			}
			w.Mu.Unlock()

			if recvCh == nil {
				return
			}

			// While the user-stop marker is set (StopCurrentTurn admitted), the
			// gateway's synthetic done(stopped_by_user) is the authoritative
			// terminal for the stopped turn. The OCS server's abort always
			// transitions the session to idle, and the converter emits its own
			// Done (and possibly Error+Done with a fresh id) for that idle
			// transition — a terminal the gateway's per-turn fence does not
			// dedup. Suppress those worker-emitted terminal envelopes so exactly
			// one terminal reaches the client per turn. BeginTurn (next primary
			// input) clears the marker, so later turns' Dones flow normally.
			if w.IsStopped() && (env.Event.Type == events.Done || env.Event.Type == events.Error) {
				w.Log.Debug("opencodeserver: suppressing worker-emitted terminal event while stopped",
					"event_type", env.Event.Type, "event_id", env.ID, "session_id", sessionID)
				continue
			}

			if isDroppable(env.Event.Type) {
				if !ch.recvGate.TrySend(recvCh, env) {
					w.Log.Warn("opencodeserver: recv channel full or closed, dropping droppable event",
						"event_type", env.Event.Type, "event_id", env.ID)
				}
				continue
			}

			// Critical events retain their bounded send budget. EventGate wakes
			// blocked sends before Close closes recvCh, eliminating the former
			// send/close race rather than merely recovering its panic.
			if !ch.recvGate.SendTimeout(recvCh, env, criticalEventSendTimeout) {
				w.Log.Warn("opencodeserver: critical event send failed (channel closed or stuck)",
					"event_type", env.Event.Type, "event_id", env.ID)
				return
			}
		}
	}
}

func isRunningStateEnvelope(env *events.Envelope) bool {
	if env == nil || env.Event.Type != events.State {
		return false
	}
	switch data := env.Event.Data.(type) {
	case events.StateData:
		return data.State == events.StateRunning
	case map[string]any:
		state, _ := data["state"].(string)
		return state == string(events.StateRunning)
	default:
		return false
	}
}

// release closes the SSE subscription and releases the singleton ref. The OCS
// session is deliberately retained so a later HotPlex resume can reuse its
// conversation context.
func (w *Worker) release() {
	w.Mu.Lock()
	sseCancel := w.sseCancel
	sessionID := ""
	if w.httpConn != nil {
		sessionID = w.httpConn.getSessionID()
	}
	w.Mu.Unlock()

	if sseCancel != nil {
		sseCancel()
	}

	if sessionID != "" && w.singleton != nil {
		w.singleton.Unsubscribe(sessionID)
	}

	w.releaseOnce.Do(func() {
		if w.singleton != nil {
			w.singleton.Release()
		}
	})

	w.Mu.Lock()
	conn := w.httpConn
	w.httpConn = nil
	w.Mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}

// DeletePersistedSession removes a server-side session after its owning
// HotPlex session has been explicitly deleted. It acquires a short-lived
// singleton reference so cleanup also works for already-terminated sessions.
func DeletePersistedSession(ctx context.Context, sessionID string) error {
	mgr := singleton.Load()
	if mgr == nil {
		return fmt.Errorf("opencodeserver: singleton not initialized")
	}
	httpAddr, client, _, _, err := mgr.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("opencodeserver: acquire server for session cleanup: %w", err)
	}
	defer mgr.Release()
	return deleteOCSSession(ctx, sessionID, httpAddr, client)
}

// deleteOCSSession sends DELETE /session/{id} to the OCS server.
func deleteOCSSession(ctx context.Context, sessionID, httpAddr string, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", httpAddr+"/session/"+url.PathEscape(sessionID), http.NoBody)
	if err != nil {
		return fmt.Errorf("opencodeserver: build delete request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opencodeserver: delete session %s: %w", sessionID, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("opencodeserver: delete session %s: unexpected status %d", sessionID, resp.StatusCode)
	}
	return nil
}

// abortOCSSession sends the OCS official POST /session/{id}/abort request and
// reports whether an active turn was aborted. OCS responds 200 with a JSON
// boolean: both true (aborted) and false (no active turn) are success for the
// caller's idempotent-abort intent. Non-200 responses only surface the first
// 4096 body bytes so a misbehaving server cannot flood logs with a full body.
func abortOCSSession(ctx context.Context, sessionID, httpAddr, projectDir string, client *http.Client) error {
	if sessionID == "" || httpAddr == "" || client == nil {
		return fmt.Errorf("opencodeserver: abort session: invalid arguments")
	}

	u := httpAddr + "/session/" + url.PathEscape(sessionID) + "/abort"
	if projectDir != "" {
		q := url.Values{}
		q.Set("directory", projectDir)
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("opencodeserver: abort session: build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opencodeserver: abort session %s: %w", sessionID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencodeserver: abort session %s: status %d, body: %s", sessionID, resp.StatusCode, string(body))
	}

	var aborted bool
	if err := json.NewDecoder(resp.Body).Decode(&aborted); err != nil {
		return fmt.Errorf("opencodeserver: abort session: decode response: %w", err)
	}
	return nil
}

func (w *Worker) httpPost(ctx context.Context, path string, payload any) error {
	w.Mu.Lock()
	addr := w.httpAddr
	conn := w.httpConn
	w.Mu.Unlock()

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("opencodeserver: marshal payload: %w", err)
	}

	var projectDir string
	if conn != nil {
		projectDir = conn.projectDir
	}

	u := addr + path
	if projectDir != "" {
		u += "?directory=" + url.QueryEscape(projectDir)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("opencodeserver: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("opencodeserver: post %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencodeserver: post %s failed: status %d, body: %s",
			path, resp.StatusCode, string(respBody))
	}

	return nil
}

func answersToArrays(m map[string]string) [][]string {
	return answersToOrderedArrays(m, nil)
}

func answersToOrderedArrays(m map[string]string, questionOrder []string) [][]string {
	result := make([][]string, 0, len(questionOrder)+len(m))
	seen := make(map[string]struct{}, len(questionOrder))
	for _, question := range questionOrder {
		if answer, ok := m[question]; ok {
			result = append(result, []string{answer})
		} else {
			result = append(result, []string{})
		}
		seen[question] = struct{}{}
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, []string{m[key]})
	}
	return result
}

func answerOptionsToOrderedArrays(answers map[string][]string, questionOrder []string) [][]string {
	result := make([][]string, 0, len(questionOrder)+len(answers))
	seen := make(map[string]struct{}, len(questionOrder))
	for _, question := range questionOrder {
		if values, ok := answers[question]; ok {
			result = append(result, append([]string{}, values...))
		} else {
			result = append(result, []string{})
		}
		seen[question] = struct{}{}
	}
	keys := make([]string, 0, len(answers))
	for key := range answers {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, append([]string(nil), answers[key]...))
	}
	return result
}

// ─── SessionConn Implementation ───────────────────────────────────────────────

type conn struct {
	userID       string
	sessionID    string
	httpAddr     string
	client       *http.Client
	sendClient   *http.Client // bounded client for prompt acceptance
	recvCh       chan *events.Envelope
	recvGate     base.EventGate
	log          *slog.Logger
	systemPrompt string
	projectDir   string

	allowedModel *ocsModelRef   // parsed from SessionInfo.AllowedModels[0]
	jsonSchema   map[string]any // parsed from SessionInfo.JSONSchema
	variant      string         // reasoning effort variant (e.g. "high", "low")

	mu         sync.Mutex
	closed     bool
	closeOnce  sync.Once
	lastInput  string // cached for crash recovery re-delivery
	lastReplay worker.InputReplay
}

type ocsModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

var (
	_ worker.InputRecoverer       = (*conn)(nil)
	_ worker.InputReplayRecoverer = (*conn)(nil)
)

func (c *conn) LastInput() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastInput
}

func (c *conn) LastInputReplay() worker.InputReplay {
	if c == nil {
		return worker.InputReplay{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	replay := c.lastReplay
	if replay.Skill != nil {
		invocation := *replay.Skill
		replay.Skill = &invocation
	}
	return replay
}

func (c *conn) setSkillReplay(invocation worker.NativeCommandInvocation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastReplay = worker.InputReplay{Content: "/" + invocation.Name, Skill: &invocation}
	if invocation.Args != "" {
		c.lastReplay.Content += " " + invocation.Args
	}
}

func (c *conn) Send(ctx context.Context, msg *events.Envelope) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: "opencodeserver: connection closed"}
	}
	c.mu.Unlock()

	var content string
	if msg.Event.Data != nil {
		switch d := msg.Event.Data.(type) {
		case map[string]any:
			if v, ok := d["content"].(string); ok {
				content = v
			}
		case events.InputData:
			content = d.Content
		}
	}

	// Cache last input for crash recovery re-delivery.
	if content != "" {
		c.mu.Lock()
		c.lastInput = content
		c.lastReplay = worker.InputReplay{Content: content}
		c.mu.Unlock()
	}

	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": content}},
	}
	c.mu.Lock()
	systemPrompt := c.systemPrompt
	jsonSchema := c.jsonSchema
	allowedModel := c.allowedModel
	variant := c.variant
	sessionID := c.sessionID
	c.mu.Unlock()
	if systemPrompt != "" {
		body["system"] = systemPrompt
	}
	if jsonSchema != nil {
		body["format"] = map[string]any{
			"type":   "json_schema",
			"schema": jsonSchema,
		}
	}
	if allowedModel != nil {
		body["model"] = allowedModel
	}
	if variant != "" {
		body["variant"] = variant
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opencodeserver: marshal input: %w", err)
	}

	msgURL := fmt.Sprintf("%s/session/%s/prompt_async", c.httpAddr, url.PathEscape(sessionID))
	if c.projectDir != "" {
		msgURL += "?directory=" + url.QueryEscape(c.projectDir)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", msgURL, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("opencodeserver: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use the prompt-delivery client; fall back to the general client for tests.
	sendCl := c.sendClient
	if sendCl == nil {
		sendCl = c.client
	}
	resp, err := sendCl.Do(req)
	if err != nil {
		if isTimeoutError(err) {
			// Server is alive but response took too long — do not treat as unreachable.
			return &worker.WorkerError{Kind: worker.ErrKindTimeout, Message: "opencodeserver: input delivery timed out", Cause: err}
		}
		if isUnreachableError(err) {
			return &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: "opencodeserver: server unreachable", Cause: err}
		}
		return fmt.Errorf("opencodeserver: send input: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &worker.WorkerError{Kind: worker.ErrKindUnavailable, Message: fmt.Sprintf("opencodeserver: input failed: status %d, body: %s", resp.StatusCode, string(respBody))}
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencodeserver: input failed: status %d, body: %s",
			resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *conn) Recv() <-chan *events.Envelope { return c.recvCh }

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closeOnce.Do(func() {
		c.closed = true
		c.recvGate.Close(c.recvCh)
	})

	return nil
}

func (c *conn) UserID() string { return c.userID }

func (c *conn) SessionID() string {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	return sid
}

// getSessionID reads sessionID under conn.mu for internal use.
func (c *conn) getSessionID() string {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	return sid
}

func (c *conn) Inject(env *events.Envelope) {
	if !c.recvGate.SendTimeout(c.recvCh, env, 2*time.Second) && c.log != nil {
		c.log.Warn("opencodeserver: inject failed, channel closed or full",
			"session_id", c.getSessionID(), "event_type", env.Event.Type)
	}
}

// isTimeoutError reports whether the error is a timeout (deadline exceeded or
// net.Error with Timeout()=true). The server may still be alive and processing.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// isUnreachableError reports whether the error indicates the server is actually
// unreachable (connection refused, DNS failure, etc.) — not a timeout.
func isUnreachableError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return false // timeout is not unreachable
	}
	ne, ok := errors.AsType[net.Error](err)
	return ok && !ne.Timeout()
}

// ─── Init ────────────────────────────────────────────────────────────────────

func init() {
	worker.Register(worker.TypeOpenCodeSrv, func() (worker.Worker, error) {
		return New(), nil
	})
	worker.RegisterSessionCleanup(worker.TypeOpenCodeSrv, DeletePersistedSession)
}
