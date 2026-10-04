// Package worker defines the interfaces that all worker adapters must implement.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/pkg/events"
)

// ErrNotImplemented is returned for unimplemented worker methods.
var ErrNotImplemented = errors.New("worker: not implemented")

// ErrSkillNotSupported indicates that a Worker cannot resolve the requested
// Skill through its native or advertised command surface.
var ErrSkillNotSupported = errors.New("worker: skill not supported")

// ErrFellBackToFreshStart is returned by Resume when session files are missing
// and the worker falls back to a fresh Start(). Bridge callers use this to
// clear the "resumed" flag so forwardEvents applies normal retry logic.
var ErrFellBackToFreshStart = errors.New("worker: resume fell back to fresh start")

// ─── SessionConn ─────────────────────────────────────────────────────────────

// SessionConn represents the bidirectional communication channel between
// the gateway and a worker's runtime process. It is the data plane interface.
type SessionConn interface {
	// Send delivers a message to the worker runtime.
	Send(ctx context.Context, msg *events.Envelope) error

	// Recv returns a channel that yields messages from the worker runtime.
	// The channel is closed when the connection is closed.
	Recv() <-chan *events.Envelope

	// Close terminates the connection and releases resources.
	Close() error

	// UserID returns the user who owns this session.
	UserID() string

	// SessionID returns the session identifier.
	SessionID() string
}

// ─── Capabilities ───────────────────────────────────────────────────────────

// Capabilities describes what a worker adapter supports.
type Capabilities interface {
	// Type returns the worker type identifier (e.g. "claude_code", "opencode_server", "codex_cli", "acp").
	Type() WorkerType

	// SupportsResume returns true if the worker can resume a previous session.
	SupportsResume() bool

	// CanResumeTerminated returns true if the worker can resume a session
	// that is in TERMINATED state (orphan recovery). Workers using shared
	// singleton processes (e.g. CodexCLI) return false because the singleton
	// process was killed on release and the thread context is gone.
	CanResumeTerminated() bool

	// SupportsStreaming returns true if the worker emits streaming (delta) events.
	SupportsStreaming() bool

	// SupportsTools returns true if the worker exposes tool call capabilities.
	SupportsTools() bool

	// EnvBlocklist returns the set of environment variable names this worker
	// must NOT receive (empty = all allowed).
	EnvBlocklist() []string

	// SessionStoreDir returns the directory where the worker stores session state,
	// or empty string if the worker does not persist sessions.
	SessionStoreDir() string

	// MaxTurns returns the maximum number of turns (input/output cycles) allowed
	// per session, or 0 if unlimited.
	MaxTurns() int

	// Modalities returns the supported content modalities (e.g. "text", "code", "image").
	Modalities() []string
}

// WorkerType is the string identifier for a worker implementation.
type WorkerType string

const (
	TypeClaudeCode  WorkerType = "claude_code"
	TypeOpenCodeSrv WorkerType = "opencode_server"
	TypeCodexCLI    WorkerType = "codex_cli"
	TypeACP         WorkerType = "acp"
	TypeUnknown     WorkerType = "unknown"
)

// Environment profiles (#946 plan unit E).
const (
	// EnvProfileCompat is today's behaviour and the default: the worker
	// inherits the host environment minus a blocklist.
	EnvProfileCompat = "compat"
	// EnvProfileStrict starts from a minimal per-OS system allowlist. It
	// narrows the ENVIRONMENT only; it is not filesystem or network
	// isolation and must never be reported as such.
	EnvProfileStrict = "strict"
)

// SessionStartParams encapsulates the parameters for creating a new worker session.
// Introduced to reduce the risk of parameter ordering errors (e.g. botName vs botID)
// across the 13-parameter StartSession call chain.
type SessionStartParams struct {
	ID            string            // session ID (deterministic UUIDv5 or random)
	UserID        string            // owner of the session
	BotID         string            // platform runtime ID (e.g. "U12345"), for logging and session isolation
	BotName       string            // YAML config name (e.g. "my-bot"), for agent-config path resolution
	WorkerType    WorkerType        // worker adapter type
	AllowedTools  []string          // tool whitelist (nil = no restriction)
	WorkDir       string            // project directory for the worker
	Platform      string            // messaging platform ("slack", "feishu", "webchat", "")
	PlatformKey   map[string]string // platform-specific context (channel IDs, cron metadata, etc.)
	Title         string            // session title (display only)
	ClientKey     string            // client session key for persistence
	WorkspaceID   string            // WebChat 多租户锚（spec ①）；平台/cron 会话为空
	InjectExclude []string          // agent config files to skip from injection
}

