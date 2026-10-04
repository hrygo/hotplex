package opencodeserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/internal/worker/proc"
	"github.com/hrygo/hotplex/pkg/events"
)

// SingletonProcessManager manages a single shared `opencode serve` process
// across all OpenCode Server sessions. The process is lazily started on first
// Acquire and shut down when the last session releases its reference.
//
// # Lifecycle
//
//	idle → starting → running → (crash → restarting → running) → stopped
//
// # Concurrency
//
// All methods are safe for concurrent use. Acquire serializes process startup
// via mutex so only the first caller starts the process.
type SingletonProcessManager struct {
	log       *slog.Logger
	client    *http.Client // with Timeout for API calls
	sseClient *http.Client // without Timeout for long-lived SSE streams
	cfg       config.OpenCodeServerConfig

	mu       sync.Mutex
	proc     *proc.Manager
	pgid     int // cached PGID for ForceKill outside proc.mu (see idle drain / Shutdown)
	httpAddr string
	refs     int
	state    singletonState
	crashCh  chan struct{} // closed when process exits unexpectedly

	// EventBus dispatches events from the global SSE stream to individual sessions.
	// eventMu serializes converter access with deferred retry expiry. Lock order:
	// mu (when held) → eventMu → retryTerminal.mu → busMu.
	eventMu     sync.Mutex
	busMu       sync.RWMutex
	subscribers map[string]chan *events.Envelope
	sseCancel   context.CancelFunc

	// Converter maps OCS BusEvents to AEP envelopes.
	converter     *Converter
	retryTerminal *retryTerminalArbiter

	idleTimer *time.Timer

	// Crash diagnostics (issue #900): preserve the raw OS exit code and a
	// bounded stderr tail so operators can distinguish a Windows NT status
	// (e.g. 0xC0000142 STATUS_DLL_INIT_FAILED) from the normalized Worker
	// exit code (1) surfaced to the Bridge. Guarded by s.mu.
	lastExitCode int
	hasExitCode  bool
	stderrRing   *ringBuffer
}

type singletonState int

const (
	stateIdle     singletonState = iota // no process
	stateStarting                       // process launching, waiting for health
	stateRunning                        // process serving requests
	stateStopped                        // gateway shutdown
)

// portRegex matches "opencode server listening on http://127.0.0.1:PORT".
var portRegex = regexp.MustCompile(`listening on http://[\d.]+:(\d+)`)

// stderrRingCapacity bounds how many recent stderr lines are retained for
// startup-failure diagnostics.
const stderrRingCapacity = 64

// NewSingletonProcessManager creates a new singleton process manager.
func NewSingletonProcessManager(log *slog.Logger, cfg config.OpenCodeServerConfig) *SingletonProcessManager {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	manager := &SingletonProcessManager{
		log:         log.With("component", "opencode-server-singleton"),
		client:      &http.Client{Timeout: cfg.HTTPTimeout, Transport: transport},
		sseClient:   &http.Client{Transport: transport}, // no Timeout for SSE
		cfg:         cfg,
		crashCh:     make(chan struct{}),
		subscribers: make(map[string]chan *events.Envelope),
		converter:   NewConverter(),
		stderrRing:  newRingBuffer(stderrRingCapacity),
	}
	manager.retryTerminal = newRetryTerminalArbiter(
		func(delay time.Duration, fn func()) retryTimer { return time.AfterFunc(delay, fn) })
	return manager
}

