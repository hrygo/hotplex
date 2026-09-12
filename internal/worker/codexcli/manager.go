package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/internal/worker/proc"
	"github.com/hrygo/hotplex/pkg/events"
)

// managerState represents the lifecycle state of the CodexAppServerManager.
type managerState int

const (
	stateIdle     managerState = iota // no process
	stateStarting                     // process launching, waiting for handshake
	stateRunning                      // process serving JSON-RPC requests
	stateStopped                      // gateway shutdown
)

const (
	defaultCallTimeout       = 30 * time.Second
	codexWriteFallbackGrace  = time.Second
	codexDeadlineWriteGrace  = time.Nanosecond
	criticalEventSendTimeout = 5 * time.Second
	scannerInitSize          = 64 * 1024        // 64 KB
	scannerMaxSize           = 10 * 1024 * 1024 // 10 MB
)

const (
	codexMethodServerRequestApproval = "serverRequest/approval"
	codexMethodCommandApproval       = "item/commandExecution/requestApproval"
	codexMethodFileChangeApproval    = "item/fileChange/requestApproval"
	codexMethodPermissionsApproval   = "item/permissions/requestApproval"
	codexMethodRequestUserInput      = "item/tool/requestUserInput"
	codexMethodMCPElicitation        = "mcpServer/elicitation/request"
	codexMethodExecCommandApproval   = "execCommandApproval"
	codexMethodApplyPatchApproval    = "applyPatchApproval"
)

// CodexAppServerManager manages a single shared `codex app-server` process
// via stdio JSON-RPC across all Codex CLI sessions. The process is lazily
// started on first Acquire and shut down when the last session releases.
//
// # Lifecycle
//
//	idle → starting → running → (crash → idle by monitorProcess) → stopped
//
// # Concurrency
//
// All public methods are safe for concurrent use. Acquire serializes process
// startup via mutex so only the first caller starts the process.
type CodexAppServerManager struct {
	log *slog.Logger
	cfg config.CodexCLIConfig

	mu            sync.Mutex
	proc          *proc.Manager
	stdin         io.WriteCloser
	stdout        io.Reader
	refs          int
	state         managerState
	crashCh       chan struct{} // closed when process exits unexpectedly
	crashExitCode int           // OS exit code set before crashCh is closed

	// pending maps JSON-RPC request IDs to response channels.
	pending sync.Map // map[int64]chan *JSONRPCResponse

	// serverReqIDs maps interaction requestID → raw JSON-RPC frame ID for server-initiated
	// requests, so the worker can respond via RespondServerRequest.
	serverReqIDs sync.Map // map[string]JSONRPCID

	// serverReqMethods maps approval requestID → JSON-RPC method. Codex uses
	// different decision enums for v2 and legacy approval requests.
	serverReqMethods sync.Map // map[string]string

	// serverReqParams retains the original request params until the response is
	// successfully written. Method-specific response codecs (notably granular
	// permissions) need fields from the original request.
	serverReqParams  sync.Map // map[string]json.RawMessage
	serverReqThreads sync.Map // map[string]string (interaction requestID → threadID)

	nextReqID atomic.Int64

	// subMu protects subscribers for thread event routing.
	subMu       sync.Mutex
	subscribers map[string]chan *events.Envelope
	subSessions map[string]string // threadID → sessionID mapping for envelope population
	subsClosed  atomic.Bool       // set when subscribers have been closed (prevents double-close)

	// writeMu serializes writes to stdin from concurrent Call/Notify.
	writeMu sync.Mutex
	stdinMu sync.RWMutex // pointer ownership only; never held across Encode

	// cancel cancels the background readNotifications goroutine.
	cancel context.CancelFunc

	convMu     sync.Mutex
	converters map[string]*Mapper

	idleTimer *time.Timer
	pgid      int // cached PGID from proc.Manager, set in startProcessLocked
}

func NewCodexAppServerManager(log *slog.Logger, cfg config.CodexCLIConfig) *CodexAppServerManager {
	if cfg.IdleDrainPeriod <= 0 {
		cfg.IdleDrainPeriod = 30 * time.Minute
	}
	return &CodexAppServerManager{
		log:         log.With("component", "codex-app-server"),
		cfg:         cfg,
		crashCh:     make(chan struct{}),
		subscribers: make(map[string]chan *events.Envelope),
		subSessions: make(map[string]string),
		converters:  make(map[string]*Mapper),
	}
}

func (m *CodexAppServerManager) getOrCreateConverter(threadID string) *Mapper {
	m.convMu.Lock()
	defer m.convMu.Unlock()
	conv, ok := m.converters[threadID]
	if !ok {
		// Empty sessionID is intentional: dispatchNotification assigns the
		// envelope-level SessionID from subSessions before sending.
		conv = NewMapper("")
		m.converters[threadID] = conv
	}
	return conv
}

func (m *CodexAppServerManager) getConverter(threadID string) *Mapper {
	m.convMu.Lock()
	defer m.convMu.Unlock()
	return m.converters[threadID]
}

func (m *CodexAppServerManager) deleteConverter(threadID string) {
	m.convMu.Lock()
	defer m.convMu.Unlock()
	delete(m.converters, threadID)
}

func (m *CodexAppServerManager) clearConverters() {
	m.convMu.Lock()
	defer m.convMu.Unlock()
	m.converters = make(map[string]*Mapper)
}

func (m *CodexAppServerManager) LastContextUsage(threadID string) map[string]any {
	conv := m.getConverter(threadID)
	if conv == nil {
		return map[string]any{}
	}
	return conv.LastContextUsage()
}

func (m *CodexAppServerManager) SetCurrentModel(threadID, model string) {
	conv := m.getOrCreateConverter(threadID)
	conv.SetModel(model)
}

// Acquire increments the reference count and starts the process if needed.
// CrashExitCode returns the OS exit code from the last process crash.
// Only valid after the crash channel returned by Acquire has been closed.
func (m *CodexAppServerManager) CrashExitCode() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.crashExitCode
}

