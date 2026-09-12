package codexcli

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

func init() {
	worker.Register(worker.TypeCodexCLI, func() (worker.Worker, error) {
		s := GetSingleton()
		if s == nil {
			return nil, fmt.Errorf("codexcli: app-server singleton not initialized")
		}
		return &AppServerWorker{
			BaseWorker: base.NewBaseWorker(slog.Default(), nil),
			manager:    s,
		}, nil
	})
}

type Config struct {
	Command string
	Model   string
	// Sandbox mode: read-only, workspace-write, danger-full-access.
	// Default YOLO mode (danger-full-access) grants full filesystem + network access.
	// Existing deployments without explicit config will gain elevated permissions.
	Sandbox string
	// Approval mode: untrusted, on-request, never.
	// Default YOLO mode (never) skips all approval prompts — commands run immediately.
	ApprovalMode     string
	Ephemeral        bool
	Personality      string
	StartupTimeout   time.Duration
	Color            bool
	OutputFile       string
	StrictConfig     bool
	SkipGitRepoCheck bool
	IgnoreUserConfig bool
	IgnoreRules      bool
	LocalProvider    bool
	ConfigProfile    string
	BypassHookTrust  bool
}

func resolveConfig() Config {
	gc := GetConfig()
	return Config{
		Command:          gc.Command,
		Model:            gc.Model,
		Sandbox:          gc.Sandbox,
		ApprovalMode:     gc.ApprovalMode,
		Ephemeral:        gc.Ephemeral,
		Personality:      gc.Personality,
		StartupTimeout:   gc.StartupTimeout,
		Color:            gc.Color,
		OutputFile:       gc.OutputFile,
		StrictConfig:     gc.StrictConfig,
		SkipGitRepoCheck: gc.SkipGitRepoCheck,
		IgnoreUserConfig: gc.IgnoreUserConfig,
		IgnoreRules:      gc.IgnoreRules,
		LocalProvider:    gc.LocalProvider,
		ConfigProfile:    gc.ConfigProfile,
		BypassHookTrust:  gc.BypassHookTrust,
	}
}

// ─── AppServerWorker ──────────────────────────────────────────────────────

var _ worker.Worker = (*AppServerWorker)(nil)
var _ worker.WorkerCommander = (*AppServerWorker)(nil)
var _ worker.ControlRequester = (*AppServerWorker)(nil)
var _ worker.SkillInvoker = (*AppServerWorker)(nil)
var _ worker.SkillCatalogProvider = (*AppServerWorker)(nil)
var _ worker.SystemPromptUpdater = (*AppServerWorker)(nil)
var _ worker.PermissionCeilingReporter = (*AppServerWorker)(nil)
var _ worker.MidTurnInjector = (*AppServerWorker)(nil)

type appState int

const (
	appStateNew appState = iota
	appStateStarting
	appStateReady
	appStateTerminated
)

type AppServerWorker struct {
	*base.BaseWorker

	manager        *CodexAppServerManager
	threadID       string
	turnID         string
	userID         string
	crashSub       <-chan struct{}
	doneCh         chan struct{}
	mu             sync.Mutex
	recvCh         chan *events.Envelope
	commands       *ServerCommander
	closed         bool
	released       bool
	managerRefHeld bool
	state          appState
	sessionID      string
	conn           *appConn

	// origSession preserves the SessionInfo from the most recent Start()
	// call so that ResetContext can re-establish a fresh thread after cleanup.
	origSession worker.SessionInfo

	// pendingHistory stores ConversationHistory from SessionInfo for injection
	// into the first user input of a new thread. Cleared after injection.
	pendingHistory  []worker.ConversationTurn
	historyInjected bool

	permissionCeiling worker.PermissionCeiling
}

var (
	_ base.MetadataHandler                    = (*AppServerWorker)(nil)
	_ base.MultiAnswerQuestionResponseHandler = (*AppServerWorker)(nil)
)

// appConn implements worker.SessionConn for the app-server mode.
type appConn struct {
	userID     string
	sessionID  string
	recvCh     chan *events.Envelope
	recvGate   *base.EventGate
	mu         sync.Mutex
	closed     bool
	manager    *CodexAppServerManager
	lastReplay worker.InputReplay
}

var _ worker.InputReplayRecoverer = (*appConn)(nil)