// ─── Worker ─────────────────────────────────────────────────────────────────

// Worker is the main interface that all worker adapters must implement.
// The gateway communicates with a worker exclusively through this interface.
type Worker interface {
	Capabilities

	// Start launches the worker runtime for the given session.
	// It blocks until the runtime is ready to receive input or an error occurs.
	Start(ctx context.Context, session SessionInfo) error

	// Input delivers a user message to the worker runtime.
	Input(ctx context.Context, content string, metadata map[string]any) error

	// Resume reattaches to an existing session using the sessionID from session.Info.
	// Returns ErrSessionNotFound if the session cannot be located.
	Resume(ctx context.Context, session SessionInfo) error

	// Terminate gracefully stops the worker runtime.
	// It sends SIGTERM first, then SIGKILL after grace period.
	Terminate(ctx context.Context) error

	// Kill immediately terminates the worker runtime with SIGKILL.
	Kill() error

	// Wait blocks until the worker runtime exits, returning the exit code.
	Wait() (int, error)

	// Conn returns the SessionConn for this worker, or nil if not started.
	Conn() SessionConn

	// Health returns a snapshot of the worker's runtime health.
	Health() WorkerHealth

	// LastIO returns the time of the last I/O activity (input sent or output received).
	// Used by GC zombie detection to identify stuck workers.
	// Implementations that don't track I/O should return the zero time.Time.
	LastIO() time.Time

	// ResetContext clears the worker's runtime context.
	// The worker decides the implementation:
	//   - Per-process workers (Claude Code, Codex): terminate + restart process, return ConnReplaced=true.
	//   - In-place workers (OCS, ACP): reset via API/RPC, emit internal_reset event, return ConnReplaced=false.
	// Gateway reads ResetResult.ConnReplaced to decide whether to spawn a new forwardEvents goroutine.
	// Note: Gateway layer has already called sm.ClearContext() to clear SessionInfo.Context.
	ResetContext(ctx context.Context) (ResetResult, error)
	// StopCurrentTurn 停止 worker 当前正在执行的 turn，session 保持活跃。
	StopCurrentTurn(ctx context.Context) error
	// IsStopped returns true if the worker was stopped by user via StopCurrentTurn.
	IsStopped() bool
}

// PermissionCeilingReporter exposes the canonical effective permission ceiling
// captured by a Worker after a successful Start or Resume. Bridge persists this
// value as immutable session metadata before launching event forwarding.
type PermissionCeilingReporter interface {
	PermissionCeiling() (string, bool)
}

// MidTurnInjector is implemented by workers that can accept a user message
// mid-turn — injecting it into the currently running turn rather than starting
// a new one. Gateway probes this via type assertion at the SESSION_BUSY branch;
// workers that don't implement it fall back to pending-buffer replay.
//
// Implementations MUST NOT update crash-recovery lastInput: a mid-turn inject
// is supplemental, not the turn's primary input.
type MidTurnInjector interface {
	// InjectMidTurn delivers a user message into the active turn of the worker.
	// It must return an error if the turn is no longer active (caller falls back
	// to pending-buffer replay).
	InjectMidTurn(ctx context.Context, content string, metadata map[string]any) error
}

// InputDispatchAcknowledger is an optional two-phase input capability for
// adapters whose Input call can remain blocked after the provider has accepted
// the request. accepted MUST be called exactly when a successful stop can no
// longer overtake this input at the provider boundary. The callback must run
// at most once and before the method returns. The method may continue waiting
// for the turn result after that callback.
//
// Workers that do not implement this capability retain the Worker.Input
// contract: a successful return is treated as the acceptance point.
type InputDispatchAcknowledger interface {
	InputWithDispatchAccepted(
		ctx context.Context,
		content string,
		metadata map[string]any,
		accepted func(),
	) error
}