// Returns a crash notification channel that is closed when the process exits
// unexpectedly. Workers should check this channel in their Wait() implementation.
func (m *CodexAppServerManager) Acquire(ctx context.Context) (<-chan struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == stateStopped {
		return nil, fmt.Errorf("codex-app-server: stopped")
	}

	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}

	if m.state == stateIdle {
		if err := m.startProcessLocked(ctx); err != nil {
			return nil, err
		}
	}

	if m.state != stateRunning {
		return nil, fmt.Errorf("codex-app-server: unexpected state %d", m.state)
	}

	m.refs++
	m.log.Debug("codex-app-server: acquire", "refs", m.refs)
	return m.crashCh, nil
}

// Release decrements the reference count. When refs reach zero, an idle drain
// timer starts. If no new Acquire arrives within idleDrainPeriod, the process
// is killed.
func (m *CodexAppServerManager) Release() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.refs <= 0 {
		m.log.Warn("codex-app-server: release with no active refs")
		return
	}

	m.refs--
	m.log.Debug("codex-app-server: release", "refs", m.refs)

	if m.refs == 0 && m.state == stateRunning {
		m.startIdleDrainLocked()
	}
}

// Subscribe returns a channel that receives AEP events for the given thread ID.
func (m *CodexAppServerManager) Subscribe(threadID, sessionID string) chan *events.Envelope {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	if ch, ok := m.subscribers[threadID]; ok {
		return ch
	}

	ch := make(chan *events.Envelope, 256)
	m.subscribers[threadID] = ch
	m.subSessions[threadID] = sessionID
	m.log.Debug("codex-app-server: subscribed", "thread_id", threadID, "session_id", sessionID)
	return ch
}

// Unsubscribe removes the subscriber channel for the given thread.
// Precondition: the corresponding appConn must be closed before or after this
// call to ensure the channel is eventually cleaned up by monitorProcess.
func (m *CodexAppServerManager) Unsubscribe(threadID string) {
	m.subMu.Lock()
	if _, ok := m.subscribers[threadID]; ok {
		delete(m.subscribers, threadID)
		delete(m.subSessions, threadID)
		m.log.Debug("codex-app-server: unsubscribed", "thread_id", threadID)
	}
	m.subMu.Unlock()

	m.deleteConverter(threadID)
	m.clearServerRequestsForThread(threadID)
}

// Call sends a JSON-RPC request to the app-server process and waits for a response.
// The params argument is marshaled as JSON. If nil, no params field is sent.
func (m *CodexAppServerManager) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := m.nextReqID.Add(1)

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("codex-app-server: marshal params: %w", err)
		}
		req.Params = raw
	}

	respCh := make(chan *JSONRPCResponse, 1)
	m.pending.Store(id, respCh)
	defer m.pending.Delete(id)

	if err := m.writeRequest(ctx, &req); err != nil {
		return nil, fmt.Errorf("codex-app-server: write request: %w", err)
	}

	callTimeout := m.cfg.CallTimeout
	if callTimeout <= 0 {
		callTimeout = defaultCallTimeout
	}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, fmt.Errorf("codex-app-server: %s: %s (code %d)",
				method, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, &responseWaitError{cause: fmt.Errorf("codex-app-server: %s: %w", method, ctx.Err())}
	case <-timer.C:
		return nil, &responseWaitError{cause: fmt.Errorf("codex-app-server: %s: timeout after %v: %w",
			method, callTimeout, context.DeadlineExceeded)}
	}
}

// Notify sends a JSON-RPC notification (no response expected) to the app-server process.
func (m *CodexAppServerManager) Notify(ctx context.Context, method string, params any) error {
	notif := JSONRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("codex-app-server: marshal notification params: %w", err)
		}
		notif.Params = raw
	}

	return m.writeNotification(ctx, &notif)
}

// Shutdown forcefully terminates the process regardless of reference count.
func (m *CodexAppServerManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.state = stateStopped

	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}

	if m.cancel != nil {
		m.cancel()
	}

	if m.proc != nil {
		m.log.Info("codex-app-server: shutdown, killing process")
		// Call ForceKill(pgid) + ForceKillTree directly rather than
		// proc.Manager.Kill(): proc.Kill() is now async-safe (#838), but the
		// direct call is lighter (no proc.mu acquisition, no reap goroutine)
		// and monitorProcess fully handles cleanup after Wait() observes the
		// exit. The defensive m.proc.Kill() fallback covers the pgid==0
		// case (should not happen in normal flow).
		if m.pgid > 0 {
			_ = proc.ForceKill(m.pgid)
			proc.ForceKillTree(m.pgid, m.log)
		} else {
			_ = m.proc.Kill()
		}
		// Do not clear m.proc here — monitorProcess will handle cleanup
		// after Wait() observes the exit. Only clear if no monitorProcess
		// goroutine exists (stateStopped set above prevents double cleanup).
	}

	// Close all active subscriptions if not already closed by monitorProcess.
	if !m.subsClosed.Load() {
		m.subsClosed.Store(true)
		m.subMu.Lock()
		for id, ch := range m.subscribers {
			close(ch)
			delete(m.subscribers, id)
		}
		m.subSessions = make(map[string]string)
		m.subMu.Unlock()
	}

	m.clearConverters()
	m.clearAllServerRequests()
}

// IsRunning reports whether the singleton process is currently running.
func (m *CodexAppServerManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == stateRunning
}

// ─── internal ─────────────────────────────────────────────────────────────