// Send returns ErrNotImplemented because in app-server mode the manager
// handles all communication via JSON-RPC. Writing AEP envelopes directly
// to stdin would bypass the JSON-RPC protocol and break the codex process.
func (c *appConn) Send(ctx context.Context, msg *events.Envelope) error {
	return worker.ErrNotImplemented
}
func (c *appConn) LastInputReplay() worker.InputReplay {
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
func (c *appConn) setSkillReplay(invocation worker.NativeCommandInvocation) {
	c.mu.Lock()
	c.lastReplay = worker.InputReplay{Content: "/" + invocation.Name, Skill: &invocation}
	if invocation.Args != "" {
		c.lastReplay.Content += " " + invocation.Args
	}
	c.mu.Unlock()
}

// setTextReplay records an ordinary text input as the crash-recovery replay,
// replacing any earlier native Skill invocation. Without this update a crash
// after a plain-text turn would re-invoke the previous, now stale Skill.
func (c *appConn) setTextReplay(content string) {
	if content == "" {
		return
	}
	c.mu.Lock()
	c.lastReplay = worker.InputReplay{Content: content}
	c.mu.Unlock()
}
func (c *appConn) Recv() <-chan *events.Envelope { return c.recvCh }
func (c *appConn) eventGate() *base.EventGate {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Standalone connections own their channel. Managed connections are
	// constructed with their subscription's gate before being published.
	if c.recvGate == nil {
		c.recvGate = &base.EventGate{}
	}
	return c.recvGate
}
func (c *appConn) TrySend(env *events.Envelope) bool {
	return c.eventGate().TrySend(c.recvCh, env)
}
func (c *appConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.eventGate().Close(c.recvCh)
	return nil
}
func (c *appConn) UserID() string    { return c.userID }
func (c *appConn) SessionID() string { return c.sessionID }

func (w *AppServerWorker) Type() worker.WorkerType   { return worker.TypeCodexCLI }
func (w *AppServerWorker) SupportsResume() bool      { return true }
func (w *AppServerWorker) CanResumeTerminated() bool { return false }
func (w *AppServerWorker) SupportsStreaming() bool   { return true }
func (w *AppServerWorker) SupportsTools() bool       { return true }
func (w *AppServerWorker) EnvBlocklist() []string    { return EnvBlocklist }
func (w *AppServerWorker) SessionStoreDir() string   { return "" }
func (w *AppServerWorker) MaxTurns() int             { return 0 }
func (w *AppServerWorker) Modalities() []string      { return []string{"text", "code", "image"} }

func (w *AppServerWorker) PermissionCeiling() (string, bool) {
	return w.permissionCeiling.Mode()
}

func (w *AppServerWorker) SendControlRequest(ctx context.Context, subtype string, body map[string]any) (map[string]any, error) {
	if w.commands == nil {
		return nil, fmt.Errorf("codexcli: not started")
	}
	return w.commands.SendControlRequest(ctx, subtype, body)
}

func (w *AppServerWorker) Start(ctx context.Context, session worker.SessionInfo) error {
	w.mu.Lock()
	if w.state != appStateNew {
		w.mu.Unlock()
		return fmt.Errorf("codexcli: app-server already started (state=%d)", w.state)
	}
	w.state = appStateStarting

	if w.doneCh == nil {
		w.doneCh = make(chan struct{})
	}
	w.mu.Unlock()

	// RPC calls outside lock to avoid blocking other goroutines (P1 fix).
	crashCh, err := w.manager.Acquire(ctx)
	if err != nil {
		// Close doneCh so Wait() does not block on this error path.
		w.mu.Lock()
		w.closeAndMarkDone()
		w.mu.Unlock()
		return fmt.Errorf("codexcli: acquire: %w", err)
	}
	w.mu.Lock()
	w.crashSub = crashCh
	w.managerRefHeld = true
	w.mu.Unlock()

	if err := w.startNewThread(ctx, session, "start"); err != nil {
		w.releaseManagerReference()
		return err
	}
	w.mu.Lock()
	w.state = appStateReady
	w.mu.Unlock()
	return nil
}

func (w *AppServerWorker) Input(ctx context.Context, content string, metadata map[string]any) error {
	handled, err := base.DispatchMetadata(ctx, metadata, w)
	if err != nil {
		return err
	}
	if handled {
		w.SetLastIO(time.Now())
		return nil
	}

	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn != nil {
		// The ordinary text path must refresh the crash-recovery replay just
		// like InvokeSkill does; otherwise recovery after a plain-text turn
		// would re-invoke the previous stale Skill.
		conn.setTextReplay(content)
	}

	content = w.injectHistoryPrefix(content)
	return w.startTurn(ctx, []TurnInputItem{{Type: "text", Text: content}})
}

// InvokeSkill uses Codex app-server's structured Skill input item. The
// explicit item lets Codex resolve the Skill by path instead of making the
// model discover it from a marker; the accompanying text carries arguments.
func (w *AppServerWorker) InvokeSkill(ctx context.Context, invocation worker.SkillInvocation) error {
	if invocation.Name == "" || invocation.Path == "" {
		return fmt.Errorf("codexcli: skill name and path are required")
	}
	args := "$" + invocation.Name
	if invocation.Args != "" {
		args += " " + invocation.Args
	}
	args = w.injectHistoryPrefix(args)
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn != nil {
		conn.setSkillReplay(worker.NativeInvocationFromSkill(invocation))
	}
	return w.startTurn(ctx, []TurnInputItem{
		{Type: "skill", Name: invocation.Name, Path: invocation.Path},
		{Type: "text", Text: args},
	})
}

// ListInvokableSkills returns the authoritative Skill catalog for workDir via
// the app-server `skills/list` endpoint. Paths come from Codex itself, never
// guessed from the HotPlex filesystem layout. The manager must be running;
// callers treat a failure as "cannot confirm invokability".
func (w *AppServerWorker) ListInvokableSkills(ctx context.Context, workDir string) ([]worker.SkillDescriptor, error) {
	w.mu.Lock()
	mgr := w.manager
	w.mu.Unlock()
	if mgr == nil || !mgr.IsRunning() {
		return nil, fmt.Errorf("codexcli: app-server not running")
	}

	params := SkillsListParams{Cwds: []string{workDir}}
	resp, err := mgr.Call(ctx, "skills/list", params)
	if err != nil {
		return nil, fmt.Errorf("codexcli: skills/list: %w", err)
	}

	var result SkillsListResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("codexcli: parse skills/list: %w", err)
	}

	descriptors := make([]worker.SkillDescriptor, 0)
	for _, entry := range result.Data {
		for _, meta := range entry.Skills {
			if !meta.Enabled {
				continue
			}
			descriptors = append(descriptors, worker.SkillDescriptor{
				Name:        meta.Name,
				Description: meta.Description,
				Path:        meta.Path,
			})
		}
	}
	return descriptors, nil
}