// Acquire increments the reference count and starts the process if needed.
// Returns the server HTTP address, HTTP client for API calls, HTTP client for SSE (no timeout),
// and a crash notification channel.
// The crash channel is closed when the process exits unexpectedly; workers should
// check it in their Wait() implementation to report the correct exit code.
func (s *SingletonProcessManager) Acquire(ctx context.Context) (httpAddr string, client, sseClient *http.Client, crashCh <-chan struct{}, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == stateStopped {
		return "", nil, nil, nil, fmt.Errorf("opencode-server-singleton: stopped")
	}

	// Cancel idle drain timer if one is pending.
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	// Start process on first reference.
	if s.state == stateIdle {
		if err := s.startProcessLocked(ctx); err != nil {
			return "", nil, nil, nil, err
		}
	}

	if s.state != stateRunning {
		return "", nil, nil, nil, fmt.Errorf("opencode-server-singleton: unexpected state %d", s.state)
	}

	s.refs++
	s.log.Debug("opencode-server-singleton: acquire", "refs", s.refs)
	return s.httpAddr, s.client, s.sseClient, s.crashCh, nil
}

// Release decrements the reference count. When refs reach zero, an idle drain
// timer starts. If no new Acquire arrives within idleDrainPeriod, the process
// is killed.
func (s *SingletonProcessManager) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.refs <= 0 {
		s.log.Warn("opencode-server-singleton: release with no active refs")
		return
	}

	s.refs--
	s.log.Debug("opencode-server-singleton: release", "refs", s.refs)

	if s.refs == 0 && s.state == stateRunning {
		s.startIdleDrainLocked()
	}
}

// Subscribe returns a channel that receives AEP events for the given session ID.
func (s *SingletonProcessManager) Subscribe(sessionID string) chan *events.Envelope {
	s.busMu.Lock()
	defer s.busMu.Unlock()

	if ch, ok := s.subscribers[sessionID]; ok {
		return ch
	}

	ch := make(chan *events.Envelope, 256)
	s.subscribers[sessionID] = ch
	s.log.Debug("opencode-server-singleton: subscribed", "session_id", sessionID)
	return ch
}

// Unsubscribe removes the subscription for the given session ID.
//
// Precondition: the caller MUST cancel the SSE context (sseCancel) before
// calling Unsubscribe to ensure the forwardBusEvents goroutine has exited.
// Failure to do so may leave a goroutine that sends to the removed channel
// (handled gracefully by trySendEnvelope's recover, but still a leak).
func (s *SingletonProcessManager) Unsubscribe(sessionID string) {
	s.eventMu.Lock()
	if s.retryTerminal != nil {
		s.retryTerminal.cancel(sessionID)
	}
	s.eventMu.Unlock()

	s.busMu.Lock()
	defer s.busMu.Unlock()

	if _, ok := s.subscribers[sessionID]; ok {
		// Do NOT close the channel here — sendCritical/sendToSubscriber may
		// still hold a reference and write to it concurrently. The forward-
		// BusEvents goroutine exits via ctx cancellation or closeAllSubscribers.
		delete(s.subscribers, sessionID)
		s.log.Debug("opencode-server-singleton: unsubscribed", "session_id", sessionID)
	}
}

// Shutdown forcefully terminates the process regardless of reference count.
func (s *SingletonProcessManager) Shutdown(ctx context.Context) {
	s.mu.Lock()

	s.state = stateStopped

	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	if s.sseCancel != nil {
		s.sseCancel()
	}

	if s.proc != nil {
		s.log.Info("opencode-server-singleton: shutdown, killing process")
		// Use ForceKill(pgid) + ForceKillTree directly rather than
		// proc.Manager.Kill(). proc.Kill() is now async-safe (#838), but the
		// direct call is kept as defense-in-depth: it avoids taking proc.mu
		// at all on this shutdown hot path and sidesteps any interaction
		// with monitorProcess's concurrent Wait(). s.pgid is always > 0 when
		// s.proc != nil (both are set together under s.mu in
		// startProcessLocked and cleared together in monitorProcess); the
		// else branch is defensive only.
		if s.pgid > 0 {
			_ = proc.ForceKill(s.pgid)
			proc.ForceKillTree(s.pgid, s.log)
		} else {
			s.log.Warn("opencode-server-singleton: shutdown found proc set but pgid==0; deferring cleanup to monitorProcess")
		}
		s.pgid = 0
		s.refs = 0
		// Do NOT clear s.proc: monitorProcess's Wait() owns pipe and Job
		// Object cleanup (closeLocked + closeJobHandle). Clearing it here
		// would skip Wait() and leak the Job handle on Windows.
	}

	s.mu.Unlock()

	// Close all active subscriptions outside s.mu to avoid lock nesting
	// with busMu (consistent with monitorProcess pattern).
	s.closeAllSubscribers()
}