func (m *CodexAppServerManager) startProcessLocked(ctx context.Context) error {
	m.state = stateStarting
	m.subsClosed.Store(false)
	m.log.Info("codex-app-server: starting codex app-server process")

	args := []string{"app-server"}

	parts := strings.Fields(m.cfg.Command)
	if len(parts) == 0 {
		parts = []string{"codex"}
	}
	binary := parts[0]
	fullArgs := make([]string, 0, len(parts)-1+len(args))
	fullArgs = append(fullArgs, parts[1:]...)
	fullArgs = append(fullArgs, args...)

	env := m.buildEnv()
	m.proc = proc.New(proc.Opts{Logger: m.log})

	// Use context.Background() for the long-lived codex process.
	// exec.CommandContext kills the process when the context is done,
	// so a timeout context would kill the process immediately after
	// startProcessLocked returns. The handshake timeout is already
	// enforced by the Call method's own timer.
	stdin, stdout, _, err := m.proc.Start(context.Background(), binary, fullArgs, env, "")
	if err != nil {
		m.proc = nil
		m.state = stateIdle
		return fmt.Errorf("codex-app-server: start process: %w", err)
	}

	m.setTransportWriter(stdin)
	m.stdout = stdout
	m.pgid = m.proc.PGID()

	bgCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.readNotifications(bgCtx, stdout)
	go m.monitorProcess()

	if err := m.handshake(ctx); err != nil {
		cancel()
		_ = m.proc.Kill()
		m.proc = nil
		m.pgid = 0
		m.setTransportWriter(nil)
		m.stdout = nil
		m.state = stateIdle
		return fmt.Errorf("codex-app-server: handshake: %w", err)
	}

	m.state = stateRunning
	m.log.Info("codex-app-server: process started")
	return nil
}

// handshake performs the JSON-RPC initialize/initialized handshake.
func (m *CodexAppServerManager) handshake(ctx context.Context) error {
	type initializeResult struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}

	resp, err := m.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "hotplex",
			"title":   "HotPlex Gateway",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	var result initializeResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse initialize result: %w", err)
	}

	if err := m.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}

	m.log.Info("codex-app-server: handshake complete",
		"capabilities", string(result.Capabilities))
	return nil
}

// writeFrame serializes a JSON-RPC frame to its captured transport. Startup may hold m.mu.
// The write is guarded by ctx: stdin.Encode blocks when the pipe buffer is
// full and the app-server stops reading, so without ctx a single stalled
// write would hold writeMu and deadlock every subsequent Call/Notify across
// all codex sessions (the manager is a singleton).
//
// If ctx has no deadline (e.g. context.Background() from lifecycle wrappers),
// wrap it with a default timeout so the write can never block forever — this
// is especially important for Notify, which has no response-wait timeout
// unlike Call.
func (m *CodexAppServerManager) writeFrame(ctx context.Context, v any) error {
	if err := ctx.Err(); err != nil {
		return &writeNotStartedError{cause: err}
	}
	// Capture before queueing. Lifecycle replacement must never redirect a
	// previously admitted frame to a different process. The state lock is
	// distinct from m.mu because startup handshake already owns m.mu.
	writer := m.transportWriter()
	if writer == nil {
		return &writeNotStartedError{cause: io.ErrClosedPipe}
	}
	// 0: queued; 1: encoder owns frame; 2: caller abandoned before encoding.
	// CAS linearizes cancellation against starting I/O, including a helper that
	// is still waiting for writeMu after WriteWithCtx has returned.
	var writePhase atomic.Int32
	_, callerHasDeadline := ctx.Deadline()
	// The mutex MUST be acquired inside the closure, not at the call site:
	// if ctx cancels and WriteWithCtx bails out, an orphaned goroutine
	// may still be blocked inside Encode → syscall.Write. By locking/unlocking
	// inside the closure (which runs in the helper's goroutine), the orphan
	// holds writeMu until the write completes (child exits → EPIPE),
	// preventing the next writer from racing on the shared stdin fd. This is
	// especially critical for codexcli: the manager is a singleton, so an
	// interleaved write would corrupt the protocol stream for ALL sessions.
	writeCtx := ctx
	if _, ok := writeCtx.Deadline(); !ok && writeCtx.Err() == nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(writeCtx, base.DefaultWriteTimeout)
		defer cancel()
	}
	fallbackGrace := codexWriteFallbackGrace
	if callerHasDeadline {
		// The caller already budgeted the operation. Do not extend that
		// deadline: Bridge reserves its following one-second window for local
		// forwarder quiescence, not for a blocked singleton stdin write.
		fallbackGrace = codexDeadlineWriteGrace
	}
	err := base.WriteWithCtx(writeCtx, func() error {
		m.writeMu.Lock()
		defer m.writeMu.Unlock()
		if err := writeCtx.Err(); err != nil {
			return &writeNotStartedError{cause: err}
		}
		if !writePhase.CompareAndSwap(0, 1) {
			return &writeNotStartedError{cause: writeCtx.Err()}
		}
		return json.NewEncoder(writer).Encode(v)
	}, fallbackGrace)
	// An orphaned write (ctx cancelled while syscall.Write is blocked) leaves
	// the goroutine holding writeMu until the child exits. Because the manager
	// is a singleton shared across all codex sessions, this wedges EVERY
	// session's writes until recovery. Log at warn so operators can correlate
	// a multi-session stall with a stalled child process.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if writePhase.CompareAndSwap(0, 2) {
			return &writeNotStartedError{cause: err}
		}
		m.log.Warn("codex-app-server: stdin write cancelled, writeMu held by orphaned goroutine until child exits",
			"err", err)
	}
	return err
}

// writeRequest marshals and writes a JSON-RPC request to stdin.
func (m *CodexAppServerManager) writeRequest(ctx context.Context, req *JSONRPCRequest) error {
	return m.writeFrame(ctx, req)
}

// writeNotification marshals and writes a JSON-RPC notification to stdin.
func (m *CodexAppServerManager) writeNotification(ctx context.Context, notif *JSONRPCNotification) error {
	return m.writeFrame(ctx, notif)
}

// readNotifications reads JSON-RPC frames from stdout and routes them to
// pending response channels or subscriber notification channels.
// reader is passed in to avoid acquiring m.mu at startup (caller holds it).
func (m *CodexAppServerManager) readNotifications(ctx context.Context, reader io.Reader) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("codex-app-server: readNotifications panic",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	if reader == nil {
		return
	}

	scanner := bufio.NewScanner(reader)
	buf := make([]byte, scannerInitSize)
	scanner.Buffer(buf, scannerMaxSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				m.log.Warn("codex-app-server: stdout read error", "err", err)
			}
			return
		}

		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}

		m.dispatchFrame(data)
	}
}