func (w *AppServerWorker) startTurn(ctx context.Context, input []TurnInputItem) error {

	w.mu.Lock()
	tid := w.threadID
	w.mu.Unlock()
	if tid == "" {
		return fmt.Errorf("codexcli: app-server not started")
	}

	params := TurnStartParams{
		ThreadID: tid,
		Input:    input,
	}

	resp, err := w.manager.Call(ctx, "turn/start", params)
	if err != nil {
		var notStarted *writeNotStartedError
		if errors.As(err, &notStarted) {
			return fmt.Errorf("codexcli: turn/start: %w", err)
		}
		var responseErr *responseWaitError
		if errors.As(err, &responseErr) {
			return &worker.WorkerError{
				Kind:    worker.ErrKindTimeout,
				Message: fmt.Sprintf("codexcli: turn/start: %v", err),
				Cause:   err,
			}
		}
		// A stalled stdin write (singleton writeMu held by orphan goroutine)
		// wedges ALL codex sessions. Classify as Unavailable so the gateway
		// kills the app-server process and unblocks the goroutine (EPIPE).
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &worker.WorkerError{
				Kind:    worker.ErrKindUnavailable,
				Message: "codexcli: stdin write stalled (app-server not reading input)",
				Cause:   err,
			}
		}
		return fmt.Errorf("codexcli: turn/start: %w", err)
	}

	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(resp, &tr); err != nil || strings.TrimSpace(tr.Turn.ID) == "" {
		w.mu.Lock()
		w.turnID = ""
		w.mu.Unlock()
		// The request was written: malformed acknowledgement is not proof it
		// did not execute. Preserve the gateway's unknown-result fence.
		return &worker.WorkerError{
			Kind:    worker.ErrKindTimeout,
			Message: "codexcli: turn/start returned an invalid native turn identity",
			Cause:   errInvalidLifecycleAck,
		}
	}
	w.mu.Lock()
	w.turnID = tr.Turn.ID
	w.mu.Unlock()

	// A new primary turn began here: the turn/start RPC succeeded, so the new
	// turn is running. Clear the user-stop marker AFTER the successful send —
	// a failed send means the new turn never started, so the previous turn's
	// stopped marker must be preserved (the bridge crash fallback must not
	// re-run a stopped turn).
	w.BeginTurn()
	w.SetLastIO(time.Now())
	return nil
}

// ─── AppServerWorker lifecycle helpers ────────────────────────────────

// closeAndMarkDone closes doneCh and marks the worker as closed.
// This ensures Wait() is always unblocked even on error paths (P1 fix).
// Guarded by w.closed; safe after release() which sets w.closed=true under the same mutex.
//
// Unlike release() which closes doneCh but keeps it non-nil (Wait() needs
// to receive from a closed channel), this path additionally nils doneCh
// because it is only called on Start error paths (before Wait() is invoked)
// and resetLifecycleState() expects doneCh == nil to create a fresh channel.
// Caller must hold w.mu.
func (w *AppServerWorker) closeAndMarkDone() {
	if w.closed {
		return
	}
	w.closed = true
	if w.doneCh != nil {
		close(w.doneCh)
		w.doneCh = nil
	}
}