// IsRunning reports whether the singleton process is currently running.
func (s *SingletonProcessManager) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateRunning
}

// PID returns the process ID, or 0 if not running.
func (s *SingletonProcessManager) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return 0
	}
	// proc.Manager doesn't expose PID directly; report 0 for now.
	// Health checks use IsRunning() instead.
	return 0
}

// --- internal ---

// LastExitCode returns the raw OS exit code of the most recent singleton
// process and whether one has been observed. The Bridge reads this via the
// RawExitCoder interface to tell a Windows NT status (e.g. 0xC0000142) apart
// from the normalized Worker exit code (1).
func (s *SingletonProcessManager) LastExitCode() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastExitCode, s.hasExitCode
}

// StderrTail returns a snapshot of the most recent captured stderr lines, for
// attaching to startup-failure diagnostics.
func (s *SingletonProcessManager) StderrTail() []string {
	return s.stderrRing.Lines()
}

// stderrTailMaxBytes bounds the total byte length of the stderr summary
// attached to startup-failure errors, preventing a runaway log line.
const stderrTailMaxBytes = 2048

// truncateTailBytes returns the last maxBytes bytes of s, backed up to a UTF-8
// rune boundary if the byte cut would split a multi-byte rune. Safe for
// arbitrary stderr content (CJK, emoji, localized panic output).
func truncateTailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// startupFailureError wraps err with the failing startup phase and a bounded
// tail of recent stderr so operators can diagnose OCS startup failures
// (spawn / discover port / health check) without grepping the gateway log at
// the exact timestamp.
func (s *SingletonProcessManager) startupFailureError(phase string, err error) error {
	tail := s.StderrTail()
	if len(tail) == 0 {
		return fmt.Errorf("opencode-server-singleton: %s: %w", phase, err)
	}
	summary := strings.Join(tail, "\n")
	if len(summary) > stderrTailMaxBytes {
		summary = truncateTailBytes(summary, stderrTailMaxBytes)
	}
	return fmt.Errorf("opencode-server-singleton: %s: %w (recent stderr: %s)", phase, err, summary)
}