// dispatchFrame parses a single JSON-RPC frame and routes to the correct handler.
//
// Routing logic (in order):
//  1. Method != "" && ID present → server-initiated request (e.g. approval)
//  2. ID present && Method == "" → response to a client request
//  3. ID absent && Error != nil → uncorrelated error, dropped
//  4. ID absent && Method != "" → notification
func (m *CodexAppServerManager) dispatchFrame(data []byte) {
	var frame JSONRPCFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		m.log.Warn("codex-app-server: unmarshal frame", "err", err)
		return
	}

	// Server-initiated request (has both ID and Method, e.g. approval).
	if frame.Method != "" && frame.ID.IsSet() {
		m.dispatchServerRequest(&frame)
		return
	}

	if frame.ID.IsSet() {
		responseID, ok := frame.ID.Int64()
		if !ok {
			m.log.Warn("codex-app-server: response has non-integer client request id", "id", frame.ID.Key())
			return
		}
		resp := &JSONRPCResponse{
			JSONRPC: frame.JSONRPC,
			ID:      responseID,
			Result:  frame.Result,
			Error:   frame.Error,
		}
		m.dispatchResponse(resp)
		return
	}

	// ID absent: notification or unknown.
	if frame.Error != nil {
		m.log.Debug("codex-app-server: error frame without ID, dropping", "err", frame.Error.Message)
		return
	}

	if frame.Method != "" {
		notif := &JSONRPCNotification{
			JSONRPC: frame.JSONRPC,
			Method:  frame.Method,
			Params:  frame.Params,
		}
		m.dispatchNotification(notif)
	} else {
		m.log.Debug("codex-app-server: frame without ID, method, or error — dropped")
	}
}

// dispatchResponse routes a JSON-RPC response to the pending request channel.
func (m *CodexAppServerManager) dispatchResponse(resp *JSONRPCResponse) {
	if v, ok := m.pending.Load(resp.ID); ok {
		if ch, ok := v.(chan *JSONRPCResponse); ok {
			select {
			case ch <- resp:
			default:
				m.log.Warn("codex-app-server: response channel full, dropping",
					"id", resp.ID)
			}
		}
	}
}

// dispatchServerRequest handles server-initiated JSON-RPC requests (e.g. approvals).
// It extracts the thread ID, maps the request to AEP envelopes via the converter,
// and stores the request ID → frame ID mapping so the worker can respond later.
func (m *CodexAppServerManager) dispatchServerRequest(frame *JSONRPCFrame) {
	// Parse all possible ID fields — different approval notification types use different names:
	//   serverRequest/approval                → requestId
	//   item/commandExecution/requestApproval → approvalId (null for regular shell) or itemId
	//   item/fileChange/requestApproval       → itemId
	//   execCommandApproval                   → approvalId or callId
	//   applyPatchApproval                    → callId
	var params struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
		RequestID      string `json:"requestId"`
		ApprovalID     string `json:"approvalId"`
		ItemID         string `json:"itemId"`
		CallID         string `json:"callId"`
	}
	if frame.Params != nil {
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			m.log.Warn("codex-app-server: unmarshal server request params", "method", frame.Method, "err", err)
			return
		}
	}

	threadID := params.ThreadID
	if threadID == "" {
		threadID = params.ConversationID
	}
	if threadID == "" {
		m.log.Debug("codex-app-server: server request without threadId/conversationId, dropping",
			"method", frame.Method, "id", frame.ID)
		return
	}

	// Resolve canonical request ID (mirrors mapNotifApproval priority:
	// approvalId → itemId → requestId → callId).
	requestID := params.ApprovalID
	if requestID == "" {
		requestID = params.ItemID
	}
	if requestID == "" {
		requestID = params.RequestID
	}
	if requestID == "" {
		requestID = params.CallID
	}

	// When params do not carry an interaction identifier (current MCP
	// elicitation), derive a stable opaque AEP ID from the JSON-RPC ID.
	if requestID == "" {
		requestID = "codex-rpc:" + frame.ID.Key()
	}

	// Store the JSON-RPC frame ID and method metadata so the worker can reply
	// using the correct native response schema.
	if requestID != "" {
		m.serverReqIDs.Store(requestID, frame.ID.Clone())
		m.serverReqMethods.Store(requestID, frame.Method)
		m.serverReqParams.Store(requestID, append(json.RawMessage(nil), frame.Params...))
		m.serverReqThreads.Store(requestID, threadID)
	} else {
		m.log.Warn("codex-app-server: server request has no usable requestId/approvalId/itemId/callId — response routing impossible",
			"method", frame.Method, "frame_id", frame.ID)
	}

	// Map and deliver as notification to the subscriber.
	notif := &JSONRPCNotification{
		JSONRPC: frame.JSONRPC,
		Method:  frame.Method,
		Params:  injectServerRequestID(frame.Params, requestID),
	}
	m.dispatchNotification(notif)
}

// RespondServerRequest sends a JSON-RPC response to a server-initiated request.
// reqID is the approval request's requestId; result is the response payload,
// which should contain a "decision" or "behavior" key for server request handling.
func (m *CodexAppServerManager) RespondServerRequest(ctx context.Context, reqID string, result any) error {
	// Validate before consuming reqID so validation failures don't orphan
	// the codex process waiting for a response that will never arrive.
	if check, ok := result.(map[string]any); ok {
		_, hasBehavior := check["behavior"]
		_, hasDecision := check["decision"]
		_, hasAction := check["action"]
		_, hasAnswers := check["answers"]
		_, hasPermissions := check["permissions"]
		if !hasBehavior && !hasDecision && !hasAction && !hasAnswers && !hasPermissions {
			return fmt.Errorf("codex-app-server: server response for %q has no recognized result key", reqID)
		}
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("codex-app-server: marshal server response: %w", err)
	}

	v, ok := m.serverReqIDs.Load(reqID)
	if !ok {
		return fmt.Errorf("codex-app-server: no pending server request for %q", reqID)
	}
	rpcID, ok := serverRequestJSONRPCID(v)
	if !ok || !rpcID.IsSet() {
		return fmt.Errorf("codex-app-server: invalid rpc id type for %q", reqID)
	}

	resp := JSONRPCServerResponse{
		JSONRPC: "2.0",
		ID:      rpcID,
		Result:  raw,
	}
	if err := m.writeFrame(ctx, resp); err != nil {
		return err
	}
	m.clearServerRequest(reqID)
	return nil
}