// cleanupOldThread closes the old connection and unsubscribes from the
// old thread. Shared by Resume and ResetContext.
func (w *AppServerWorker) cleanupOldThread(ctx context.Context) {
	w.mu.Lock()
	oldConn := w.conn
	oldThreadID := w.threadID
	w.recvCh = nil
	w.conn = nil
	w.pendingHistory = nil
	w.historyInjected = false
	w.mu.Unlock()

	if oldConn != nil {
		_ = oldConn.Close()
	}
	if oldThreadID != "" && w.manager != nil {
		_ = w.manager.Notify(ctx, "thread/unsubscribe", ThreadUnsubscribeParams{ThreadID: oldThreadID})
		w.manager.Unsubscribe(oldThreadID)
	}
}

// resetLifecycleState resets lifecycle state for a new thread attempt.
// After calling this, Terminate/Kill can release the new thread cleanly.
// Always creates a fresh doneCh: after release(), doneCh may be closed-but-non-nil
// (release() no longer nils it to fix #691), which would cause Wait() to return
// immediately instead of blocking for the new thread's lifecycle.
func (w *AppServerWorker) resetLifecycleState() {
	w.mu.Lock()
	w.closed = false
	w.released = false
	w.doneCh = make(chan struct{})
	w.mu.Unlock()
}

// startNewThread starts a fresh thread on the manager and wires up worker
// state. errPrefix is used in error messages for caller identification.
func (w *AppServerWorker) startNewThread(ctx context.Context, session worker.SessionInfo, errPrefix string) error {
	if w.manager != nil && !w.manager.IsRunning() {
		w.mu.Lock()
		w.closeAndMarkDone()
		w.mu.Unlock()
		return fmt.Errorf("codexcli: %s: manager process not running", errPrefix)
	}

	// Ensure the working directory exists before starting a thread bound to it.
	// codex runs as a shared app-server singleton (proc.Dir=""), so the workdir
	// is only passed via the JSON cwd field in buildThreadStartParams. If it does
	// not exist, codex receives a non-existent cwd and later turns fail — same
	// class of bug as opencode_server (see #863).
	if session.ProjectDir != "" {
		if err := os.MkdirAll(session.ProjectDir, 0o755); err != nil {
			return fmt.Errorf("codexcli: %s: create workdir: %w", errPrefix, err)
		}
	}

	cfg := resolveConfig()
	session = w.sessionWithPermissionCeiling(session)
	params := buildThreadStartParams(session, cfg)
	effectiveMode := permissionModeFromCodexEffective(params)
	if err := w.permissionCeiling.Capture(effectiveMode); err != nil {
		return fmt.Errorf("codexcli: %s: capture permission ceiling: %w", errPrefix, err)
	}

	resp, err := w.manager.Call(ctx, "thread/start", params)
	if err != nil {
		w.mu.Lock()
		w.closeAndMarkDone()
		w.mu.Unlock()
		return fmt.Errorf("codexcli: %s thread/start: %w", errPrefix, err)
	}

	var result ThreadStartResult
	if err := json.Unmarshal(resp, &result); err != nil || strings.TrimSpace(result.Thread.ID) == "" {
		w.mu.Lock()
		w.closeAndMarkDone()
		w.mu.Unlock()
		return fmt.Errorf("codexcli: %s invalid thread/start acknowledgement: %w", errPrefix, errInvalidLifecycleAck)
	}

	w.mu.Lock()
	w.threadID = result.Thread.ID
	w.turnID = ""
	w.sessionID = session.SessionID
	w.userID = session.UserID
	w.origSession = session
	sub := w.manager.subscribe(result.Thread.ID, session.SessionID)
	w.recvCh = sub.ch
	w.commands = NewServerCommander(w.manager, result.Thread.ID)
	w.manager.SetCurrentModel(result.Thread.ID, cfg.Model)
	w.conn = &appConn{
		userID:    session.UserID,
		sessionID: session.SessionID,
		recvCh:    w.recvCh,
		recvGate:  &sub.gate,
		manager:   w.manager,
	}
	w.StartTime = time.Now()
	w.SetLastIO(w.StartTime)
	w.pendingHistory = slices.Clone(session.ConversationHistory)
	w.historyInjected = false
	w.mu.Unlock()

	return nil
}

func (w *AppServerWorker) sessionWithPermissionCeiling(session worker.SessionInfo) worker.SessionInfo {
	if ceiling, ok := w.permissionCeiling.Mode(); ok {
		session.PermissionMode = ceiling
	}
	return session
}