// startProcessLocked starts the opencode serve process. Caller must hold s.mu.
func (s *SingletonProcessManager) startProcessLocked(ctx context.Context) error {
	s.state = stateStarting
	s.eventMu.Lock()
	s.converter.Reset()
	if s.retryTerminal != nil {
		s.retryTerminal.cancelAll()
	}
	s.eventMu.Unlock()
	s.stderrRing.Reset() // clear previous lifecycle's tail (pointer stays stable → no race with readStderr)
	s.lastExitCode = 0   // clear stale exit code from a previous lifecycle
	s.hasExitCode = false

	// Allocate an ephemeral port.
	port, err := s.allocatePort()
	if err != nil {
		s.state = stateIdle
		return fmt.Errorf("opencode-server-singleton: allocate port: %w", err)
	}

	args := []string{
		"serve",
		"--port", strconv.Itoa(port),
	}

	parts := strings.Fields(s.cfg.Command)
	if len(parts) == 0 {
		parts = []string{"opencode"}
	}
	binary := parts[0]
	fullArgs := make([]string, 0, len(parts)-1+len(args))
	fullArgs = append(fullArgs, parts[1:]...)
	fullArgs = append(fullArgs, args...)

	// Resolve the actual executable path so a Windows wrapper (.cmd shim) is
	// distinguishable from a native .exe in crash diagnostics. LookPath failure
	// is non-fatal — proc.Start will surface the real spawn error.
	resolved, lookErr := exec.LookPath(binary)
	if lookErr != nil {
		resolved = binary
	}
	commandSummary := binary + " " + strings.Join(fullArgs, " ")
	s.log.Info("opencode-server-singleton: startup phase",
		"phase", "spawn",
		"resolved_executable", resolved,
		"command", commandSummary,
		"port", port)

	env := s.buildEnv()
	s.proc = proc.New(proc.Opts{Logger: s.log})

	stdin, stdout, stderr, err := s.proc.Start(context.Background(), binary, fullArgs, env, "")
	if err != nil {
		s.proc = nil
		s.state = stateIdle
		s.log.Warn("opencode-server-singleton: startup phase failed", "phase", "spawn",
			"resolved_executable", resolved, "error", err)
		return s.startupFailureError("spawn", err)
	}
	_ = stdin

	// Continuously drain OCS stderr so errors (including HTTP 500 ref stacks)
	// are captured in the gateway log instead of trapped in the pipe.
	go s.readStderr(stderr)

	s.log.Info("opencode-server-singleton: startup phase",
		"phase", "discover_port", "resolved_executable", resolved, "pid", s.proc.PID())

	// Discover actual port from stdout (opencode serve prints it).
	actualPort, err := s.discoverPort(stdout, s.cfg.ReadyTimeout)
	if err != nil {
		pid := s.proc.PID() // capture before Kill sets proc=nil
		_ = s.proc.Kill()
		s.proc = nil
		s.state = stateIdle
		s.log.Warn("opencode-server-singleton: startup phase failed", "phase", "discover_port",
			"resolved_executable", resolved, "pid", pid, "error", err)
		return s.startupFailureError("discover port", err)
	}

	s.httpAddr = fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	// Wait for /health endpoint.
	s.log.Info("opencode-server-singleton: startup phase",
		"phase", "health_check", "addr", s.httpAddr, "pid", s.proc.PID())
	if err := s.waitForHealth(ctx); err != nil {
		_ = s.proc.Kill()
		s.proc = nil
		s.state = stateIdle
		s.log.Warn("opencode-server-singleton: startup phase failed", "phase", "health_check",
			"resolved_executable", resolved, "addr", s.httpAddr, "error", err)
		return s.startupFailureError("health check", err)
	}

	s.state = stateRunning
	s.pgid = s.proc.PGID()
	s.log.Info("opencode-server-singleton: startup phase",
		"phase", "running", "addr", s.httpAddr, "pid", s.proc.PID())

	// Monitor process exit in background.
	go s.monitorProcess()

	// Start global SSE reader for all sessions.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	s.sseCancel = sseCancel
	go s.readGlobalSSE(sseCtx)

	return nil
}

// allocatePort gets an OS-assigned ephemeral port by briefly opening a listener.
func (s *SingletonProcessManager) allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		return 0, fmt.Errorf("unexpected listener address type: %T", l.Addr())
	}
	_ = l.Close()
	return addr.Port, nil
}

// discoverPort reads stdout until finding the listening address line.
// Closes stdout after discovery since OCS communicates via HTTP, not stdout.
func (s *SingletonProcessManager) discoverPort(stdout *os.File, timeout time.Duration) (int, error) {
	type result struct {
		port int
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		defer func() { _ = stdout.Close() }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			s.log.Debug("opencode-server-singleton: stdout", "line", line)
			if m := portRegex.FindStringSubmatch(line); len(m) == 2 {
				p, err := strconv.Atoi(m[1])
				ch <- result{port: p, err: err}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("stdout read: %w", err)}
		} else {
			ch <- result{err: fmt.Errorf("stdout closed without port announcement")}
		}
	}()

	select {
	case r := <-ch:
		return r.port, r.err
	case <-time.After(timeout):
		// Close stdout to unblock the scanner goroutine on timeout.
		// The goroutine's defer will get os.ErrClosed, which is harmless.
		_ = stdout.Close()
		return 0, fmt.Errorf("timeout discovering port")
	}
}