func serverRequestJSONRPCID(value any) (JSONRPCID, bool) {
	switch id := value.(type) {
	case JSONRPCID:
		return id.Clone(), true
	case int64: // compatibility with older in-process registrations/tests
		return integerJSONRPCID(id), true
	default:
		return nil, false
	}
}

func injectServerRequestID(params json.RawMessage, requestID string) json.RawMessage {
	if requestID == "" {
		return params
	}
	var values map[string]any
	if len(params) == 0 || json.Unmarshal(params, &values) != nil {
		return params
	}
	if current, _ := values["requestId"].(string); current == "" {
		values["requestId"] = requestID
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return params
	}
	return encoded
}

func (m *CodexAppServerManager) ApprovalDecision(reqID string, allowed bool) string {
	method := m.ServerRequestMethod(reqID)
	return codexApprovalDecision(method, allowed)
}

func (m *CodexAppServerManager) ServerRequestMethod(reqID string) string {
	method := ""
	if v, ok := m.serverReqMethods.Load(reqID); ok {
		method, _ = v.(string)
	}
	return method
}

func (m *CodexAppServerManager) PermissionResponseResult(reqID string, allowed bool, reason string) (map[string]any, error) {
	if m.ServerRequestMethod(reqID) != codexMethodPermissionsApproval {
		result := map[string]any{"decision": m.ApprovalDecision(reqID, allowed)}
		if reason != "" {
			result["reason"] = reason
		}
		return result, nil
	}

	permissions := map[string]any{}
	if allowed {
		params, ok := m.serverRequestParams(reqID)
		if !ok {
			return nil, fmt.Errorf("codex-app-server: missing permissions request params for %q", reqID)
		}
		var request struct {
			Permissions map[string]any `json:"permissions"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, fmt.Errorf("codex-app-server: decode permissions request %q: %w", reqID, err)
		}
		if request.Permissions != nil {
			permissions = request.Permissions
		}
	}
	return map[string]any{
		"scope":       "turn",
		"permissions": permissions,
	}, nil
}

func (m *CodexAppServerManager) QuestionResponseResult(reqID string, answers map[string]string) (map[string]any, error) {
	answerOptions := make(map[string][]string, len(answers))
	for key, answer := range answers {
		answerOptions[key] = []string{answer}
	}
	return m.QuestionResponseOptionsResult(reqID, answerOptions)
}

func (m *CodexAppServerManager) QuestionResponseOptionsResult(reqID string, answers map[string][]string) (map[string]any, error) {
	if m.ServerRequestMethod(reqID) != codexMethodRequestUserInput {
		flattened := make(map[string]string, len(answers))
		for key, options := range answers {
			flattened[key] = strings.Join(options, ", ")
		}
		return map[string]any{
			"behavior": "allow",
			"updatedInput": map[string]any{
				"answers": flattened,
			},
		}, nil
	}

	params, ok := m.serverRequestParams(reqID)
	if !ok {
		return nil, fmt.Errorf("codex-app-server: missing user-input request params for %q", reqID)
	}
	var request struct {
		Questions []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, fmt.Errorf("codex-app-server: decode user-input request %q: %w", reqID, err)
	}

	nativeAnswers := make(map[string]any, len(answers))
	consumed := make(map[string]bool, len(answers))
	for _, question := range request.Questions {
		answer, found := answers[question.ID]
		if found {
			consumed[question.ID] = true
		} else {
			answer, found = answers[question.Question]
			if found {
				consumed[question.Question] = true
			}
		}
		if found {
			nativeAnswers[question.ID] = map[string]any{"answers": answer}
		}
	}
	for key, answer := range answers {
		if !consumed[key] {
			nativeAnswers[key] = map[string]any{"answers": answer}
		}
	}
	return map[string]any{"answers": nativeAnswers}, nil
}

func (m *CodexAppServerManager) serverRequestParams(reqID string) (json.RawMessage, bool) {
	value, ok := m.serverReqParams.Load(reqID)
	if !ok {
		return nil, false
	}
	params, ok := value.(json.RawMessage)
	return params, ok
}

func (m *CodexAppServerManager) clearServerRequest(reqID string) {
	m.serverReqIDs.Delete(reqID)
	m.serverReqMethods.Delete(reqID)
	m.serverReqParams.Delete(reqID)
	m.serverReqThreads.Delete(reqID)
}

func (m *CodexAppServerManager) clearServerRequestsForThread(threadID string) {
	m.serverReqThreads.Range(func(key, value any) bool {
		requestID, keyOK := key.(string)
		requestThread, valueOK := value.(string)
		if keyOK && valueOK && requestThread == threadID {
			m.clearServerRequest(requestID)
		}
		return true
	})
}

func (m *CodexAppServerManager) clearAllServerRequests() {
	m.serverReqIDs.Range(func(key, _ any) bool {
		if requestID, ok := key.(string); ok {
			m.clearServerRequest(requestID)
		}
		return true
	})
}

func (m *CodexAppServerManager) clearServerRequestByRPCID(id JSONRPCID) {
	if !id.IsSet() {
		return
	}
	m.serverReqIDs.Range(func(key, value any) bool {
		stored, ok := serverRequestJSONRPCID(value)
		if !ok || string(stored) != string(id) {
			return true
		}
		if requestID, ok := key.(string); ok {
			m.clearServerRequest(requestID)
		}
		return true
	})
}

func codexApprovalDecision(method string, allowed bool) string {
	if isLegacyApprovalMethod(method) {
		if allowed {
			return "approved"
		}
		return "denied"
	}
	if allowed {
		return "accept"
	}
	return "decline"
}

func isLegacyApprovalMethod(method string) bool {
	switch method {
	case codexMethodServerRequestApproval, codexMethodExecCommandApproval, codexMethodApplyPatchApproval:
		return true
	default:
		return false
	}
}

// ─── Thread Lifecycle ─────────────────────────────────────────────────────

// ResumeThread resumes a paused or interrupted thread.
func (m *CodexAppServerManager) ResumeThread(threadID string) (json.RawMessage, error) {
	resp, err := m.Call(context.Background(), "thread/resume", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: thread/resume: %w", err)
	}
	return resp, nil
}

// ForkThread forks a thread at the current state with additional parameters.
func (m *CodexAppServerManager) ForkThread(threadID string, params map[string]any) (json.RawMessage, error) {
	merged := make(map[string]any, len(params)+1)
	maps.Copy(merged, params)
	merged["threadId"] = threadID // authoritative value, overrides any caller-supplied threadId in params
	resp, err := m.Call(context.Background(), "thread/fork", merged)
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: thread/fork: %w", err)
	}
	return resp, nil
}

// ArchiveThread archives a thread so it no longer appears in the active list.
func (m *CodexAppServerManager) ArchiveThread(threadID string) error {
	err := m.Notify(context.Background(), "thread/archive", map[string]any{"threadId": threadID})
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/archive: %w", err)
	}
	return nil
}

// UnarchiveThread restores a previously archived thread.
func (m *CodexAppServerManager) UnarchiveThread(threadID string) error {
	err := m.Notify(context.Background(), "thread/unarchive", map[string]any{"threadId": threadID})
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/unarchive: %w", err)
	}
	return nil
}

// SetThreadName updates the display name of a thread.
func (m *CodexAppServerManager) SetThreadName(threadID, name string) error {
	err := m.Notify(context.Background(), "thread/name/set", map[string]any{
		"threadId": threadID,
		"name":     name,
	})
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/name/set: %w", err)
	}
	return nil
}

// SetThreadGoal sets a goal for the specified thread.
// Upstream ThreadGoalSetParams uses "objective" (thread.rs:746), not "goal".
func (m *CodexAppServerManager) SetThreadGoal(threadID, objective string) error {
	err := m.Notify(context.Background(), "thread/goal/set", map[string]any{
		"threadId":  threadID,
		"objective": objective,
	})
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/goal/set: %w", err)
	}
	return nil
}

// GetThreadGoal retrieves the current goal for the specified thread.
func (m *CodexAppServerManager) GetThreadGoal(threadID string) (json.RawMessage, error) {
	resp, err := m.Call(context.Background(), "thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: thread/goal/get: %w", err)
	}
	return resp, nil
}

// ClearThreadGoal removes the goal from the specified thread.
func (m *CodexAppServerManager) ClearThreadGoal(threadID string) error {
	err := m.Notify(context.Background(), "thread/goal/clear", map[string]any{"threadId": threadID})
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/goal/clear: %w", err)
	}
	return nil
}

// UpdateThreadSettings updates the settings configuration for a thread.
// Upstream ThreadSettingsUpdateParams (thread.rs:229) is a flat struct: settings
// keys (cwd, approvalPolicy, sandboxPolicy, model, ...) sit alongside threadId,
// not nested under a "settings" wrapper.
func (m *CodexAppServerManager) UpdateThreadSettings(threadID string, settings map[string]any) error {
	params := make(map[string]any, len(settings)+1)
	maps.Copy(params, settings)
	params["threadId"] = threadID // authoritative value, overrides any caller-supplied threadId in settings
	err := m.Notify(context.Background(), "thread/settings/update", params)
	if err != nil {
		return fmt.Errorf("codex-app-server: thread/settings/update: %w", err)
	}
	return nil
}

// CompactThread starts context compaction for the specified thread, returning
// the compaction result.
func (m *CodexAppServerManager) CompactThread(threadID string) (json.RawMessage, error) {
	return m.CompactThreadContext(context.Background(), threadID)
}

// CompactThreadContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) CompactThreadContext(ctx context.Context, threadID string) (json.RawMessage, error) {
	resp, err := m.Call(ctx, "thread/compact/start", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: thread/compact/start: %w", err)
	}
	return resp, nil
}

// RollbackThread drops numTurns turns from the end of the thread.
// Upstream ThreadRollbackParams (thread.rs:956) requires num_turns (>= 1),
// not a target ID. numTurns < 1 is clamped to 1.
func (m *CodexAppServerManager) RollbackThread(threadID string, numTurns uint32) (json.RawMessage, error) {
	return m.RollbackThreadContext(context.Background(), threadID, numTurns)
}

// RollbackThreadContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) RollbackThreadContext(ctx context.Context, threadID string, numTurns uint32) (json.RawMessage, error) {
	if numTurns < 1 {
		numTurns = 1
	}
	resp, err := m.Call(ctx, "thread/rollback", map[string]any{
		"threadId": threadID,
		"numTurns": numTurns,
	})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: thread/rollback: %w", err)
	}
	return resp, nil
}

// ─── Turn Control ──────────────────────────────────────────────────────────

// SteerTurn injects mid-turn input into the active turn of a thread.
// Upstream TurnSteerParams (turn.rs:160) requires input (Vec<UserInput>) and
// expectedTurnId (the currently-active turn; the request fails if it does not
// match). The text is wrapped as a single UserInput text item.
func (m *CodexAppServerManager) SteerTurn(threadID, expectedTurnID, text string) (json.RawMessage, error) {
	return m.SteerTurnContext(context.Background(), threadID, expectedTurnID, text)
}

// SteerTurnContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) SteerTurnContext(ctx context.Context, threadID, expectedTurnID, text string) (json.RawMessage, error) {
	resp, err := m.Call(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": expectedTurnID,
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: turn/steer: %w", err)
	}
	return resp, nil
}

// InterruptTurn interrupts the running turn in the specified thread.
// Upstream TurnInterruptParams (turn.rs:188) requires both threadId and turnId.
func (m *CodexAppServerManager) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	err := m.Notify(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	if err != nil {
		return fmt.Errorf("codex-app-server: turn/interrupt: %w", err)
	}
	return nil
}

// ─── Environment ───────────────────────────────────────────────────────────

// AddEnvironment registers a remote execution environment with the app-server.
// Upstream EnvironmentAddParams (environment.rs:9) requires environmentId and
// execServerUrl — it is not a key/value env-var setter.
func (m *CodexAppServerManager) AddEnvironment(environmentID, execServerURL string) error {
	err := m.Notify(context.Background(), "environment/add", map[string]any{
		"environmentId": environmentID,
		"execServerUrl": execServerURL,
	})
	if err != nil {
		return fmt.Errorf("codex-app-server: environment/add: %w", err)
	}
	return nil
}

// ─── MCP ───────────────────────────────────────────────────────────────────

// ListMCPServerStatus returns the status of all configured MCP servers.
// Upstream ListMcpServerStatusParams (v2/mcp.rs:36) has no serde(default),
// so the params field must be present — send an empty object, not nil,
// or codex-app-server rejects the request with "missing field params".
func (m *CodexAppServerManager) ListMCPServerStatus() (json.RawMessage, error) {
	return m.ListMCPServerStatusContext(context.Background())
}

// ListMCPServerStatusContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) ListMCPServerStatusContext(ctx context.Context) (json.RawMessage, error) {
	resp, err := m.Call(ctx, "mcpServerStatus/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: mcpServerStatus/list: %w", err)
	}
	return resp, nil
}

// ReadMCPResource reads a resource from a configured MCP server.
// Upstream McpResourceReadParams (mcp.rs:77) requires both server and uri.
func (m *CodexAppServerManager) ReadMCPResource(server, uri string) (json.RawMessage, error) {
	resp, err := m.Call(context.Background(), "mcpServer/resource/read", map[string]any{
		"server": server,
		"uri":    uri,
	})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: mcpServer/resource/read: %w", err)
	}
	return resp, nil
}

// CallMCPTool invokes a tool on a configured MCP server.
// Upstream McpServerToolCallParams (mcp.rs:94) requires threadId, server, tool;
// arguments is optional. The args map is omitted from params if nil.
func (m *CodexAppServerManager) CallMCPTool(threadID, server, tool string, args map[string]any) (json.RawMessage, error) {
	params := map[string]any{
		"threadId": threadID,
		"server":   server,
		"tool":     tool,
	}
	if args != nil {
		params["arguments"] = args
	}
	resp, err := m.Call(context.Background(), "mcpServer/tool/call", params)
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: mcpServer/tool/call: %w", err)
	}
	return resp, nil
}

// RefreshMCPServer reloads the global MCP server registry.
// Upstream McpServerRefresh (common.rs:874) takes no params (undefined/None).
func (m *CodexAppServerManager) RefreshMCPServer() error {
	return m.RefreshMCPServerContext(context.Background())
}

// RefreshMCPServerContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) RefreshMCPServerContext(ctx context.Context) error {
	err := m.Notify(ctx, "config/mcpServer/reload", nil)
	if err != nil {
		return fmt.Errorf("codex-app-server: config/mcpServer/reload: %w", err)
	}
	return nil
}

// MCPServerOAuthLogin initiates an OAuth login flow for the named MCP server.
// Upstream McpServerOauthLoginParams (mcp.rs:187) uses "name", not "serverName".
func (m *CodexAppServerManager) MCPServerOAuthLogin(name string) (json.RawMessage, error) {
	return m.MCPServerOAuthLoginContext(context.Background(), name)
}

// MCPServerOAuthLoginContext preserves the caller cancellation budget.
func (m *CodexAppServerManager) MCPServerOAuthLoginContext(ctx context.Context, name string) (json.RawMessage, error) {
	resp, err := m.Call(ctx, "mcpServer/oauth/login", map[string]any{"name": name})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: mcpServer/oauth/login: %w", err)
	}
	return resp, nil
}

// ─── Review ────────────────────────────────────────────────────────────────

// StartReview initiates a code review on the given thread.
// Upstream ReviewStartParams (review.rs:17) requires both threadId and target.
func (m *CodexAppServerManager) StartReview(threadID string, target map[string]any) (json.RawMessage, error) {
	resp, err := m.Call(context.Background(), "review/start", map[string]any{
		"threadId": threadID,
		"target":   target,
	})
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: review/start: %w", err)
	}
	return resp, nil
}

// dispatchNotification extracts the thread ID, converts via the mapper, and
// delivers envelopes to the subscriber channel. Locks subMu once per notification.
func (m *CodexAppServerManager) dispatchNotification(notif *JSONRPCNotification) {
	if notif.Method == "serverRequest/resolved" {
		var resolved struct {
			RequestID JSONRPCID `json:"requestId"`
		}
		if len(notif.Params) > 0 && json.Unmarshal(notif.Params, &resolved) == nil {
			m.clearServerRequestByRPCID(resolved.RequestID)
		}
	}
	var params struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
	}
	if notif.Params != nil {
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			m.log.Warn("codex-app-server: unmarshal notification params", "err", err)
			return
		}
	}

	if params.ThreadID == "" {
		params.ThreadID = params.ConversationID
	}

	if params.ThreadID == "" {
		// Also try nested thread.id format (codex sends some notifications with
		// params.thread.id instead of params.threadId).
		var nested struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if notif.Params != nil {
			_ = json.Unmarshal(notif.Params, &nested)
		}
		if nested.Thread.ID != "" {
			params.ThreadID = nested.Thread.ID
		}
	}

	if params.ThreadID == "" {
		m.log.Debug("codex-app-server: notification without threadId, skipping", "method", notif.Method)
		return
	}

	// Only log lifecycle events; skip high-frequency deltas to avoid log flooding.
	switch notif.Method {
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta",
		"item/reasoning/textDelta", "item/commandExecution/outputDelta",
		"thread/tokenUsage/updated", "mcpServer/startupStatus/updated":
	default:
		m.log.Debug("codex-app-server: dispatching notification", "method", notif.Method, "thread_id", params.ThreadID)
	}

	m.subMu.Lock()
	sessionID := m.subSessions[params.ThreadID]
	ch, ok := m.subscribers[params.ThreadID]
	m.subMu.Unlock()
	if !ok {
		return
	}

	conv := m.getOrCreateConverter(params.ThreadID)
	envs := conv.MapNotification(notif.Method, notif.Params)
	for _, env := range envs {
		if env != nil {
			env.SessionID = sessionID
			m.sendEnvelope(ch, env)
		}
	}
}

// sendEnvelope delivers a single envelope to a subscriber channel with backpressure.
// Delta events are dropped silently when full; critical events block with a 5s timeout.
func (m *CodexAppServerManager) sendEnvelope(ch chan *events.Envelope, env *events.Envelope) {
	// Recover from send on closed channel — release() may close ch
	// concurrently with Unsubscribe+conn.Close after we released subMu.
	defer func() {
		if r := recover(); r != nil {
			m.log.Debug("codex-app-server: send on closed channel, subscriber gone")
		}
	}()
	if env.Event.Type == events.MessageDelta || env.Event.Type == events.Reasoning {
		select {
		case ch <- env:
		default:
		}
		return
	}
	timer := time.NewTimer(criticalEventSendTimeout)
	defer timer.Stop()
	select {
	case ch <- env:
	case <-timer.C:
		m.log.Warn("codex-app-server: critical event send timeout, dropping",
			"event_type", env.Event.Type)
	}
}

// monitorProcess waits for the process to exit and handles crash recovery.
func (m *CodexAppServerManager) monitorProcess() {
	m.mu.Lock()
	pm := m.proc
	m.mu.Unlock()
	if pm == nil {
		return
	}
	code, _ := pm.Wait()

	m.mu.Lock()
	wasRunning := m.state == stateRunning
	refs := m.refs
	// Guard against overwriting stateStopped set by Shutdown (mirrors OCS
	// singleton.go): once stopped, monitorProcess must not flip the manager
	// back to idle, which would let a post-Shutdown Acquire restart it.
	if m.state != stateStopped {
		m.state = stateIdle
	}
	m.proc = nil
	m.pgid = 0
	m.setTransportWriter(nil)
	m.stdout = nil

	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	if wasRunning && refs > 0 {
		m.log.Warn("codex-app-server: process crashed", "exit_code", code, "refs", refs)
		m.crashExitCode = code
		close(m.crashCh)
		m.crashCh = make(chan struct{})
	} else {
		m.log.Info("codex-app-server: process exited", "exit_code", code, "refs", refs)
	}
	m.mu.Unlock()

	if wasRunning {
		m.clearConverters()
		m.clearAllServerRequests()
		m.subsClosed.Store(true)
		m.subMu.Lock()
		for id, ch := range m.subscribers {
			close(ch)
			delete(m.subscribers, id)
		}
		m.subSessions = make(map[string]string)
		m.subMu.Unlock()
	}
}

// startIdleDrainLocked starts a timer to kill the process when idle.
// Caller must hold m.mu.
func (m *CodexAppServerManager) startIdleDrainLocked() {
	m.log.Info("codex-app-server: starting idle drain timer",
		"period", m.cfg.IdleDrainPeriod)
	m.idleTimer = time.AfterFunc(m.cfg.IdleDrainPeriod, func() {
		m.mu.Lock()
		if m.idleTimer == nil {
			// Timer was already stopped (e.g. by KillIfIdle or Acquire).
			m.mu.Unlock()
			return
		}
		shouldKill := m.refs == 0 && m.state == stateRunning && m.pgid > 0
		pgid := m.pgid
		m.mu.Unlock()

		if shouldKill {
			m.log.Info("codex-app-server: idle drain expired, killing process")
			// Call package-level ForceKill + ForceKillTree directly instead
			// of m.proc.Kill(). proc.Manager.Kill() is now async-safe (#838),
			// but the direct call is defense-in-depth: it sends SIGKILL by
			// PGID via syscall.Kill WITHOUT touching proc.mu, guaranteeing
			// no interaction with monitorProcess's concurrent Wait() — which
			// is what owns the singleton's post-exit cleanup. monitorProcess
			// will observe the exit, set state to stateIdle, and clean up.
			_ = proc.ForceKill(pgid)
			proc.ForceKillTree(pgid, m.log)
		}

		m.mu.Lock()
		m.idleTimer = nil
		m.mu.Unlock()
	})
}

// KillIfIdle immediately kills the singleton process when no sessions hold
// references (refs == 0). Used by both Kill() and Terminate() to avoid the
// 30-minute idle drain wait.
//
// This skips the graceful SIGTERM→5s→SIGKILL protocol used by Shutdown()
// because an idle process has no active sessions — there is nothing to
// drain or notify.
//
// We call package-level ForceKill(pgid) directly instead of proc.Manager.Kill().
// proc.Kill() is async-safe since #838 (cmd.Wait() moved off proc.mu), but
// ForceKill here remains preferable: it sends SIGKILL by PGID via syscall.Kill
// without ever acquiring proc.mu, guaranteeing zero interaction with the
// monitorProcess goroutine's concurrent Wait() — which is the path that owns
// the singleton's post-exit cleanup. This is defense-in-depth rather than a
// required workaround.
//
// NOTE: There is a benign TOCTOU window between the shouldKill check (under
// m.mu) and the ForceKill(pgid) call (after unlock). If the process exits
// in this window, ForceKill returns ESRCH, which is harmless and ignored.
func (m *CodexAppServerManager) KillIfIdle() {
	m.mu.Lock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	shouldKill := m.refs == 0 && m.state == stateRunning && m.pgid > 0
	pgid := m.pgid
	m.mu.Unlock()

	if shouldKill {
		m.log.Info("codex-app-server: killing idle process immediately", "pgid", pgid)
		_ = proc.ForceKill(pgid)
		proc.ForceKillTree(pgid, m.log)
		// monitorProcess will observe the exit, set state to stateIdle, and clean up.
	}
}

func (m *CodexAppServerManager) buildEnv() []string {
	return base.BuildEnv(worker.SessionInfo{}, EnvBlocklist, "codex-app-server")
}

var _ interface{ IsRunning() bool } = (*CodexAppServerManager)(nil)