// injectHistoryPrefix prepends conversation history to the first user
// input of a new thread. After injection, pendingHistory is cleared.
// A unique boundary ID is generated per injection call so that the
// sentinel markers cannot collide with real content, eliminating the
// need for destructive content sanitization.
func (w *AppServerWorker) injectHistoryPrefix(content string) string {
	w.mu.Lock()
	if w.historyInjected || len(w.pendingHistory) == 0 {
		w.mu.Unlock()
		return content
	}
	history := w.pendingHistory
	w.pendingHistory = nil
	w.historyInjected = true
	w.mu.Unlock()

	boundary := generateBoundaryID()

	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "CONVERSATION_HISTORY_%s_START\n", boundary)
	sb.WriteString("Below is the conversation history from a previous session. ")
	sb.WriteString("Use it as context to maintain continuity.\n\n")
	for _, turn := range history {
		switch turn.Role {
		case "user":
			sb.WriteString("[User]: ")
		case "assistant":
			sb.WriteString("[Assistant]: ")
		default:
			continue
		}
		sb.WriteString(turn.Content)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "CONVERSATION_HISTORY_%s_END\n", boundary)
	sb.WriteString("---\n\n")
	sb.WriteString(content)
	return sb.String()
}

// generateBoundaryID returns a cryptographically random 8-char hex string
// used to make history sentinel markers unique per injection call.
// Falls back to timestamp-based ID if the system entropy source is unavailable.
func generateBoundaryID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback: use timestamp-derived bytes. crypto/rand.Read only fails
		// in extreme sandbox environments; acceptable collision resistance for non-cryptographic use.
		binary.LittleEndian.PutUint32(buf[:], uint32(time.Now().UnixNano()))
	}
	return hex.EncodeToString(buf[:])
}

// ─── AppServerWorker lifecycle ────────────────────────────────────────

func (w *AppServerWorker) Resume(ctx context.Context, session worker.SessionInfo) error {
	// CodexCLI uses an ephemeral singleton process with no persistent
	// thread state — Resume on a fresh/terminated worker is semantically
	// identical to Start. Delegate to Start which handles Acquire + thread
	// creation, then report fallback so Bridge adjusts bookkeeping.
	//
	// Acquire/Release lifecycle: Start() calls Acquire() (increments ref
	// count on the singleton manager). Terminate/Kill calls release() which
	// calls Release() (decrements). Resume→Start re-acquires a slot, which
	// is correct — the old slot was released on Terminate.
	//
	// appStateNew is reachable when Bridge creates a fresh AppServerWorker
	// via the factory (init() + worker.Register) and then calls Resume()
	// instead of Start() — e.g., when startOrResumeOnInUse reuses a worker
	// that was registered but never started.
	w.mu.Lock()
	if w.state == appStateNew || w.state == appStateTerminated {
		// Reset to appStateNew so Start()'s guard (state != appStateNew) passes.
		// For terminated workers, release() already cleaned up manager refs;
		// resetLifecycleState re-creates the doneCh channel.
		w.state = appStateNew
		w.mu.Unlock()
		w.resetLifecycleState()
		if err := w.Start(ctx, session); err != nil {
			return err
		}
		return worker.ErrFellBackToFreshStart
	}
	w.state = appStateStarting
	w.mu.Unlock()

	w.cleanupOldThread(ctx)
	w.resetLifecycleState()
	if err := w.startNewThread(ctx, session, "resume"); err != nil {
		return err
	}
	w.mu.Lock()
	w.state = appStateReady
	w.mu.Unlock()
	return nil
}

func (w *AppServerWorker) Terminate(ctx context.Context) error {
	err := w.shutdown(ctx)
	w.mu.Lock()
	w.state = appStateTerminated
	w.mu.Unlock()
	return err
}

// StopCurrentTurn stops the current running turn on codex app-server using InterruptTurn.
func (w *AppServerWorker) StopCurrentTurn(ctx context.Context) error {
	w.mu.Lock()
	tid := w.threadID
	turnID := w.turnID
	w.mu.Unlock()
	if tid == "" || turnID == "" {
		return nil
	}
	w.MarkStopped()
	if err := w.manager.InterruptTurn(ctx, tid, turnID); err != nil {
		// No successful interrupt acknowledgement was observed. Roll back the
		// local stop marker with the gateway fence; a transport timeout can
		// still be an unknown remote outcome, not permission for blind replay.
		w.ClearStopped()
		return err
	}
	return nil
}