// readStderr drains the opencode serve process stderr line by line into the
// gateway log. OCS writes internal errors and stack traces (referenced by the
// "ref" field in HTTP 500 responses) to stderr; previously this stream was
// discarded, making 500 errors opaque. The goroutine returns when stderr is
// closed by process exit.
func (s *SingletonProcessManager) readStderr(stderr *os.File) {
	defer func() { _ = stderr.Close() }()
	scanner := bufio.NewScanner(stderr)
	// OCS stack traces can exceed the default 64 KiB limit; raise the cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		s.stderrRing.Add(line)
		s.log.Warn("opencode-server-singleton: stderr", "line", line)
	}
}

// waitForHealth polls the /health endpoint until the server is ready.
func (s *SingletonProcessManager) waitForHealth(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.ReadyPollInterval)
	defer ticker.Stop()

	timeout := time.After(s.cfg.ReadyTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for server health after %v", s.cfg.ReadyTimeout)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, "GET", s.httpAddr+"/health", http.NoBody)
			if err != nil {
				continue
			}
			resp, err := s.client.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// monitorProcess waits for the process to exit and notifies subscribers.
func (s *SingletonProcessManager) monitorProcess() {
	// Capture the proc pointer under s.mu to avoid a data race with any
	// concurrent write to s.proc. Shutdown deliberately does NOT clear s.proc
	// (it leaves pipe/Job cleanup to this Wait() call), so pm is non-nil in
	// normal flow; the nil guard is defensive only.
	s.mu.Lock()
	pm := s.proc
	s.mu.Unlock()
	if pm == nil {
		return
	}
	code, _ := pm.Wait()
	_, hexCode := proc.FormatExitCode(code)

	s.mu.Lock()
	s.lastExitCode = code
	s.hasExitCode = true
	wasRunning := s.state == stateRunning
	refs := s.refs
	// Guard against overwriting stateStopped set by Shutdown: once stopped,
	// the monitor goroutine must not flip the singleton back to idle (which
	// would allow an unintended restart). Only Idle/Running transition to idle.
	if s.state != stateStopped {
		s.state = stateIdle
	}
	s.proc = nil
	s.pgid = 0

	// Cancel the global SSE reader so it doesn't leak into the next lifecycle.
	if s.sseCancel != nil {
		s.sseCancel()
		s.sseCancel = nil
	}

	// Notify crash subscribers if process died unexpectedly while sessions are active.
	if wasRunning && refs > 0 {
		s.log.Warn("opencode-server-singleton: process crashed",
			"exit_code", code, "exit_code_hex", hexCode, "refs", refs)
		close(s.crashCh)
		s.crashCh = make(chan struct{}) // new channel for next lifecycle
	} else {
		s.log.Info("opencode-server-singleton: process exited",
			"exit_code", code, "exit_code_hex", hexCode, "refs", refs)
	}
	s.mu.Unlock()

	// Close all subscriber channels outside s.mu to avoid lock nesting with busMu.
	if wasRunning {
		s.closeAllSubscribers()
	}
}

// startIdleDrainLocked starts a timer to kill the process when idle.
// Caller must hold s.mu.
func (s *SingletonProcessManager) startIdleDrainLocked() {
	s.log.Info("opencode-server-singleton: starting idle drain timer", "period", s.cfg.IdleDrainPeriod)
	s.idleTimer = time.AfterFunc(s.cfg.IdleDrainPeriod, func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.idleTimer == nil {
			// Timer was already stopped (e.g. by Shutdown or Acquire).
			return
		}

		if s.refs == 0 && s.state == stateRunning && s.pgid > 0 {
			s.log.Info("opencode-server-singleton: idle drain expired, killing process")
			// Cancel the SSE reader context BEFORE killing the process so the
			// reader sees ctx.Done() before the transport error (~40ms race)
			// and exits cleanly instead of logging a WARN reconnect attempt
			// against a dead process. monitorProcess also cancels sseCancel
			// in the normal exit path (it guards with nil check, so a double
			// cancel is harmless). See B4 in PR #891.
			if s.sseCancel != nil {
				s.sseCancel()
				s.sseCancel = nil
			}
			// Hold s.mu during the kill to close the TOCTOU window where a
			// concurrent Acquire could grab the doomed process and hand the
			// caller a httpAddr about to die. We call package-level
			// proc.ForceKill / ForceKillTree directly instead of
			// proc.Manager.Kill(): ForceKill sends SIGKILL by PGID via
			// syscall.Kill WITHOUT touching proc.mu, so it cannot deadlock
			// against monitorProcess's Wait() even under s.mu. (proc.Kill()
			// is async-safe since #838, but the direct call is lighter — no
			// proc.mu acquisition, no reap goroutine — and the singleton
			// cleanup is fully handled by monitorProcess.) See #836 / #838.
			_ = proc.ForceKill(s.pgid)
			proc.ForceKillTree(s.pgid, s.log)
		}
	})
}