// DispatchInput invokes the strongest input-delivery contract exposed by w.
// The callback is never synthesized for an error: in that case the caller
// learns the outcome from the returned error instead.
func DispatchInput(
	ctx context.Context,
	w Worker,
	content string,
	metadata map[string]any,
	accepted func(),
) error {
	if accepted == nil {
		accepted = func() {}
	}
	if acknowledger, ok := w.(InputDispatchAcknowledger); ok {
		return acknowledger.InputWithDispatchAccepted(ctx, content, metadata, accepted)
	}
	if err := w.Input(ctx, content, metadata); err != nil {
		return err
	}
	accepted()
	return nil
}

// ResetResult describes the outcome of a ResetContext call.
// Gateway reads this to decide orchestration without knowing Worker internals.
type ResetResult struct {
	// ConnReplaced indicates whether the worker replaced its underlying connection.
	// true  = Worker restarted the process or rebuilt the connection (e.g. Claude Code, Codex CLI).
	//         Gateway must spawn a new forwardEvents goroutine.
	// false = Worker reset in-place without replacing the connection (e.g. OCS HTTP reset, ACP new session).
	//         The existing forwardEvents goroutine continues running.
	ConnReplaced bool
}

// WorkerHealth reports the runtime health of a worker process.
type WorkerHealth struct {
	Type      WorkerType `json:"type"`
	SessionID string     `json:"session_id"`
	PID       int        `json:"pid"`
	Running   bool       `json:"running"`
	Healthy   bool       `json:"healthy"`
	Uptime    string     `json:"uptime"`
	Error     string     `json:"error,omitempty"`

	// Agent discovery fields (populated by ACP and similar workers).
	AgentName       string `json:"agent_name,omitempty"`
	AgentVersion    string `json:"agent_version,omitempty"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
}

// InputRecoverer is an optional interface for session connections that cache
// the last user input for crash recovery re-delivery. Bridge detects
// implementations via type assertion to recover input after resume failure.
type InputRecoverer interface {
	LastInput() string
}

// EventInjector is an optional interface for SessionConn implementations
// that support injecting synthetic events into the Recv() stream.
// In-place-reset workers use this to emit internal_reset events that
// forwardEvents processes without client forwarding.
type EventInjector interface {
	Inject(env *events.Envelope)
}

// ResetGenerationer is an optional interface for workers that track a monotonic
// reset generation counter. forwardEvents captures the generation at goroutine
// start and checks after recvCh closes — if the current generation differs, the
// worker was reset by a NEW forwardEvents goroutine and this OLD goroutine must
// NOT cleanupCrashedWorker.
//
// Workers that return ResetResult{ConnReplaced:true} from ResetContext MUST
// implement this interface (typically via embedding BaseWorker). Failure to
// implement it causes the stale-goroutine guard to be silently skipped.
type ResetGenerationer interface {
	IncResetGeneration() int64
	LoadResetGeneration() int64
}

// RawExitCoder is an optional interface for workers whose Wait() returns a
// normalized exit code that differs from the raw OS exit code of their runtime.
// The Bridge uses it in crash diagnostics to surface the original code (e.g. a
// Windows NT status like 0xC0000142 STATUS_DLL_INIT_FAILED) alongside the
// normalized value, without changing Wait()'s contract.
//
// Workers whose Wait() already returns the raw process exit code (Claude Code,
// Codex CLI, ACP) do NOT implement this — for them the normalized value already
// is the raw value. Only workers that normalize (the OCS singleton maps every
// crash to 1) implement it.
type RawExitCoder interface {
	RawExitCode() (code int, ok bool)
}

// WorkerSessionIDHandler is an optional interface for workers that manage
// their own internal session IDs separate from the Gateway session ID.
// Bridge detects implementations via type assertion and uses them to
// persist/restore worker-internal session IDs for resume support.
type WorkerSessionIDHandler interface {
	SetWorkerSessionID(id string)
	GetWorkerSessionID() string
}

// SystemPromptUpdater is an optional interface for workers that support
// updating their stored system prompt at runtime (e.g. during /reset).
// Workers that do not implement this interface will continue using the
// system prompt from their initial Start() call.
// Bridge detects implementations via type assertion and calls this before
// ResetContext so the worker's next session uses the reloaded agent config.
type SystemPromptUpdater interface {
	UpdateSystemPrompt(prompt string)
}

// PermissionMode tiers (#789, r3 #804). The gateway maps each tier to the worker's
// native permission parameters at startup. Admin picks one per workspace. An empty
// workspace value means "no explicit override" — the bridge then injects its global
// default (config.worker.default_permission_mode, seeded "workspace" in r3) for
// workspace sessions, or "" for cron/platform sessions so they honor operator config.
// See resolveWorkspacePermissionMode for the full precedence.
// Mapping lives inside each worker (not a central table) — see claudecode/codexcli/
// opencodeserver/acp workers for the per-runtime translation.
const (
	PermissionModeReadOnly  = "read-only" // CC plan / Codex read-only×untrusted / OCS plan / ACP approve=false
	PermissionModeWorkspace = "workspace" // CC acceptEdits / Codex workspace-write×on-request / OCS acceptEdits / ACP approve=false
	PermissionModeAutoEdit  = "auto-edit" // CC auto / Codex workspace-write×never / OCS acceptEdits / ACP approve=true
	PermissionModeBypass    = "bypass"    // CC --dangerously-skip-permissions / Codex danger-full-access×never / OCS bypassPermissions / ACP approve=true
)

// ErrInvalidPermissionMode signals a permission mode that is not one of the 4 tiers.
var ErrInvalidPermissionMode = errors.New("invalid permission mode")

var validPermissionModes = map[string]struct{}{
	PermissionModeReadOnly:  {},
	PermissionModeWorkspace: {},
	PermissionModeAutoEdit:  {},
	PermissionModeBypass:    {},
}

// ValidatePermissionMode returns nil for the empty string ("worker default":
// CC/OCS apply bypass; Codex/ACP honor operator config) or a valid tier;
// otherwise it wraps ErrInvalidPermissionMode. Mirrors ValidateType.
func ValidatePermissionMode(mode string) error {
	if mode == "" {
		return nil
	}
	if _, ok := validPermissionModes[mode]; !ok {
		return fmt.Errorf("%w: %q not a valid tier (read-only|workspace|auto-edit|bypass)", ErrInvalidPermissionMode, mode)
	}
	return nil
}

// NormalizePermissionMode returns the effective tier for a (possibly empty) mode.
// Empty → PermissionModeWorkspace (the r3 Default() seed). Valid tiers pass through
// unchanged. Called by NewBridge/UpdateDefaultPermissionMode; consumed via Load by
// resolveWorkspacePermissionMode, which treats the result as a permissiveness CEILING
// — each worker clamps its operator config to never exceed it (codexcli takes the
// more-restrictive of session tier vs cfg.Sandbox/ApprovalMode), so an injected
// default can tighten but never escalate past operator config.
func NormalizePermissionMode(mode string) string {
	if mode == "" {
		return PermissionModeWorkspace
	}
	return mode
}

// SessionInfo contains metadata about a session needed by the worker to start/resume.
type SessionInfo struct {
	SessionID    string
	UserID       string
	ProjectDir   string
	Env          map[string]string
	Args         []string
	AllowedTools []string // tools allowed for this session (from InitConfig.AllowedTools)

	// WorkerSessionID is the internal session ID used by the worker runtime.
	// For workers that manage their own session state (OpenCode Server),
	// this field carries the worker-internal session ID for persistence and resume.
	// Empty for workers that use Gateway SessionID directly (Claude Code).
	WorkerSessionID string
	AllowedModels   []string // models allowed for this session

	// MCPConfig holds MCP server configuration as JSON content ({"mcpServers":{...}}).
	// Written to a temp file by buildCLIArgs and passed via --mcp-config.
	MCPConfig string
	// StrictMCPConfig restricts MCP servers to only those specified in MCPConfig (--strict-mcp-config).
	StrictMCPConfig bool
	// DisallowedTools lists tools that the worker should NOT use.
	DisallowedTools []string
	// SystemPrompt is appended to the worker's default system prompt (--append-system-prompt).
	SystemPrompt string
	// SystemPromptReplace, if non-empty, replaces the default system prompt entirely (--system-prompt).
	// Takes precedence over SystemPrompt when set.
	SystemPromptReplace string
	// ConfigEnv holds extra env vars from worker.environment config. These always
	// override values from os.Environ().
	ConfigEnv []string
	// ConfigBlocklist holds additional env var names from worker.env_blocklist config.
	// These are merged with the hardcoded per-worker blocklist in BuildEnv.
	ConfigBlocklist []string
	// EnvProfile selects how the worker process environment is built
	// (#946 E). "" and any unrecognized value mean EnvProfileCompat, which is
	// today's behaviour: inherit the host environment minus a blocklist.
	//
	// EnvProfileStrict starts from a minimal system allowlist instead. It
	// narrows the ENVIRONMENT only — it is not filesystem or network
	// isolation, and must never be reported as such.
	EnvProfile string
	// EnvAllowKeys names additional host variables a strict worker may
	// inherit, on top of the OS system allowlist.
	//
	// This is operator configuration, never client input: a request must not
	// be able to widen what the host environment leaks into a Worker process.
	EnvAllowKeys []string
	// PermissionMode controls how the worker handles permission requests (issue #789).
	// Valid values: PermissionModeReadOnly|Workspace|AutoEdit|Bypass. Empty = "worker
	// default" (CC/OCS apply bypass; Codex/ACP honor operator config); the bridge leaves
	// it empty when no explicit workspace override exists (resolveWorkspacePermissionMode).
	PermissionMode string
	// SkipPermissions bypasses all permission checks (equivalent to --dangerously-skip-permissions).
	SkipPermissions bool
	// Sandbox controls the codex CLI sandbox mode ("read-only", "workspace-write", "danger-full-access").
	// Empty = use config default. Per-bot override propagated from messaging config.
	Sandbox string
	// ACPCommand overrides the global ACP agent binary for this session.
	// Empty = use worker.acp.command config. Per-bot override propagated via platformKey.
	ACPCommand string
	// ContinueSession resumes the latest session in the current directory without a session ID.
	ContinueSession bool
	// ForkSession, when resuming, creates a new session ID instead of reusing the existing one.
	ForkSession bool
	// ResumeSessionAt restores the session up to and including the specified
	// assistant message ID, discarding later history (--resume-session-at).
	ResumeSessionAt string
	// ResumeSessionID is the worker-internal session ID for resuming a previous session.
	// For Codex CLI, this carries the thread ID for resume --last.
	ResumeSessionID string
	// MaxTurns limits the number of agentic turns in non-interactive mode.
	MaxTurns int
	// Bare runs Claude Code in minimal mode, skipping hooks, LSP, and plugin sync.
	Bare bool
	// AllowedDirs lists additional directories the worker can access (--add-dir).
	AllowedDirs []string
	// MaxBudgetUSD caps API spending per session (--max-budget-usd).
	MaxBudgetUSD float64
	// JSONSchema validates structured output against a JSON Schema (--json-schema).
	JSONSchema string
	// IncludeHookEvents exposes all hook lifecycle events in the output stream
	// (--include-hook-events).
	IncludeHookEvents bool
	// IncludePartialMessages exposes partial message blocks as they arrive
	// (--include-partial-messages).
	IncludePartialMessages bool
	// Images carries image file paths for Codex CLI --image flags.
	// Populated from SessionInfo by buildThreadStartParams. Per-session
	// injection through gateway/bridge is not yet wired.
	Images []string
	// ConversationHistory carries prior turns for pre-seeding a new worker
	// thread. Populated by the bridge when resuming/recreating a session
	// with existing history. Workers that support native resume (Claude Code
	// --resume) ignore this; workers that always create fresh threads
	// (CodexCLI) use it to inject context into the first user input.
	ConversationHistory []ConversationTurn
}

// ConversationTurn represents a single turn in a conversation history,
// used to seed context when a worker cannot natively resume its session.
type ConversationTurn struct {
	Role    string // "user" | "assistant"
	Content string
}

// SandboxPlatformKey is the platformKey map key used to propagate sandbox config
// from bridge/executor through session persistence to the worker.
const SandboxPlatformKey = "_sandbox"

// ACPCommandPlatformKey is the platformKey map key used to propagate per-bot
// ACP command override from messaging bridge through session persistence to the ACP worker.
const ACPCommandPlatformKey = "_acp_command"

// ForkSessionPlatformKey is the platformKey map key used to signal that a session
// should be forked from an existing session instead of resumed or created fresh.
// Expected value: "true" (string, checked via == "true" in bridge).
const ForkSessionPlatformKey = "_fork_session"

// JSONSchemaPlatformKey is the platformKey map key used to inject a JSON Schema
// for structured output into the first prompt of a session.
// Expected value: raw JSON Schema string (e.g. '{"type":"object","properties":{...}}').
const JSONSchemaPlatformKey = "_json_schema"