// InjectMidTurn steers the active turn with supplemental user input via the
// app-server turn/steer RPC. Unlike Input (turn/start), it does not start a
// new turn and does not update lastInput. Returns an error if no turn is
// active so the gateway can fall back to pending-buffer replay.
//
// SteerTurn is the first caller of the already-implemented manager RPC
// (manager.go:1093); this method is the SESSION_BUSY mid-turn wiring point
// probed by the gateway via the worker.MidTurnInjector interface assertion.
// Lock pattern mirrors StopCurrentTurn: snapshot threadID/turnID under w.mu,
// release the lock, empty-check, then call SteerTurn outside the lock. Does
// NOT call SetLastIO — a mid-turn inject is supplemental, not the turn's
// primary input (crash recovery must not replay it).
func (w *AppServerWorker) InjectMidTurn(ctx context.Context, content string, metadata map[string]any) error {
	w.mu.Lock()
	tid := w.threadID
	turnID := w.turnID
	w.mu.Unlock()
	if tid == "" || turnID == "" {
		return fmt.Errorf("codexcli: no active turn to steer")
	}
	_, err := w.manager.SteerTurnContext(ctx, tid, turnID, content)
	return err
}

func (w *AppServerWorker) Kill() error {
	ctx, cancel := context.WithTimeout(context.Background(), codexWriteFallbackGrace)
	defer cancel()
	err := w.shutdown(ctx)
	w.mu.Lock()
	w.state = appStateTerminated
	w.mu.Unlock()
	return err
}

// shutdown releases the worker's manager subscription and decrements the
// singleton ref count. Unlike per-session workers, the shared AppServer
// singleton process is NOT killed here — it stops via idle drain or
// explicit ShutdownSingleton(). This prevents GC from killing a shared
// process when reclaiming sessions.
func (w *AppServerWorker) shutdown(ctx context.Context) error {
	return w.release(ctx)
}

func (w *AppServerWorker) Wait() (int, error) {
	w.mu.Lock()
	crashSub := w.crashSub
	doneCh := w.doneCh
	w.mu.Unlock()

	if doneCh == nil && crashSub == nil {
		return 0, nil
	}
	select {
	case <-crashSub:
		if w.manager != nil {
			return w.manager.CrashExitCode(), nil
		}
		return 1, nil
	case <-doneCh:
		return 0, nil
	}
}

// releaseManagerReference separates resource ownership from connection state.
// A reset error may close the connection without releasing its acquired slot.
func (w *AppServerWorker) releaseManagerReference() {
	w.mu.Lock()
	held, mgr := w.managerRefHeld, w.manager
	w.managerRefHeld = false
	w.mu.Unlock()
	if held && mgr != nil {
		mgr.Release()
	}
}

func (w *AppServerWorker) release(ctx context.Context) error {
	w.mu.Lock()
	if w.released {
		w.mu.Unlock()
		return nil
	}
	w.released = true
	wasClosed := w.closed
	w.closed = true
	doneCh := w.doneCh
	tid := w.threadID
	conn := w.conn
	mgr := w.manager
	w.mu.Unlock()

	if doneCh != nil && !wasClosed {
		// Close but do NOT nil: Wait() relies on receiving from a closed
		// channel (returns immediately). closeAndMarkDone() additionally
		// nils for error-path reuse via resetLifecycleState().
		close(doneCh)
	}

	var unsubscribeErr error
	if mgr != nil && tid != "" {
		unsubscribeErr = mgr.Notify(ctx, "thread/unsubscribe", ThreadUnsubscribeParams{
			ThreadID: tid,
		})
		mgr.Unsubscribe(tid)
		// Close recvCh so forwardEvents exits its range loop.
		// Must happen after Unsubscribe (removes from dispatch map) to
		// avoid racing with in-flight dispatchNotification sends.
		if conn != nil {
			_ = conn.Close()
		}
	}
	w.releaseManagerReference()
	return unsubscribeErr
}

func (w *AppServerWorker) ResetContext(ctx context.Context) (worker.ResetResult, error) {
	// Increment reset generation so the OLD forwardEvents goroutine can detect
	// it's stale and skip cleanupCrashedWorker (which would detach the NEW worker).
	w.IncResetGeneration()

	w.mu.Lock()
	origSess := w.origSession
	w.mu.Unlock()

	w.cleanupOldThread(ctx)
	w.resetLifecycleState()

	// If the process crashed between turns, bail out — bridge will Terminate+Start.
	if origSess.SessionID == "" {
		w.mu.Lock()
		w.closeAndMarkDone()
		w.mu.Unlock()
		return worker.ResetResult{ConnReplaced: true}, nil
	}
	origSess.ConversationHistory = nil // /reset clears context, do not re-inject old history
	if err := w.startNewThread(ctx, origSess, "reset"); err != nil {
		return worker.ResetResult{}, err
	}
	return worker.ResetResult{ConnReplaced: true}, nil
}

func (w *AppServerWorker) Conn() worker.SessionConn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

func (w *AppServerWorker) Health() worker.WorkerHealth {
	return w.BaseWorker.Health(worker.TypeCodexCLI)
}