// buildEnv creates the environment for the opencode serve process.
func (s *SingletonProcessManager) buildEnv() []string {
	// Process-scoped: one `opencode serve` backs every session, so the profile
	// comes from the worker config rather than from any session. Pretending a
	// per-session env exists here would be a claim nothing can back.
	env := base.BuildProcessEnv(
		s.cfg.EnvProfile, s.cfg.EnvAllowKeys, openCodeSrvEnvBlocklist, "opencode-server")
	env = append(env, "OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true")
	if s.cfg.Password != "" {
		env = append(env, "OPENCODE_SERVER_PASSWORD="+s.cfg.Password)
	}
	return env
}

// readGlobalSSE connects to the OCS global event stream and dispatches events to session channels.
func (s *SingletonProcessManager) readGlobalSSE(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("opencode-server-singleton: readGlobalSSE panic", "panic", r, "stack", string(debug.Stack()))
		}
		// Close all subscriber channels when the SSE reader exits for any reason.
		// For ctx.Done() (intentional shutdown), monitorProcess or Shutdown handles
		// this — but we do it here too as a safety net. For unexpected exits (max
		// reconnects, fatal errors), this is the ONLY notification subscribers get.
		s.closeAllSubscribers()
	}()

	s.mu.Lock()
	sseURL := s.httpAddr + "/global/event"
	s.mu.Unlock()

	var attempts int
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if attempts >= sseMaxReconnects {
			s.log.Error("opencode-server-singleton: SSE max reconnects exceeded", "attempts", attempts)
			return
		}

		req, err := http.NewRequestWithContext(ctx, "GET", sseURL, http.NoBody)
		if err != nil {
			s.log.Error("opencode-server-singleton: create SSE request", "err", err)
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		resp, err := s.sseClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempts++
			s.log.Warn("opencode-server-singleton: SSE connect error, reconnecting",
				"attempt", attempts, "err", err)
			s.sseBackoffSleep(ctx, attempts)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			attempts++
			s.log.Warn("opencode-server-singleton: SSE non-200 status, reconnecting",
				"status", resp.StatusCode, "attempt", attempts, "body", string(body))
			s.sseBackoffSleep(ctx, attempts)
			continue
		}

		s.log.Debug("opencode-server-singleton: global SSE connected", "url", sseURL)

		gotData := false
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				_ = resp.Body.Close()
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, io.EOF) {
					if gotData {
						attempts = 0
						s.log.Debug("opencode-server-singleton: global SSE stream ended, reconnecting")
					} else {
						attempts++
						s.log.Debug("opencode-server-singleton: global SSE empty stream, reconnecting with backoff",
							"attempt", attempts)
						s.sseBackoffSleep(ctx, attempts)
					}
					break
				}
				s.log.Warn("opencode-server-singleton: global SSE read error, reconnecting", "err", err)
				break
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			gotData = true
			attempts = 0
			data := strings.TrimPrefix(line, "data: ")

			// Parse and dispatch.
			s.dispatchOCSEvent([]byte(data))
		}
	}
}

// dispatchOCSEvent parses a raw OCS event and forwards the converted AEP envelopes
// to the appropriate session channel.
func (s *SingletonProcessManager) dispatchOCSEvent(data []byte) {
	var evt ocsGlobalEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return
	}

	if evt.Payload.Type == "sync" || evt.Payload.Type == "server.connected" ||
		evt.Payload.Type == "server.heartbeat" || evt.Payload.Type == "global.disposed" {
		return
	}

	// session.error may have no sessionID — route via directory or skip.
	var props struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(evt.Payload.Properties, &props); err != nil {
		return
	}
	sessionID := props.SessionID

	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.retryTerminal == nil {
		s.retryTerminal = newRetryTerminalArbiter(
			func(delay time.Duration, fn func()) retryTimer { return time.AfterFunc(delay, fn) })
	}

	if sessionID == "" {
		// session.error can have optional sessionID — dispatch to all subscribers.
		if evt.Payload.Type == ocsSessionError {
			s.retryTerminal.cancelAll()
			s.dispatchToAllSubscribers(evt.Payload.Properties)
		}
		return
	}

	if s.retryTerminal.deferEvent(sessionID, evt.Payload.Type, evt.Payload.Properties,
		func(token *pendingRetryTerminal) { s.flushRetryTerminal(sessionID, token) }) {
		return
	}

	// Delegate to converter.
	envs := s.converter.Convert(sessionID, evt.Payload.Type, evt.Payload.Properties)
	for _, env := range envs {
		s.sendToSubscriber(sessionID, env)
	}
}

func (s *SingletonProcessManager) flushRetryTerminal(sessionID string, token *pendingRetryTerminal) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()

	message, ok := s.retryTerminal.consumeExpired(sessionID, token)
	if !ok {
		return
	}
	for _, env := range s.converter.handleRetryExhausted(sessionID, message) {
		s.sendToSubscriber(sessionID, env)
	}
}

// sendToSubscriber delivers a single envelope to the session's channel.
// Critical events (Done, Error, State, PermissionRequest, etc.) use a blocking
// write with 5s timeout to guarantee delivery. Droppable events (MessageDelta,
// Raw) use non-blocking sends to avoid backpressure propagation.
func (s *SingletonProcessManager) sendToSubscriber(sessionID string, env *events.Envelope) {
	s.busMu.RLock()
	ch, ok := s.subscribers[sessionID]
	if !ok {
		s.busMu.RUnlock()
		return
	}

	if isDroppable(env.Event.Type) {
		select {
		case ch <- env:
		default:
			s.log.Warn("opencode-server-singleton: session channel full, dropping droppable event",
				"session_id", sessionID, "type", env.Event.Type)
		}
		s.busMu.RUnlock()
		return
	}

	// Critical event: block with timeout to guarantee delivery.
	s.busMu.RUnlock()
	s.sendCritical(ch, env, sessionID)
}

// dispatchToAllSubscribers sends session.error to every active subscriber.
// Releases busMu before writing to avoid blocking other dispatches.
//
// Design trade-off: after releasing busMu, the channel snapshot may race with
// concurrent closeAllSubscribers (shutdown). sendCritical's recover handles the
// resulting send-on-closed-channel panic, so delivery during shutdown is best-effort —
// session.error may be silently dropped. This is acceptable because the shutdown
// itself signals failure to callers via crashCh.
func (s *SingletonProcessManager) dispatchToAllSubscribers(props json.RawMessage) {
	s.busMu.RLock()
	type item struct {
		ch        chan *events.Envelope
		sessionID string
		envs      []*events.Envelope
	}
	var items []item
	for sessionID := range s.subscribers {
		envs := s.converter.Convert(sessionID, ocsSessionError, props)
		items = append(items, item{ch: s.subscribers[sessionID], sessionID: sessionID, envs: envs})
	}
	s.busMu.RUnlock()

	for _, it := range items {
		for _, env := range it.envs {
			s.sendCritical(it.ch, env, it.sessionID)
		}
	}
}