// UpdateSystemPrompt updates the stored origSession.SystemPrompt so that
// the next ResetContext uses the reloaded agent config.
func (w *AppServerWorker) UpdateSystemPrompt(prompt string) {
	w.mu.Lock()
	w.origSession.SystemPrompt = prompt
	w.mu.Unlock()
}

func (w *AppServerWorker) LastIO() time.Time {
	return w.BaseWorker.LastIO()
}

func (w *AppServerWorker) HandlePermissionResponse(ctx context.Context, reqID string, allowed bool, reason string) error {
	result, err := w.manager.PermissionResponseResult(reqID, allowed, reason)
	if err != nil {
		return err
	}
	return w.manager.RespondServerRequest(ctx, reqID, result)
}

func (w *AppServerWorker) HandleQuestionResponse(ctx context.Context, reqID string, answers map[string]string) error {
	result, err := w.manager.QuestionResponseResult(reqID, answers)
	if err != nil {
		return err
	}
	return w.manager.RespondServerRequest(ctx, reqID, result)
}

func (w *AppServerWorker) HandleQuestionResponseOptions(ctx context.Context, reqID string, answers map[string][]string, _ []string) error {
	result, err := w.manager.QuestionResponseOptionsResult(reqID, answers)
	if err != nil {
		return err
	}
	return w.manager.RespondServerRequest(ctx, reqID, result)
}

func (w *AppServerWorker) HandleElicitationResponse(ctx context.Context, reqID, action string, content map[string]any) error {
	result := map[string]any{
		"action":  action,
		"content": content,
	}
	return w.manager.RespondServerRequest(ctx, reqID, result)
}

func (w *AppServerWorker) Compact(ctx context.Context, args map[string]any) error {
	w.mu.Lock()
	tid := w.threadID
	w.mu.Unlock()
	if tid == "" {
		return fmt.Errorf("codexcli: no active thread")
	}
	_, err := w.manager.CompactThreadContext(ctx, tid)
	if err != nil {
		return fmt.Errorf("codexcli: compact: %w", err)
	}
	return nil
}

func (w *AppServerWorker) Clear(ctx context.Context) error {
	w.mu.Lock()
	tid := w.threadID
	turnID := w.turnID
	w.mu.Unlock()
	if tid == "" {
		return fmt.Errorf("codexcli: no active thread")
	}
	// Only interrupt when a turn is in flight; turn/interrupt requires a turnId.
	if turnID != "" {
		_ = w.manager.InterruptTurn(ctx, tid, turnID)
	}
	// ConnReplaced is ignored: Clear is invoked over WorkerCommander (HTTP REST),
	// bridge doesn't restart forwardEvents on Clear.
	_, err := w.ResetContext(ctx)
	return err
}

// Rewind drops turns from the end of the thread. targetID is interpreted as the
// number of turns to drop (parseable uint); empty or invalid defaults to 1.
func (w *AppServerWorker) Rewind(ctx context.Context, targetID string) error {
	w.mu.Lock()
	tid := w.threadID
	w.mu.Unlock()
	if tid == "" {
		return fmt.Errorf("codexcli: no active thread")
	}
	numTurns := uint32(1)
	if targetID != "" {
		if n, perr := strconv.ParseUint(targetID, 10, 32); perr == nil && n >= 1 {
			numTurns = uint32(n)
		}
	}
	_, err := w.manager.RollbackThreadContext(ctx, tid, numTurns)
	if err != nil {
		return fmt.Errorf("codexcli: rewind: %w", err)
	}
	return nil
}

// sandboxFromSession returns the session-level sandbox override if set,
// otherwise falls back to the config default.
func sandboxFromSession(session worker.SessionInfo, defaultSandbox string) string {
	if session.Sandbox != "" {
		return session.Sandbox
	}
	return defaultSandbox
}

// permissionModeFromSession maps a PermissionMode tier to Codex's (sandbox, approval)
// pair. Returns ok=false for empty/unknown so the caller falls back to config defaults
// (preserving the YOLO default for sessions without a workspace, issue #789).
func permissionModeFromSession(mode string) (sandbox, approval string, ok bool) {
	switch mode {
	case worker.PermissionModeReadOnly:
		return "read-only", "untrusted", true
	case worker.PermissionModeWorkspace:
		return "workspace-write", "on-request", true
	case worker.PermissionModeAutoEdit:
		return "workspace-write", "never", true
	case worker.PermissionModeBypass:
		return "danger-full-access", "never", true
	default:
		return "", "", false
	}
}

// codexSandboxRank returns a restrictiveness rank for a Codex sandbox mode
// (higher = more restrictive). Unknown values return 0 so a recognized session
// tier is always preferred over an unrecognized operator value.
func codexSandboxRank(s string) int {
	switch s {
	case "read-only":
		return 3
	case "workspace-write":
		return 2
	case "danger-full-access":
		return 1
	}
	return 0
}