// trySendEnvelope attempts to send env to ch, recovering from send-on-closed-channel
// panics (TOCTOU race with conn/subscriber Close between the closed check and send).
// If block is true, waits up to timeout for the channel to become available.
// Returns true if the event was sent successfully.
func trySendEnvelope(ch chan *events.Envelope, env *events.Envelope, block bool, timeout time.Duration) (sent bool) {
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()
	if block {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case ch <- env:
			return true
		case <-timer.C:
			return false
		}
	}
	select {
	case ch <- env:
		return true
	default:
		return false
	}
}

// sendCritical performs a blocking write with timeout to guarantee critical
// event delivery. Delegates to trySendEnvelope for TOCTOU-safe send.
// Must NOT be called while holding busMu — the write may block.
func (s *SingletonProcessManager) sendCritical(ch chan *events.Envelope, env *events.Envelope, sessionID string) {
	if !trySendEnvelope(ch, env, true, criticalEventSendTimeout) {
		s.log.Debug("opencode-server-singleton: critical event send failed (channel closed or stuck)",
			"session_id", sessionID, "type", env.Event.Type)
	}
}

func (s *SingletonProcessManager) sseBackoffSleep(ctx context.Context, attempt int) {
	dur := min(sseBackoffInitial*time.Duration(1<<min(attempt, 5)), sseBackoffMax)
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}

type ocsGlobalEvent struct {
	Directory string `json:"directory"`
	Payload   struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	} `json:"payload"`
}

var (
	sseMaxReconnects  = 50
	sseBackoffInitial = 100 * time.Millisecond
	sseBackoffMax     = 10 * time.Second
)

// criticalEventSendTimeout is the maximum time to wait when sending a
// critical event (Done/Error/State) to a subscriber channel.
// Matches ACP safeSend and CodexCLI criticalEventSendTimeout.
const criticalEventSendTimeout = 5 * time.Second

// isDroppable reports whether an event kind can be silently dropped under
// backpressure. All streaming-append content (text delta, reasoning, raw) is
// droppable; the authoritative turns table reconciles on done.
// Keep in sync with gateway/hub.go isDroppable and acp/conn.go isDroppable.
func isDroppable(kind events.Kind) bool {
	return kind == events.MessageDelta || kind == events.Reasoning || kind == events.Raw
}

// closeAllSubscribers closes and removes all subscriber channels.
// This signals forwardBusEvents goroutines to exit (channel closed → !ok).
//
// Concurrency safety: may be called concurrently from up to 3 paths (readGlobalSSE
// defer, monitorProcess, Shutdown). This is safe because: (1) close(ch) on an
// already-closed channel panics, but busMu serializes map access so each channel
// is closed exactly once; (2) a second concurrent call sees an empty map (no-op).
//
// Invariant: after the process transitions to stopped, no new subscribers can be
// added because Acquire rejects stateStopped. This guarantees the map only shrinks.
func (s *SingletonProcessManager) closeAllSubscribers() {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.retryTerminal != nil {
		s.retryTerminal.cancelAll()
	}

	s.busMu.Lock()
	n := len(s.subscribers)
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
	s.busMu.Unlock()
	if n > 0 {
		s.log.Warn("opencode-server-singleton: closed all subscriber channels", "count", n)
	}
}

// --- package-level singleton ---

var singleton atomic.Pointer[SingletonProcessManager]

// InitSingleton initializes the global singleton process manager.
// Must be called during gateway startup before any sessions are created.
func InitSingleton(log *slog.Logger, cfg config.OpenCodeServerConfig) {
	mgr := NewSingletonProcessManager(log, cfg)
	singleton.Store(mgr)
}

// ShutdownSingleton shuts down the global singleton process manager.
// Must be called during gateway shutdown after bridge.Shutdown().
func ShutdownSingleton(ctx context.Context) {
	if m := singleton.Load(); m != nil {
		m.Shutdown(ctx)
		singleton.Store((*SingletonProcessManager)(nil))
	}
}