// codexApprovalRank returns a restrictiveness rank for a Codex approval policy
// (higher = more restrictive). Unknown values return 0.
func codexApprovalRank(s string) int {
	switch s {
	case "untrusted":
		return 4
	case "on-request":
		return 3
	case "on-failure":
		return 2
	case "never":
		return 1
	}
	return 0
}

// permissionModeFromCodexEffective converts the actual thread/start sandbox and
// approval pair back to a conservative unified permission tier for persistence.
// Unknown/custom sandboxes fail closed to read-only.
func permissionModeFromCodexEffective(params map[string]any) string {
	sandbox, _ := params["sandbox"].(string)
	approval, _ := params["approvalPolicy"].(string)
	switch sandbox {
	case "read-only":
		return worker.PermissionModeReadOnly
	case "workspace-write":
		switch approval {
		case "never":
			return worker.PermissionModeAutoEdit
		case "on-request":
			return worker.PermissionModeWorkspace
		default:
			return worker.PermissionModeReadOnly
		}
	case "danger-full-access":
		if approval == "never" {
			return worker.PermissionModeBypass
		}
		return worker.PermissionModeReadOnly
	default:
		return worker.PermissionModeReadOnly
	}
}

// buildThreadStartParams constructs the JSON-RPC params for "thread/start".
// Shared by Start() and ResetContext() to avoid duplication.
//
// Default YOLO mode: danger-full-access sandbox + never approval.
// DangerFullAccess grants full network + filesystem access (Codex source).
// ⚠ This is a security-sensitive default — deployments should explicitly
// set sandbox to workspace-write or read-only for restricted environments.
func buildThreadStartParams(session worker.SessionInfo, cfg Config) map[string]any {
	approvalMode := cfg.ApprovalMode
	sandbox := sandboxFromSession(session, cfg.Sandbox)
	// SkipPermissions: legacy hard-bypass escape hatch. In codex it only forces
	// approval=never; a non-empty PermissionMode below takes precedence (asymmetric
	// with claudecode, where SkipPermissions is top priority). Production never sets
	// SkipPermissions (bridge injects only PermissionMode), so the two never co-occur
	// in practice and the asymmetry is theoretical. Documented per #789 P3.
	if session.SkipPermissions {
		approvalMode = "never"
	}
	// #789 / r3 #804 review: a non-empty PermissionMode tier (workspace override or
	// bridge-injected default) is a CEILING on permissiveness — it tightens the
	// operator's config but never relaxes it. Take the more-restrictive of (operator
	// config, session tier) per axis, so injecting "workspace" cannot override an
	// operator's read-only sandbox (the prior unconditional overwrite was an
	// escalation: it clobbered any cfg.Sandbox stricter than workspace-write).
	if sb, ap, ok := permissionModeFromSession(session.PermissionMode); ok {
		if codexSandboxRank(sb) > codexSandboxRank(sandbox) {
			sandbox = sb
		}
		if codexApprovalRank(ap) > codexApprovalRank(approvalMode) {
			approvalMode = ap
		}
	}
	params := map[string]any{
		"cwd":            session.ProjectDir,
		"sandbox":        sandbox,
		"personality":    cfg.Personality,
		"approvalPolicy": approvalMode,
	}
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.Ephemeral {
		params["ephemeral"] = true
	}
	if len(session.Images) > 0 {
		params["images"] = session.Images
	}
	if session.JSONSchema != "" {
		params["outputSchema"] = session.JSONSchema
	}
	if len(session.AllowedDirs) > 0 {
		params["additionalDirectories"] = session.AllowedDirs
	}
	if cfg.Color {
		params["color"] = true
	}
	if cfg.StrictConfig {
		params["strictConfig"] = true
	}
	if cfg.SkipGitRepoCheck {
		params["skipGitRepoCheck"] = true
	}
	if cfg.IgnoreRules {
		params["ignoreRules"] = true
	}
	if cfg.IgnoreUserConfig {
		params["ignoreUserConfig"] = true
	}
	if cfg.LocalProvider {
		params["localProvider"] = true
	}
	if cfg.BypassHookTrust {
		params["bypassHookTrust"] = true
	}
	if cfg.OutputFile != "" {
		params["outputFile"] = cfg.OutputFile
	}
	if cfg.ConfigProfile != "" {
		params["profile"] = cfg.ConfigProfile
	}
	// Agent-configs injection: bridge.injectAgentConfig() writes the merged
	// B/C channel prompt into session.SystemPrompt. Forward it as baseInstructions
	// so codex app-server uses it as the system prompt base.
	if session.SystemPrompt != "" {
		params["baseInstructions"] = session.SystemPrompt
	}
	// developerInstructions is reserved for future use — higher priority than
	// baseInstructions. Do not set until SessionInfo gains a corresponding field.
	return params
}
