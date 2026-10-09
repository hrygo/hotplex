// Package session implements the session manager with SQLite persistence,
// state machine, and background GC.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"

	"github.com/hrygo/hotplex/internal/agentspec"
	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// Errors returned by the session manager.
var (
	ErrSessionNotFound       = errors.New("session: not found")
	ErrSessionNotActive      = errors.New("session: not active")
	ErrSessionBusy           = errors.New("session: busy")
	ErrInvalidTransition     = errors.New("session: invalid state transition")
	ErrPoolExhausted         = errors.New("session: pool exhausted")
	ErrUserQuotaExceeded     = errors.New("session: user quota exceeded")
	ErrOwnershipMismatch     = errors.New("session: ownership mismatch")
	ErrMaxTurnsReached       = errors.New("session: max turns reached")
	ErrWorkerAttached        = errors.New("session: worker already attached")
	ErrClientKeyTooLong      = errors.New("session: client_key too long")
	ErrSessionCleanupPending = errors.New("session: cleanup pending")
	ErrCleanupLeaseLost      = errors.New("session: cleanup task lease lost")
)

// MaxClientKeyLen is the maximum allowed length for ClientKey.
const MaxClientKeyLen = 256

// Atomic gauge state for ObservableGauge callbacks.
// These track counts that were previously GaugeVec.Inc/Dec in promauto.
var (
	sessionsActiveByState sync.Map // map[string]*atomic.Int64 — key is state name
	workersRunningByType  sync.Map // map[string]*atomic.Int64 — key is worker_type
	lifecycleGCGauges     lifecycleGCMetricsSnapshot
)

type lifecycleGCMetricsSnapshot struct {
	eligibleBacklog      atomic.Int64
	blockedBacklog       atomic.Int64
	unknownExecutionHold atomic.Int64
	eligibleLagNanos     atomic.Int64
	blockedLagNanos      atomic.Int64
}

// sessionsActiveGauge tracks the count of sessions per state.
func sessionsActiveGauge(key string) *atomic.Int64 {
	v, _ := sessionsActiveByState.LoadOrStore(key, &atomic.Int64{})
	g, _ := v.(*atomic.Int64)
	return g
}

// workersRunningGauge tracks the count of running workers per type.
func workersRunningGauge(key string) *atomic.Int64 {
	v, _ := workersRunningByType.LoadOrStore(key, &atomic.Int64{})
	g, _ := v.(*atomic.Int64)
	return g
}

func init() {
	observability.RegisterGaugeCallbacks(func(m metric.Meter) {
		sessionGauge, err := m.Int64ObservableGauge(
			"hotplex.session.active",
			metric.WithDescription("Number of active sessions by state"),
		)
		if err != nil {
			slog.Warn("session: failed to create session.active gauge", "err", err)
			return
		}
		_, _ = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
			sessionsActiveByState.Range(func(key, val any) bool {
				k, _ := key.(string)
				v, _ := val.(*atomic.Int64)
				o.ObserveInt64(sessionGauge, v.Load(), metric.WithAttributes(attribute.String("state", k)))
				return true
			})
			return nil
		}, sessionGauge)

		workerGauge, err := m.Int64ObservableGauge(
			"hotplex.worker.running",
			metric.WithDescription("Number of currently running workers"),
		)
		if err != nil {
			slog.Warn("session: failed to create worker.running gauge", "err", err)
			return
		}
		_, _ = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
			workersRunningByType.Range(func(key, val any) bool {
				k, _ := key.(string)
				v, _ := val.(*atomic.Int64)
				o.ObserveInt64(workerGauge, v.Load(), metric.WithAttributes(attribute.String("worker_type", k)))
				return true
			})
			return nil
		}, workerGauge)

		backlogGauge := observability.LifecycleGCBacklog()
		unknownBlockedGauge := observability.LifecycleGCUnknownBlocked()
		oldestLagGauge := observability.LifecycleGCOldestLag()
		_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
			o.ObserveInt64(
				backlogGauge,
				lifecycleGCGauges.eligibleBacklog.Load(),
				metric.WithAttributes(attribute.String("status", "eligible")),
			)
			o.ObserveInt64(
				backlogGauge,
				lifecycleGCGauges.blockedBacklog.Load(),
				metric.WithAttributes(attribute.String("status", "blocked")),
			)
			o.ObserveInt64(unknownBlockedGauge, lifecycleGCGauges.unknownExecutionHold.Load())
			o.ObserveFloat64(
				oldestLagGauge,
				float64(lifecycleGCGauges.eligibleLagNanos.Load())/float64(time.Second),
				metric.WithAttributes(attribute.String("status", "eligible")),
			)
			o.ObserveFloat64(
				oldestLagGauge,
				float64(lifecycleGCGauges.blockedLagNanos.Load())/float64(time.Second),
				metric.WithAttributes(attribute.String("status", "blocked")),
			)
			return nil
		}, backlogGauge, unknownBlockedGauge, oldestLagGauge)
		if err != nil {
			slog.Warn("session: failed to register lifecycle GC gauges", "err", err)
		}
	})
}

// Manager orchestrates session lifecycle, persistence, and GC.
type Manager struct {
	log      *slog.Logger
	store    Store
	cfg      *config.Config
	cfgStore *config.ConfigStore // hot-reloadable config; nil = use static cfg
	pool     *PoolManager

	mu       sync.RWMutex
	sessions map[string]*managedSession

	// runningIndex tracks RUNNING session IDs for O(1) zombie scan.
	// Protected by riMu (independent of m.mu to avoid lock ordering constraints).
	riMu         sync.RWMutex
	runningIndex map[string]struct{}

	gcStop                 context.CancelFunc
	gcDone                 chan struct{}
	gcReset                chan time.Duration // signals GC ticker reset
	lifecycleGCLagExceeded atomic.Bool

	OnTerminate func(sessionID string)
	OnDelete    func(ctx context.Context, info SessionInfo)
	// OnRuntimeRelease runs synchronously after a durable delete makes a session
	// non-resumable, or retention GC physically removes it. Consumers use it to
	// release state that must survive disconnects and ordinary termination.
	OnRuntimeRelease func(ctx context.Context, sessionID string)
	StateNotifier    func(ctx context.Context, sessionID string, state events.SessionState, message string)

	auditCollector *audit.Collector // issue #833 P1; nil = audit disabled
}

// SetAuditCollector injects the audit collector for session.create/delete instrumentation.
// Nil disables audit emission (no-op).
func (m *Manager) SetAuditCollector(c *audit.Collector) {
	m.auditCollector = c
}

// terminatedSessionTTL controls how long TERMINATED sessions remain in memory.
// After this duration, they are evicted from the in-memory map (DB records preserved).
const terminatedSessionTTL = 24 * time.Hour

// Session source constants — used for differential DB retention.
const (
	SourceCron = "cron" // cron-triggered session (24h retention)
)

// runningIndex helpers — use riMu (independent of m.mu/ms.mu) to avoid lock ordering issues.

func (m *Manager) addToRunningIndex(id string) {
	m.riMu.Lock()
	m.runningIndex[id] = struct{}{}
	m.riMu.Unlock()
}

func (m *Manager) removeFromRunningIndex(id string) {
	m.riMu.Lock()
	delete(m.runningIndex, id)
	m.riMu.Unlock()
}

func (m *Manager) updateRunningIndexForTransition(id string, from, to events.SessionState) {
	if from == events.StateRunning {
		m.removeFromRunningIndex(id)
	}
	if to == events.StateRunning {
		m.addToRunningIndex(id)
	}
}

// getRunningSessionIDs returns a snapshot of all RUNNING session IDs.
func (m *Manager) getRunningSessionIDs() []string {
	m.riMu.RLock()
	ids := make([]string, 0, len(m.runningIndex))
	for id := range m.runningIndex {
		ids = append(ids, id)
	}
	m.riMu.RUnlock()
	return ids
}

// managedSession holds a session's in-memory state and its mutex.
type managedSession struct {
	info   SessionInfo
	worker worker.Worker
	// attachedWorkspaceID is the workspaceID captured under ms.mu at attach
	// time and used to pair every pool-quota release with its acquire. It is
	// deliberately decoupled from ms.info.WorkspaceID so a concurrent
	// SetWorkspaceID cannot strand a quota slot on the wrong workspace
	// (review P2 quota-drift fix). Reads/writes are guarded by ms.mu.
	attachedWorkspaceID string
	TurnCount           int
	startedAt           time.Time
	deleting            bool // guarded by mu; prevents AttachWorker during durable delete
	log                 *slog.Logger
	mu                  sync.RWMutex // protects state transitions and input handling; reads use RLock
}

// SessionInfo is the in-memory session metadata.
type SessionInfo struct {
	ID            string              `json:"id"`
	UserID        string              `json:"user_id"`
	OwnerID       string              `json:"owner_id,omitempty"` // authenticated owner; falls back to UserID when nil
	BotID         string              `json:"bot_id,omitempty"`   // SEC-007: bot isolation
	BotName       string              `json:"bot_name,omitempty"` // YAML config name for agent-config resolution
	WorkerType    worker.WorkerType   `json:"worker_type"`
	State         events.SessionState `json:"state"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	ExpiresAt     *time.Time          `json:"expires_at,omitempty"`
	IdleExpiresAt *time.Time          `json:"idle_expires_at,omitempty"`
	// Lifecycle fields are independent of runtime expiry. Only dedicated
	// lifecycle operations may change them; ordinary session Upserts preserve
	// the values already stored in the database.
	LifecyclePolicy         string     `json:"lifecycle_policy,omitempty"`
	LifecyclePolicyRevision string     `json:"lifecycle_policy_revision,omitempty"`
	LastInputAt             *time.Time `json:"last_input_at,omitempty"`
	RuntimeFinishedAt       *time.Time `json:"runtime_finished_at,omitempty"`
	ArchiveAt               *time.Time `json:"archive_at,omitempty"`
	ConversationExpiresAt   *time.Time `json:"conversation_expires_at,omitempty"`
	LastContentExpiresAt    *time.Time `json:"last_content_expires_at,omitempty"`
	HistoryExpiresAt        *time.Time `json:"history_expires_at,omitempty"`
	DeletedAt               *time.Time `json:"deleted_at,omitempty"`
	// Archived is a response projection derived from ArchiveAt. It is not
	// persisted and does not change the AEP runtime state.
	Archived bool           `json:"archived,omitempty"`
	Context  map[string]any `json:"context,omitempty"`
	// WorkerSessionID is the session ID used by the worker runtime itself.
	// Only populated for workers that auto-generate their own session IDs (OpenCode Server).
	// For Claude Code this is always empty — the gateway's ID IS the worker's session ID
	// (passed via --session-id / --resume).
	WorkerSessionID string `json:"worker_session_id,omitempty"`
	// AllowedTools is the list of tools this session is allowed to use.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Platform identifies the messaging platform ("slack", "feishu", "" for direct WS).
	Platform string `json:"platform,omitempty"`
	// PlatformKey stores the consistency-mapping inputs as JSON.
	// This is the same data fed to DerivePlatformSessionKey, persisted so that
	// the mapping can be reconstructed from DB after a gateway restart.
	// Example (Feishu): {"chat_id":"oc_xxx","thread_ts":"","user_id":"ou_xxx"}
	// Example (Slack):  {"team_id":"Txxx","channel_id":"Cxxx","thread_ts":"1234.56","user_id":"Uxxx"}
	PlatformKey map[string]string `json:"platform_key,omitempty"`
	// WorkDir is the working directory for this session.
	WorkDir string `json:"work_dir,omitempty"`
	// Title is the user-facing session name. Used as DeriveSessionKey input for WebChat sessions.
	// Empty for Slack/Feishu sessions (they use DerivePlatformSessionKey instead).
	Title string `json:"title,omitempty"`
	// Source identifies the session origin: "" (user-initiated) or "cron" (cron-triggered).
	// Used for differential DB retention — cron sessions are cleaned up after 24h vs 7d for normal.
	Source string `json:"source,omitempty"`
	// ClientKey is the client-provided session_id from the init envelope.
	// Empty for platform sessions (Slack/Feishu) which use DerivePlatformSessionKey.
	ClientKey string `json:"client_key,omitempty"`
	// WorkspaceID is the WebChat multitenancy anchor (spec ① §6.4).
	// Empty for platform/cron sessions (Slack/Feishu) — they have no workspace.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// PermissionCeiling is the first effective permission tier reported by the
	// Worker after a successful Start/Resume. It is immutable for the lifetime
	// of this session and survives gateway restarts and Worker replacement.
	PermissionCeiling string `json:"permission_ceiling,omitempty"`
	// Identity is the bound AgentIdentity value object (#848): the stable,
	// secret-free identity snapshot persisted under context_json (no dedicated
	// column). Nil for sessions created before #848 — callers use
	// EffectiveIdentity(), which derives an equivalent identity on the fly so
	// legacy sessions remain correlatable. It is independent of Context (which
	// /reset clears): identity is session-level and survives a context reset.
	Identity *agentspec.AgentIdentity `json:"-"`
	// SpecSnapshot is the effective AgentSpec snapshot (#866): the versioned,
	// secret-free projection of the runtime policy that governed this session at
	// start time, persisted under context_json (no dedicated column). On resume
	// it is the source of truth for the effective policy — a config change does
	// NOT silently alter it — and AllowedTools is restored from it (the one
	// effective-policy field that is otherwise in-memory only). Nil for sessions
	// with no tool whitelist or created before #866 (legacy fallback: no
	// restore, AllowedTools stays empty = no restriction). Independent of
	// Context (/reset clears Context but the snapshot survives, like Identity).
	SpecSnapshot *agentspec.EffectiveAgentSpecSnapshot `json:"-"`
}

// EffectiveIdentity returns the bound identity if one was persisted, otherwise
// derives an equivalent AgentIdentity from this SessionInfo's existing fields.
// The derived AgentID is deterministic, so a legacy session (Identity == nil)
// correlates identically to one created after #848. Existing owner/workspace
// checks remain authoritative — this is an observation/correlation view only.
func (s SessionInfo) EffectiveIdentity() agentspec.AgentIdentity {
	if s.Identity != nil {
		return *s.Identity
	}
	return agentspec.BuildAgentIdentity(agentspec.IdentityInput{
		UserID:      s.UserID,
		WorkspaceID: s.WorkspaceID,
		BotID:       s.BotID,
		BotName:     s.BotName,
		Platform:    s.Platform,
		WorkerType:  string(s.WorkerType),
	})
}

// bindIdentity derives a fresh AgentIdentity snapshot from info's current fields
// and attaches it. It is called at creation and whenever an identity-bearing
// field (notably WorkspaceID, which WebChat binds after creation) changes, so
// the persisted identity stays consistent with the session references. It always
// re-derives from fields rather than reusing a cached value, so a late-bound
// workspace corrects an identity that was first bound without one.
func bindIdentity(info *SessionInfo) {
	if info == nil {
		return
	}
	id := agentspec.BuildAgentIdentity(agentspec.IdentityInput{
		UserID:      info.UserID,
		WorkspaceID: info.WorkspaceID,
		BotID:       info.BotID,
		BotName:     info.BotName,
		Platform:    info.Platform,
		WorkerType:  string(info.WorkerType),
	})
	info.Identity = &id
}

// bindSpecSnapshot captures the effective runtime policy as a persisted snapshot
// (#866). It is called at creation so the policy that governed the session is
// frozen: on resume the snapshot — not mutable config — is the source of truth,
// so a config change does not silently alter an existing session's effective
// policy (issue #866 AC2).
//
// First-cut binding captures the effective fields available on SessionInfo at
// create time (WorkerType, AllowedTools, and the permission ceiling once the
// worker reports it). A snapshot is bound only when there is an effective tool
// whitelist to persist — otherwise the session is tool-unrestricted and a
// snapshot would add bytes without value (legacy-equivalent: nil → no restore).
// The full effective spec (sandbox/budget/model) is captured once the
// agentspec resolver becomes authoritative on the live path (follow-up slice).
func bindSpecSnapshot(info *SessionInfo) {
	if info == nil || len(info.AllowedTools) == 0 {
		return
	}
	spec := agentspec.AgentSpec{
		Worker: agentspec.WorkerSpec{Type: string(info.WorkerType)},
		Policy: agentspec.PolicySpec{
			PermissionMode: info.PermissionCeiling,
			AllowedTools:   append([]string(nil), info.AllowedTools...),
		},
	}
	snap := agentspec.SnapshotFromSpec(spec)
	info.SpecSnapshot = &snap
}

// NewManager creates a new session manager using the provided Store.
// cfgStore is optional; when non-nil, GC and state transitions read the latest config dynamically.
func NewManager(ctx context.Context, log *slog.Logger, cfg *config.Config, cfgStore *config.ConfigStore, store Store) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}

	m := &Manager{
		log:          log.With("component", "session"),
		store:        store,
		cfg:          cfg,
		cfgStore:     cfgStore,
		pool:         NewPoolManagerWithWorkspace(log, cfg.Pool.MaxSize, cfg.Pool.MaxIdlePerUser, cfg.Pool.MaxMemoryPerUser, cfg.Pool.MaxPerWorkspace),
		sessions:     make(map[string]*managedSession),
		runningIndex: make(map[string]struct{}),
		gcReset:      make(chan time.Duration, 1),
	}

	// Start background GC.
	gcCtx, stop := context.WithCancel(context.Background())
	m.gcStop = stop
	m.gcDone = make(chan struct{})
	go m.runGC(gcCtx)

	m.log.Info("session: manager initialized")
	return m, nil
}

// Create creates a new session and persists it to SQLite.
func (m *Manager) Create(ctx context.Context, id, userID string, workerType worker.WorkerType, allowedTools []string, workDir, title string) (*SessionInfo, error) {
	return m.CreateWithBot(ctx, id, userID, "", "", workerType, allowedTools, "", nil, "", workDir, title, "")
}

// CreateWithBot creates a new session with explicit bot/workspace identity and
// persists it atomically. Binding WorkspaceID before identity derivation keeps
// the create audit, persisted identity, and first runtime metadata correlated by
// the same AgentID.
func (m *Manager) CreateWithBot(ctx context.Context, id, userID, botID, botName string, workerType worker.WorkerType, allowedTools []string, platform string, platformKey map[string]string, workspaceID, workDir, title, clientKey string) (*SessionInfo, error) {
	if len(clientKey) > MaxClientKeyLen {
		return nil, fmt.Errorf("%w: length %d exceeds maximum %d", ErrClientKeyTooLong, len(clientKey), MaxClientKeyLen)
	}
	now := time.Now()
	source := ""
	if _, isCron := platformKey["cron_job_id"]; isCron {
		source = SourceCron
	}
	lifecycle, err := newInitialLifecycleState(now, m.cfg.Lifecycle)
	if err != nil {
		return nil, err
	}
	info := &SessionInfo{
		ID:           id,
		UserID:       userID,
		BotID:        botID,
		BotName:      botName,
		WorkerType:   workerType,
		State:        events.StateCreated,
		CreatedAt:    now,
		UpdatedAt:    now,
		ExpiresAt:    ptr(now.Add(m.cfg.Session.RetentionPeriod)),
		AllowedTools: allowedTools,
		Platform:     platform,
		PlatformKey:  platformKey,
		WorkDir:      workDir,
		Title:        title,
		Source:       source,
		ClientKey:    clientKey,
		WorkspaceID:  workspaceID,
	}
	lifecycle.apply(info)
	// Bind the AgentIdentity snapshot (#848) from the final creation inputs.
	bindIdentity(info)
	// Capture the effective runtime policy snapshot (#866) so it survives
	// restart and is not silently altered by later config drift.
	bindSpecSnapshot(info)

	if err := m.store.Upsert(ctx, info); err != nil {
		m.emitSessionCreateAudit(userID, botID, platform, id, string(workerType), audit.OutcomeFailure, info.EffectiveIdentity(), info.SpecSnapshot)
		return nil, err
	}

	m.mu.Lock()
	m.sessions[id] = &managedSession{info: *info, log: m.log.With("worker_type", workerType, "channel", info.Platform)}
	m.mu.Unlock()

	m.log.Info("session: created", "session_id", id, "user_id", userID, "worker_type", workerType, "bot_id", botID)
	observability.SessionCreated().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(workerType))))
	sessionsActiveGauge(string(events.StateCreated)).Add(1)
	m.emitSessionCreateAudit(userID, botID, platform, id, string(workerType), audit.OutcomeSuccess, info.EffectiveIdentity(), info.SpecSnapshot)
	return info, nil
}

// emitSessionCreateAudit enqueues a session.create audit event.
// Uses context.Background() so audit never blocks the caller.
func (m *Manager) emitSessionCreateAudit(userID, botID, platform, sessionID, workerType, outcome string, id agentspec.AgentIdentity, snap *agentspec.EffectiveAgentSpecSnapshot) {
	if m.auditCollector == nil {
		return
	}
	_ = m.auditCollector.Enqueue(context.Background(), &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   audit.UserIDTypePlatform,
		Platform:     platform,
		SessionID:    sessionID,
		Action:       audit.ActionSessionCreate,
		ResourceType: "session",
		ResourceID:   sessionID,
		Outcome:      outcome,
		DetailJSON:   auditDetailJSON(botID, workerType, id, snap),
	})
}

// emitSessionDeleteAudit enqueues a session.delete audit event.
// Uses context.Background() so audit never blocks the caller.
func (m *Manager) emitSessionDeleteAudit(ctx context.Context, userID, botID, platform, sessionID, workerType, outcome string, id agentspec.AgentIdentity, snap *agentspec.EffectiveAgentSpecSnapshot) {
	if m.auditCollector == nil {
		return
	}
	request := deleteAuditRequestFromContext(ctx)
	_ = m.auditCollector.Enqueue(context.Background(), &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   audit.UserIDTypePlatform,
		Platform:     platform,
		SessionID:    sessionID,
		Action:       audit.ActionSessionDelete,
		ResourceType: "session",
		ResourceID:   sessionID,
		Outcome:      outcome,
		DetailJSON:   auditDetailJSON(botID, workerType, id, snap),
		IP:           request.IP,
		UserAgent:    request.UserAgent,
	})
}

// AuditDetailFields returns the secret-free field map for a session audit
// event's detail_json. It carries the unified identity correlation keys (#848):
// agent_id is always present (the primary cross-surface key), and worker_type /
// user_id / workspace_id / platform use the SAME key names as AEP metadata and
// trace attributes, so a single agent correlates across all three surfaces.
// bot_id is an audit-specific key. Keys with empty values are omitted to keep
// platform/anonymous-session payloads minimal. Callers may extend the map (e.g.
// add an "error" field) before marshalling.
func AuditDetailFields(botID, workerType string, id agentspec.AgentIdentity) map[string]any {
	detail := map[string]any{
		agentspec.MetadataKeyAgentID:    id.AgentID,
		agentspec.MetadataKeyWorkerType: workerType,
	}
	if botID != "" {
		detail["bot_id"] = botID
	}
	if id.UserID != "" {
		detail[agentspec.MetadataKeyUserID] = id.UserID
	}
	if id.WorkspaceID != "" {
		detail[agentspec.MetadataKeyWorkspaceID] = id.WorkspaceID
	}
	if id.Platform != "" {
		detail[agentspec.MetadataKeyPlatform] = id.Platform
	}
	if id.Anonymous {
		detail["anonymous"] = true
	}
	return detail
}

// auditDetailJSON marshals AuditDetailFields (plus the effective-spec fingerprint
// when a snapshot is bound) into the detail_json string. The map only holds
// strings/bools/ints, so marshal cannot fail in practice; the fallback keeps the
// primary correlation key if it ever does.
func auditDetailJSON(botID, workerType string, id agentspec.AgentIdentity, snap *agentspec.EffectiveAgentSpecSnapshot) string {
	detail := AuditDetailFields(botID, workerType, id)
	snap.StampMetadata(detail)
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Sprintf(`{%q:%q}`, agentspec.MetadataKeyAgentID, id.AgentID)
	}
	return string(b)
}

// SetWorkspaceID late-binds a legacy/unbound session to a workspace and persists
// the change (spec ①). New sessions receive WorkspaceID in CreateWithBot so their
// identity and create audit are atomic. Empty workspaceID is a no-op.
func (m *Manager) SetWorkspaceID(ctx context.Context, id, workspaceID string) error {
	if workspaceID == "" {
		return nil
	}
	m.mu.Lock()
	ms, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	// Write ms.info under ms.mu (not just m.mu) to prevent data race with
	// Get/transitionState readers that access ms.info under ms.mu after
	// releasing m.mu (review P1 fix — lock ordering: m.mu → ms.mu).
	ms.mu.Lock()
	previousWorkspaceID := ms.info.WorkspaceID
	previousIdentity := ms.info.Identity
	ms.info.WorkspaceID = workspaceID
	bindIdentity(&ms.info) // re-bind now that the workspace anchor is known
	info := ms.info
	ms.mu.Unlock()
	m.mu.Unlock()
	if err := m.store.Upsert(ctx, &info); err != nil {
		// Rollback in-memory on DB failure to prevent divergence.
		m.mu.Lock()
		ms.mu.Lock()
		ms.info.WorkspaceID = previousWorkspaceID
		ms.info.Identity = previousIdentity
		ms.mu.Unlock()
		m.mu.Unlock()
		return err
	}
	return nil
}

// Get returns a snapshot of a session by ID. Returns ErrSessionNotFound if not found.
// The returned *SessionInfo is a copy safe to read without holding locks.
func (m *Manager) Get(ctx context.Context, id string) (*SessionInfo, error) {
	m.mu.RLock()
	ms, ok := m.sessions[id]
	m.mu.RUnlock()
	if ok {
		ms.mu.RLock()
		if ms.deleting {
			ms.mu.RUnlock()
			return nil, ErrSessionCleanupPending
		}
		info := ms.info
		ms.mu.RUnlock()
		updateArchiveProjection(&info, time.Now())
		return &info, nil
	}

	// Fall back to Store.
	info, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	updateArchiveProjection(info, time.Now())

	m.mu.Lock()
	// Double-check: another goroutine may have populated this while we loaded from store.
	if existing, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		existing.mu.RLock()
		if existing.deleting {
			existing.mu.RUnlock()
			return nil, ErrSessionCleanupPending
		}
		cached := existing.info
		existing.mu.RUnlock()
		updateArchiveProjection(&cached, time.Now())
		return &cached, nil
	}
	m.sessions[id] = &managedSession{info: *info, log: m.log.With("worker_type", info.WorkerType, "channel", info.Platform)}
	m.mu.Unlock()

	return info, nil
}

// GetPurgeStatus reads a deletion job through its persisted user ownership
// snapshot. The status remains available after the deleted session is hidden.
func (m *Manager) GetPurgeStatus(ctx context.Context, sessionID, userID string) (*PurgeStatus, error) {
	reader, ok := m.store.(PurgeStatusReader)
	if !ok {
		return nil, ErrPurgeStatusNotReady
	}
	return reader.GetPurgeStatus(ctx, sessionID, userID)
}

// IsSeqActive reports whether a session may still allocate or persist AEP
// sequence events. It rejects the in-memory deleting phase before the durable
// DELETED write completes, closing the gap where late producers could enter
// between delete start and the runtime-release barrier.
func (m *Manager) IsSeqActive(ctx context.Context, id string) bool {
	m.mu.RLock()
	ms, ok := m.sessions[id]
	m.mu.RUnlock()
	if ok {
		ms.mu.RLock()
		active := !ms.deleting && ms.info.State != events.StateDeleted
		ms.mu.RUnlock()
		return active
	}

	info, err := m.store.Get(ctx, id)
	return err == nil && info.State != events.StateDeleted
}

// updateSession applies a field mutation under ms.mu, persists to DB, and rolls back on error.
// The apply closure must capture previous values and return a rollback closure — all under the lock.
func (m *Manager) updateSession(ctx context.Context, ms *managedSession, apply func(*SessionInfo) func()) error {
	ms.mu.Lock()
	rollback := apply(&ms.info)
	ms.info.UpdatedAt = time.Now()
	info := ms.info
	ms.mu.Unlock()

	if err := m.store.Upsert(ctx, &info); err != nil {
		ms.mu.Lock()
		rollback()
		ms.mu.Unlock()
		return err
	}
	return nil
}

// UpdateWorkDir updates the workDir for an active session in memory and persists to DB.
func (m *Manager) UpdateWorkDir(ctx context.Context, id, workDir string) error {
	m.mu.RLock()
	ms, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return ErrSessionNotFound
	}
	m.mu.RUnlock()
	ms.mu.RLock()
	same := ms.info.WorkDir == workDir
	ms.mu.RUnlock()
	if same {
		return nil
	}
	return m.updateSession(ctx, ms, func(info *SessionInfo) func() {
		prev := info.WorkDir
		info.WorkDir = workDir
		return func() { info.WorkDir = prev }
	})
}

// ─── State transitions ───────────────────────────────────────────────────────

// transitionState performs the common state-transition work: validation,
// in-memory update, persistence, and notifications.
// Uses copy-on-write: mutates a snapshot of ms.info, then persists the
// snapshot. On DB success the snapshot becomes the new ms.info — on failure
// ms.info is unchanged, eliminating the need for rollback.
// Caller must hold ms.mu for write; this method temporarily releases ms.mu
// for the DB write and re-acquires it before returning.
// Returns a worker.Worker that the caller must Terminate (or Kill) outside
// the session mutex to avoid blocking concurrent reads during graceful shutdown.
//
// IMPORTANT: the caller MUST NOT call Terminate/Kill on the returned worker
// while holding ms.mu — doing so re-introduces the deadlock described in #655.
func (m *Manager) transitionState(ctx context.Context, ms *managedSession, from, to events.SessionState, termReason string) (worker.Worker, error) {
	if ms.deleting {
		return nil, ErrSessionCleanupPending
	}
	// Build the candidate state as a value copy (never mutates ms.info in-place).
	// NOTE: shallow copy — map fields (Context, PlatformKey) share headers.
	// Safe here because only scalar fields (State, UpdatedAt, etc.) are mutated.
	// WorkerSessionID is protected by the targeted SQL UPDATE guard below;
	// other scalar fields have a theoretical stale-write
	// window during the lock release for DB I/O, mitigated by the safety-net
	// persist on first event (see #709).
	candidate := ms.info
	candidate.State = to
	candidate.UpdatedAt = time.Now()

	// Set idle expiry when entering IDLE; clear when leaving IDLE.
	if to == events.StateIdle {
		candidate.IdleExpiresAt = ptr(time.Now().Add(m.cfg.Worker.IdleTimeout))
	} else {
		candidate.IdleExpiresAt = nil
	}

	ms.mu.Unlock()
	dbErr := m.store.Upsert(ctx, &candidate)
	ms.mu.Lock()

	if dbErr != nil {
		// ms.info untouched — no rollback needed.
		return nil, dbErr
	}

	// Preserve WorkerSessionID if it was set concurrently while the lock
	// was released during Upsert. Without this guard, the candidate copy
	// (taken before the concurrent update) overwrites both DB and in-memory
	// with a stale empty value, breaking session resume for ACP workers.
	//
	// NOTE: This targeted SQL UPDATE avoids full-row Upsert's risk of overwriting
	// concurrent field changes (UpdatedAt, ExpiresAt, Source, etc.) made during
	// the unlock window. There is a theoretical second race: a concurrent
	// UpdateWorkerSessionID that acquires ms.mu during the µs SQL window and
	// sets a different WorkerSessionID, which our stale "wsid" then overwrites.
	// Accepted because (a) the window is ~µs, (b) forwardEvents' first-event
	// safety-net persist (EnsureWorkerSessionID) will repair the row, and
	// (c) the concurrent overwrite detection below re-syncs in-memory state.
	if candidate.WorkerSessionID == "" && ms.info.WorkerSessionID != "" {
		wsid := ms.info.WorkerSessionID
		candidate.WorkerSessionID = wsid
		ms.mu.Unlock()
		guardCtx := context.WithoutCancel(ctx)
		dbErr := m.store.UpdateWorkerSessionIDSQL(guardCtx, candidate.ID, wsid)
		ms.mu.Lock()
		if dbErr != nil {
			// Roll back in-memory value to maintain consistency with DB
			// (both empty). The safety-net (EnsureWorkerSessionID) will
			// repair on first event.
			candidate.WorkerSessionID = ""
			observability.SessionGuardRePersistFailures().Add(ctx, 1)
			m.log.Error("session: failed to re-persist WorkerSessionID after guard",
				"session_id", candidate.ID, "worker_session_id", wsid, "err", dbErr)
		} else if ms.info.WorkerSessionID != wsid {
			// SQL succeeded but a concurrent writer changed the value while
			// the lock was released — re-sync in-memory snapshot.
			candidate.WorkerSessionID = ms.info.WorkerSessionID
			observability.SessionGuardRePersistConcurrentOverwrites().Add(ctx, 1)
		}
	}

	// Commit: replace ms.info with the persisted snapshot.
	ms.info = candidate

	// Capture worker reference and nil under lock to prevent DetachWorker from
	// releasing quota a second time. The caller must Terminate the returned
	// worker OUTSIDE the session mutex — worker.Terminate() can block for
	// seconds during graceful shutdown (SIGTERM + grace period), and holding
	// ms.mu during that time blocks all concurrent reads (Get, GetWorker).
	var workerToTerminate worker.Worker
	if (to == events.StateTerminated || to == events.StateDeleted) && ms.worker != nil {
		// Record worker execution duration and decrement running gauge.
		if !ms.startedAt.IsZero() {
			observability.WorkerExecDuration().Record(ctx, time.Since(ms.startedAt).Seconds(), metric.WithAttributes(attribute.String("worker_type", string(ms.info.WorkerType))))
		}
		workersRunningGauge(string(ms.info.WorkerType)).Add(-1)
		// Release quota only when worker is still attached (DetachWorker may
		// have already released it on the bridge cleanup path).
		m.releaseWorkerQuota(ctx, ms)
		workerToTerminate = ms.worker
		ms.worker = nil
	}

	m.log.Info("session: transitioned", "session_id", ms.info.ID, "from", from, "to", to)

	// Update active sessions gauge.
	sessionsActiveGauge(string(from)).Add(-1)
	sessionsActiveGauge(string(to)).Add(1)

	// Record termination reason.
	if to == events.StateTerminated {
		if termReason == "" {
			termReason = "terminated"
		}
		observability.SessionTerminated().Add(ctx, 1, metric.WithAttributes(attribute.String("reason", termReason)))
	}
	if to == events.StateDeleted {
		observability.SessionDeleted().Add(ctx, 1)
	}

	m.notifyStateChange(ctx, ms.info.ID, to, "", termReason, candidate.LifecyclePolicy)

	m.updateRunningIndexForTransition(ms.info.ID, from, to)

	return workerToTerminate, nil
}

// terminateWorkerGracefully sends SIGTERM with a full grace period timeout.
// Uses context.WithoutCancel to inherit OTel trace context from ctx while
// ensuring the grace period is not shortened by parent cancellation — the
// previous code used the parent ctx directly, allowing parent cancel (e.g.
// GC tick ctx) to truncate the graceful shutdown window before SIGKILL fires.
func (m *Manager) terminateWorkerGracefully(ctx context.Context, w worker.Worker, sessionID string) {
	if w == nil {
		return
	}
	terminateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), base.GracefulShutdownTimeout)
	defer cancel()
	if err := w.Terminate(terminateCtx); err != nil {
		m.log.Warn("session: worker terminate failed", "session_id", sessionID, "err", err)
	}
}

// forceTerminateInMemory performs in-memory state cleanup when transitionState fails
// (DB error or invalid transition). It mirrors transitionState's side effects
// (metrics, quota release, worker nil) without DB persistence.
// Caller must hold ms.mu for write. Returns worker to kill outside lock.
//
// Design note: the returned worker is Kill()'d (hard) by the caller, whereas
// transitionState uses Terminate() (graceful SIGTERM + 5s timeout). This is
// intentional — when DB persistence fails we cannot track the graceful-shutdown
// window, so a hard kill is the safer choice to guarantee process cleanup.
func (m *Manager) forceTerminateInMemory(ctx context.Context, ms *managedSession, from events.SessionState, termReason string) worker.Worker {
	var workerToKill worker.Worker
	ms.info.State = events.StateTerminated
	ms.info.UpdatedAt = time.Now()

	// Guard termReason to avoid empty Prometheus label (matches transitionState behavior).
	if termReason == "" {
		termReason = "terminated"
	}

	// Record worker execution duration.
	if !ms.startedAt.IsZero() && ms.worker != nil {
		observability.WorkerExecDuration().Record(ctx, time.Since(ms.startedAt).Seconds(), metric.WithAttributes(attribute.String("worker_type", string(ms.info.WorkerType))))
	}
	// Release quota and decrement running gauge.
	if ms.worker != nil {
		workersRunningGauge(string(ms.info.WorkerType)).Add(-1)
		m.releaseWorkerQuota(ctx, ms)
		workerToKill = ms.worker
	}
	ms.worker = nil // unconditional nil — matches transitionState, prevents double-release

	m.updateRunningIndexForTransition(ms.info.ID, from, events.StateTerminated)
	sessionsActiveGauge(string(from)).Add(-1)
	sessionsActiveGauge(string(events.StateTerminated)).Add(1)
	observability.SessionTerminated().Add(ctx, 1, metric.WithAttributes(attribute.String("reason", termReason)))

	m.notifyStateChange(ctx, ms.info.ID, events.StateTerminated, termReason, termReason, ms.info.LifecyclePolicy)

	return workerToKill
}

// Transition atomically transitions a session to a new state.
// Both the in-memory state and the DB are updated.
// When transitioning to IDLE, sets idle_expires_at = now + IdleTimeout.
func (m *Manager) Transition(ctx context.Context, id string, to events.SessionState) error {
	return m.TransitionWithReason(ctx, id, to, "client_kill")
}

// TransitionWithReason transitions a session with an explicit termination reason.
// termReason is used as the label value for SessionsTerminated when transitioning
// to StateTerminated (e.g., "idle_timeout", "max_lifetime", "zombie", "admin_kill").
//
// Uses explicit ms.mu.Unlock() instead of defer — worker.Terminate() must run
// outside the session mutex to prevent blocking concurrent reads (#655).
func (m *Manager) TransitionWithReason(ctx context.Context, id string, to events.SessionState, termReason string) error {
	if m == nil {
		return ErrSessionNotFound
	}
	ms := m.getManagedSession(ctx, id)
	if ms == nil {
		return ErrSessionNotFound
	}

	ms.mu.Lock()
	from := ms.info.State
	if from == to {
		ms.mu.Unlock()
		return nil // idempotent: already in target state
	}
	if !events.IsValidTransition(from, to) {
		ms.mu.Unlock()
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}

	workerToTerminate, err := m.transitionState(ctx, ms, from, to, termReason)
	ms.mu.Unlock()
	m.terminateWorkerGracefully(ctx, workerToTerminate, id)
	return err
}

// TransitionWithInput performs a state transition and processes user input
// atomically (both under the same mutex).
func (m *Manager) TransitionWithInput(ctx context.Context, id string, to events.SessionState, content string, metadata map[string]any) error {
	if m == nil {
		return ErrSessionNotFound
	}
	ms := m.getManagedSession(ctx, id)
	if ms == nil {
		return ErrSessionNotFound
	}

	ms.mu.Lock()

	from := ms.info.State
	if !events.IsValidTransition(from, to) {
		ms.mu.Unlock()
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}

	ms.TurnCount++
	if ms.worker != nil {
		maxTurns := ms.worker.MaxTurns()
		if maxTurns > 0 && ms.TurnCount > maxTurns {
			m.log.Warn("session: max turns exceeded, initiating anti-pollution restart",
				"session_id", id, "turn_count", ms.TurnCount, "max_turns", maxTurns)
			// transitionState returns worker to terminate outside lock.
			var workerToKill worker.Worker
			var workerToTerminate worker.Worker
			if events.IsValidTransition(from, events.StateTerminated) {
				wt, err := m.transitionState(ctx, ms, from, events.StateTerminated, "max_turns")
				if err != nil {
					// Deliberate escape hatch: DB persistence failed, but we
					// force-terminate in-memory to ensure worker cleanup.
					// DB consistency is sacrificed here — the session may
					// appear active in DB after restart until GC reaps it.
					m.log.Error("session: max-turns state transition failed, force-terminating in-memory state",
						"session_id", id, "err", err)
					workerToKill = m.forceTerminateInMemory(ctx, ms, from, "max_turns")
				} else {
					workerToTerminate = wt
				}
			} else {
				// Escape hatch: invalid transition — force-terminate in-memory.
				m.log.Warn("session: max-turns transition invalid, force-terminating in-memory state",
					"session_id", id, "from_state", from)
				workerToKill = m.forceTerminateInMemory(ctx, ms, from, "max_turns")
			}
			ms.mu.Unlock()
			// Terminate/kill worker outside lock (only one will be non-nil).
			if workerToTerminate != nil {
				m.terminateWorkerGracefully(ctx, workerToTerminate, id)
			} else if workerToKill != nil {
				if err := workerToKill.Kill(); err != nil {
					m.log.Warn("session: worker kill failed during max-turns cleanup",
						"session_id", id, "err", err)
				}
			}
			return ErrMaxTurnsReached
		}
	}

	workerToTerminate, err := m.transitionState(ctx, ms, from, to, "client_input")
	ms.mu.Unlock()
	m.terminateWorkerGracefully(ctx, workerToTerminate, id)
	return err
}

// AttachWorker attempts to allocate concurrency quota and pair the worker runtime to the session.
// Pool quota is acquired outside m.mu to reduce lock contention under burst load.
// TOCTOU re-validation under m.mu ensures correctness.
func (m *Manager) AttachWorker(ctx context.Context, id string, w worker.Worker) error {
	if m == nil {
		return ErrSessionNotFound
	}

	// Pre-check: read userID, workspaceID and worker status under ms.mu.
	// ms.info.WorkspaceID is a mutable field (written by SetWorkspaceID under
	// ms.mu), so reading it under ms.mu matches Get's lock discipline and keeps
	// the race detector quiet if a future writer ever drops m.mu (review P1).
	m.mu.RLock()
	ms, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return ErrSessionNotFound
	}
	ms.mu.RLock()
	userID := ms.info.UserID
	workspaceID := ms.info.WorkspaceID
	alreadyAttached := ms.worker != nil
	deleting := ms.deleting
	ms.mu.RUnlock()
	m.mu.RUnlock()

	if deleting {
		return ErrSessionCleanupPending
	}
	if alreadyAttached {
		return ErrWorkerAttached
	}

	// Acquire pool quota (slot + memory) in a single atomic operation.
	if poolErr := m.pool.AcquireForWorkspace(ctx, userID, workspaceID); poolErr != nil {
		var pe *PoolError
		if !errors.As(poolErr, &pe) {
			m.log.Warn("session: attach rejected", "err", poolErr, "session_id", id)
			return ErrPoolExhausted
		}
		m.log.Warn("session: attach rejected", "kind", pe.Kind, "session_id", id)
		if pe.Kind == poolErrKindUserQuotaExceeded {
			return ErrUserQuotaExceeded
		}
		if pe.Kind == poolErrKindMemoryExceeded {
			return ErrMemoryExceeded
		}
		return ErrPoolExhausted
	}

	// Re-validate under write lock (TOCTOU safety).
	m.mu.Lock()
	ms, ok = m.sessions[id]
	if !ok {
		m.mu.Unlock()
		m.pool.ReleaseForWorkspace(ctx, userID, workspaceID)
		observability.PoolAcquire().Add(ctx, 1, metric.WithAttributes(attribute.String("result", "toctou_retry")))
		return ErrSessionNotFound
	}
	ms.mu.Lock()
	if ms.deleting {
		ms.mu.Unlock()
		m.mu.Unlock()
		m.pool.ReleaseForWorkspace(ctx, userID, workspaceID)
		observability.PoolAcquire().Add(ctx, 1, metric.WithAttributes(attribute.String("result", "toctou_retry")))
		return ErrSessionCleanupPending
	}
	if ms.worker != nil {
		ms.mu.Unlock()
		m.mu.Unlock()
		m.pool.ReleaseForWorkspace(ctx, userID, workspaceID)
		observability.PoolAcquire().Add(ctx, 1, metric.WithAttributes(attribute.String("result", "toctou_retry")))
		return ErrWorkerAttached
	}
	// Reject attach if session is no longer active — prevents Delete/Attach
	// race where Delete releases locks during DB write and a concurrent
	// AttachWorker slips in, creating an orphan worker with no session record.
	if !ms.info.State.IsActive() {
		ms.mu.Unlock()
		m.mu.Unlock()
		m.pool.ReleaseForWorkspace(ctx, userID, workspaceID)
		observability.PoolAcquire().Add(ctx, 1, metric.WithAttributes(attribute.String("result", "toctou_retry")))
		return ErrSessionNotActive
	}
	ms.worker = w
	// Pin quota ownership to the workspace acquired above so every later
	// release (DetachWorker/Delete/transition) returns the slot to the SAME
	// workspace, even if SetWorkspaceID mutates ms.info.WorkspaceID in between
	// (review P2 quota-drift fix).
	ms.attachedWorkspaceID = workspaceID
	ms.startedAt = time.Now()
	observability.PoolAcquire().Add(ctx, 1, metric.WithAttributes(attribute.String("result", "success")))
	observability.WorkerStarts().Add(ctx, 1, metric.WithAttributes(attribute.String("worker_type", string(ms.info.WorkerType)), attribute.String("result", "success")))
	workersRunningGauge(string(ms.info.WorkerType)).Add(1)
	ms.mu.Unlock()
	m.mu.Unlock()

	m.log.Debug("session: worker attached", "session_id", id, "user_id", userID)
	return nil
}

// GetWorker returns the worker for a session.
func (m *Manager) GetWorker(id string) worker.Worker {
	if m == nil {
		return nil
	}
	ms := m.getManagedSession(context.Background(), id)
	if ms == nil {
		return nil
	}
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.worker
}

// releaseWorkerQuota releases both concurrency slot and memory quota.
// pool.Release now handles both slot and memory under a single lock.
func (m *Manager) releaseWorkerQuota(ctx context.Context, ms *managedSession) {
	// Release against the workspace the quota was acquired on at attach time
	// (attachedWorkspaceID), not the possibly-since-changed ms.info.WorkspaceID,
	// so the release always pairs with its acquire (review P2 quota-drift fix).
	m.pool.ReleaseForWorkspace(ctx, ms.info.UserID, ms.attachedWorkspaceID)
}

// DetachWorker removes the worker from the session and releases the concurrency quota.
// It is safe to call even if no worker is attached.
func (m *Manager) DetachWorker(id string) {
	m.detachWorkerUnchecked(id, nil)
}

// DetachWorkerIf removes the worker only if it matches the expected one (CAS semantics).
// Returns true if detached, false if the current worker differs (another goroutine already replaced it).
// Use this from stale goroutines (e.g., old forwardEvents) to avoid clobbering a newer worker.
func (m *Manager) DetachWorkerIf(id string, expected worker.Worker) bool {
	return m.detachWorkerUnchecked(id, expected)
}

// detachWorkerUnchecked is the shared implementation.
// If expected is non-nil, it acts as a CAS guard — only detaches when ms.worker == expected.
func (m *Manager) detachWorkerUnchecked(id string, expected worker.Worker) bool {
	if m == nil {
		return false
	}
	ms := m.getManagedSession(context.Background(), id)
	if ms == nil {
		return false
	}

	ms.mu.Lock()
	if expected != nil && ms.worker != expected {
		// CAS mismatch — another goroutine already replaced the worker.
		ms.mu.Unlock()
		m.log.Debug("session: detach skipped, worker replaced", "session_id", id)
		return false
	}
	hasWorker := ms.worker != nil
	workerType := ms.info.WorkerType
	ms.worker = nil
	uid := ms.info.UserID
	// Release against the workspace the quota was acquired on at attach time
	// (review P2 quota-drift fix), not the live ms.info.WorkspaceID.
	wsID := ms.attachedWorkspaceID
	ms.mu.Unlock()

	if hasWorker {
		workersRunningGauge(string(workerType)).Add(-1)
		m.pool.ReleaseForWorkspace(context.Background(), uid, wsID)
		m.log.Debug("session: worker detached", "session_id", id)
	}
	return true
}

// Delete marks a session as DELETED, terminates any attached worker, and removes
// it from the in-memory cache.
// Lock ordering: m.mu → ms.mu (same as AttachWorker/DetachWorker to avoid deadlock).
// DB write is performed outside locks to avoid holding mutexes during I/O.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	ms, ok := m.sessions[id]
	if ok {
		ms.mu.Lock()
		if ms.deleting {
			ms.mu.Unlock()
			m.mu.Unlock()
			return ErrSessionCleanupPending
		}
		candidate := ms.info
		wasRunning := candidate.State == events.StateRunning
		now := time.Now()
		candidate.State = events.StateDeleted
		candidate.UpdatedAt = now
		if candidate.DeletedAt == nil {
			candidate.DeletedAt = &now
		}
		ms.deleting = true
		ms.mu.Unlock()
		m.mu.Unlock()
		return m.persistDelete(ctx, id, ms, candidate, nil, wasRunning)
	}

	// Install a cache tombstone before loading the row. This blocks Get,
	// AttachWorker, and sequence producers while the durable delete barrier is
	// being established, even when the session was not previously cached.
	ms = &managedSession{info: SessionInfo{ID: id}, deleting: true, log: m.log.With("session_id", id)}
	m.sessions[id] = ms
	m.mu.Unlock()

	info, err := m.store.Get(ctx, id)
	if err != nil {
		m.mu.Lock()
		if current, exists := m.sessions[id]; exists && current == ms {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
		if errors.Is(err, ErrSessionNotFound) {
			m.notifyRuntimeRelease(ctx, id)
			return nil
		}
		return err
	}
	candidate := *info
	wasRunning := candidate.State == events.StateRunning
	now := time.Now()
	candidate.State = events.StateDeleted
	candidate.UpdatedAt = now
	if candidate.DeletedAt == nil {
		candidate.DeletedAt = &now
	}
	return m.persistDelete(ctx, id, ms, candidate, info, wasRunning)
}

// persistDelete commits the durable tombstone and cleanup task before removing
// the in-memory entry. original is non-nil when Delete installed a cold-cache
// tombstone and loaded the session from Store.
func (m *Manager) persistDelete(ctx context.Context, id string, ms *managedSession, candidate SessionInfo, original *SessionInfo, wasRunning bool) error {
	var persistErr error
	if cleanup, ok := m.store.(CleanupTaskStore); ok {
		persistErr = cleanup.MarkDeletedWithCleanup(ctx, &candidate)
	} else {
		persistErr = m.store.Upsert(ctx, &candidate)
	}
	if persistErr != nil {
		m.mu.Lock()
		if current, exists := m.sessions[id]; exists && current == ms {
			if original != nil {
				m.sessions[id] = &managedSession{
					info: *original,
					log:  m.log.With("worker_type", original.WorkerType, "channel", original.Platform),
				}
			} else {
				ms.mu.Lock()
				ms.deleting = false
				ms.mu.Unlock()
			}
		}
		m.mu.Unlock()
		m.emitSessionDeleteAudit(ctx, candidate.UserID, candidate.BotID, candidate.Platform, id, string(candidate.WorkerType), audit.OutcomeFailure, candidate.EffectiveIdentity(), candidate.SpecSnapshot)
		return persistErr
	}

	// AttachWorker observes deleting under the same mutex, so no worker can be
	// attached during the durable state-and-outbox transaction above.
	var workerToTerminate worker.Worker
	m.mu.Lock()
	if current, exists := m.sessions[id]; exists && current == ms {
		ms.mu.Lock()
		ms.info = candidate
		if ms.worker != nil {
			workerToTerminate = ms.worker
			workersRunningGauge(string(candidate.WorkerType)).Add(-1)
			// Release against the workspace the quota was acquired on at attach
			// time (review P2 quota-drift fix), not the live ms.info.WorkspaceID.
			m.pool.ReleaseForWorkspace(ctx, candidate.UserID, ms.attachedWorkspaceID)
		}
		ms.mu.Unlock()
		delete(m.sessions, id)
		if wasRunning {
			m.removeFromRunningIndex(id)
		}
	}
	m.mu.Unlock()

	m.notifyStateChange(ctx, id, events.StateDeleted, "session deleted", "session_deleted", candidate.LifecyclePolicy)
	m.terminateWorkerGracefully(ctx, workerToTerminate, id)
	m.notifyRuntimeRelease(ctx, id)
	if _, ok := m.store.(CleanupTaskStore); !ok {
		m.notifyDelete(candidate)
	}

	m.log.Info("session: deleted", "session_id", id)
	m.emitSessionDeleteAudit(ctx, candidate.UserID, candidate.BotID, candidate.Platform, id, string(candidate.WorkerType), audit.OutcomeSuccess, candidate.EffectiveIdentity(), candidate.SpecSnapshot)
	return nil
}

// DeletePhysical physically removes a session from memory and database.
// USE WITH CAUTION: this bypasses state machine safety and conversation history.
func (m *Manager) DeletePhysical(ctx context.Context, id string) error {
	m.mu.Lock()
	ms, ok := m.sessions[id]
	if !ok {
		// Keep an in-memory deleting tombstone until the store operation
		// completes. Without it, IsSeqActive falls back to the still-live DB row
		// and admits late durable producers during physical deletion.
		ms = &managedSession{info: SessionInfo{ID: id}, deleting: true}
		m.sessions[id] = ms
	}
	var workerToKill worker.Worker
	var workerType string
	var deletedInfo *SessionInfo
	if ok {
		ms.mu.Lock()
		if ms.deleting {
			ms.mu.Unlock()
			m.mu.Unlock()
			return ErrSessionCleanupPending
		}
		ms.deleting = true
		info := ms.info
		deletedInfo = &info
		wasRunning := ms.info.State == events.StateRunning
		if ms.worker != nil {
			workerToKill = ms.worker
			workerType = string(ms.info.WorkerType)
			workersRunningGauge(workerType).Add(-1)
			m.releaseWorkerQuota(ctx, ms)
		}
		ms.mu.Unlock()
		if wasRunning {
			m.removeFromRunningIndex(id)
		}
	}
	m.mu.Unlock()
	if workerToKill != nil {
		if err := workerToKill.Kill(); err != nil {
			m.log.Warn("session: worker kill failed during physical delete",
				"session_id", id, "err", err)
		}
	}

	var deleteErr error
	cleanup, hasCleanup := m.store.(CleanupTaskStore)
	if hasCleanup {
		_, deleteErr = cleanup.DeletePhysicalWithCleanup(ctx, id)
	} else {
		if deletedInfo == nil && m.OnDelete != nil {
			if info, err := m.store.Get(ctx, id); err == nil {
				deletedInfo = info
			}
		}
		deleteErr = m.store.DeletePhysical(ctx, id)
	}

	// Remove only our tombstone, and only after the durable delete attempt.
	// On failure, the next access may safely reload the still-existing DB row.
	m.mu.Lock()
	if current, exists := m.sessions[id]; exists && current == ms {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if deleteErr != nil {
		return deleteErr
	}

	m.notifyRuntimeRelease(ctx, id)
	if !hasCleanup && deletedInfo != nil {
		m.notifyDelete(*deletedInfo)
	}
	return nil
}

// ValidateOwnership checks whether the given userID owns the session.
// Returns nil if the user is the owner, or ErrOwnershipMismatch otherwise.
// Admin bypass: if adminUserID is non-empty, it bypasses ownership check.
func (m *Manager) ValidateOwnership(ctx context.Context, sessionID, userID, adminUserID string) error {
	si, err := m.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if adminUserID != "" {
		m.log.Info("session: admin access to session",
			"session_id", sessionID,
			"admin_user_id", adminUserID,
			"session_owner", si.UserID,
		)
		return nil // admin bypass
	}
	if si.UserID != userID {
		m.log.Warn("session: ownership mismatch",
			"session_id", sessionID,
			"expected_owner", si.UserID,
			"actual_user", userID,
		)
		return ErrOwnershipMismatch
	}
	return nil
}

// ClearContext clears the session context map.
// Used by control.reset: Gateway layer clears SessionInfo.Context.
// Worker runtime context clearing is delegated to Worker.ResetContext (in-place or terminate+start).
func (m *Manager) ClearContext(ctx context.Context, sessionID string) error {
	if m == nil {
		return ErrSessionNotFound
	}
	ms := m.getManagedSession(ctx, sessionID)
	if ms == nil {
		return ErrSessionNotFound
	}
	ms.mu.RLock()
	empty := len(ms.info.Context) == 0
	ms.mu.RUnlock()
	if empty {
		return nil
	}
	return m.updateSession(ctx, ms, func(info *SessionInfo) func() {
		prev := info.Context
		info.Context = map[string]any{}
		return func() { info.Context = prev }
	})
}

// UpdateWorkerSessionID persists the worker-internal session ID for resume support.
// Workers that manage their own session IDs (OpenCode Server) call this
// to store the ID so it can be restored on resume.
func (m *Manager) UpdateWorkerSessionID(ctx context.Context, id, workerSessionID string) error {
	return m.updateWorkerSessionID(ctx, id, workerSessionID, false)
}

// EnsureWorkerSessionID persists the worker-internal session ID, bypassing
// the fast-path skip. Used by the safety-net caller (forwardEvents) to repair
// stale DB rows after guard re-persist failures.
func (m *Manager) EnsureWorkerSessionID(ctx context.Context, id, workerSessionID string) error {
	return m.updateWorkerSessionID(ctx, id, workerSessionID, true)
}

// SetPermissionCeilingIfEmpty atomically persists the first effective Worker
// permission ceiling. The authoritative stored value is returned so callers
// can detect a concurrent first-writer mismatch and fence the Worker.
func (m *Manager) SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error) {
	if m == nil {
		return "", ErrSessionNotFound
	}
	canonical, err := worker.NormalizeRuntimePermissionMode(ceiling)
	if err != nil {
		return "", err
	}
	ms := m.getManagedSession(ctx, id)
	if ms == nil {
		return "", ErrSessionNotFound
	}

	stored, err := m.store.SetPermissionCeilingIfEmpty(ctx, id, canonical)
	if err != nil {
		return "", err
	}

	ms.mu.Lock()
	ms.info.PermissionCeiling = stored
	if ms.info.SpecSnapshot != nil && ms.info.SpecSnapshot.PermissionMode != stored {
		updated := ms.info.SpecSnapshot.WithPermissionMode(stored)
		ms.info.SpecSnapshot = &updated
	}
	info := ms.info
	ms.mu.Unlock()

	// Persist whenever a snapshot exists, even if another concurrent call already
	// refreshed the in-memory value. The targeted update preserves unrelated DB
	// columns and makes a retry repair a prior failed snapshot write.
	if info.SpecSnapshot != nil {
		if err := m.store.UpdateSpecSnapshot(ctx, id, info.SpecSnapshot); err != nil {
			return "", fmt.Errorf("session: persist permission snapshot: %w", err)
		}
	}
	return stored, nil
}

func (m *Manager) updateWorkerSessionID(ctx context.Context, id, workerSessionID string, force bool) error {
	if m == nil {
		return ErrSessionNotFound
	}
	ms := m.getManagedSession(ctx, id)
	if ms == nil {
		return ErrSessionNotFound
	}

	// Fast-path skip when in-memory matches. Force mode (safety-net) always
	// writes to repair stale DB rows.
	if !force {
		ms.mu.RLock()
		same := ms.info.WorkerSessionID == workerSessionID
		ms.mu.RUnlock()
		if same {
			return nil
		}
	}

	// Update in-memory under lock, then persist via targeted SQL.
	// Uses UpdateWorkerSessionIDSQL rather than updateSession→Upsert because
	// the Upsert ON CONFLICT clause intentionally excludes worker_session_id
	// to prevent stale writes during transitionState's lock-release window.
	ms.mu.Lock()
	prev := ms.info.WorkerSessionID
	ms.info.WorkerSessionID = workerSessionID
	ms.mu.Unlock()

	if err := m.store.UpdateWorkerSessionIDSQL(ctx, id, workerSessionID); err != nil {
		// Roll back in-memory on DB failure.
		ms.mu.Lock()
		ms.info.WorkerSessionID = prev
		ms.mu.Unlock()
		return err
	}

	return nil
}

// DebugSessionSnapshot holds safe-to-expose debug info for a managed session.
// Exists to prevent callers from acquiring the per-session mutex directly,
// which would violate lock ordering invariants and risk deadlocks.
type DebugSessionSnapshot struct {
	TurnCount    int
	WorkerHealth worker.WorkerHealth
	HasWorker    bool
}

// DebugSnapshot safely captures debug fields from a managed session under the read lock.
func (m *Manager) DebugSnapshot(id string) (DebugSessionSnapshot, bool) {
	ms := m.getManagedSession(context.Background(), id)
	if ms == nil {
		return DebugSessionSnapshot{}, false
	}
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	snap := DebugSessionSnapshot{
		TurnCount: ms.TurnCount,
	}
	if ms.worker != nil {
		snap.HasWorker = true
		snap.WorkerHealth = ms.worker.Health()
	}
	return snap, true
}

// Lock acquires the per-session mutex for exclusive access.
// The caller MUST call Unlock when done.
func (m *Manager) Lock(id string) (release func(), err error) {
	ms := m.getManagedSession(context.Background(), id)
	if ms == nil {
		return nil, ErrSessionNotFound
	}
	ms.mu.Lock()
	return ms.mu.Unlock, nil
}

// List returns all sessions from Store. Use ListActive for in-memory active sessions only.
func (m *Manager) List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*SessionInfo, error) {
	sessions, err := m.store.List(ctx, userID, platform, workspaceID, limit, offset)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, info := range sessions {
		updateArchiveProjection(info, now)
	}
	return sessions, nil
}

// ListActive returns in-memory active sessions (no DB round-trip).
func (m *Manager) ListActive() []*SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	sessions := make([]*SessionInfo, 0, len(m.sessions))
	for _, ms := range m.sessions {
		ms.mu.RLock()
		info := ms.info
		ms.mu.RUnlock()
		updateArchiveProjection(&info, now)
		sessions = append(sessions, &info)
	}
	return sessions
}

// RepairRunningSessions transitions all sessions stuck in RUNNING state to TERMINATED.
// Called at gateway startup to repair sessions orphaned by a previous crash/restart.
func (m *Manager) RepairRunningSessions(ctx context.Context) (int, error) {
	ids, err := m.store.GetSessionsByState(ctx, events.StateRunning)
	if err != nil {
		return 0, fmt.Errorf("repair running sessions: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	repaired := 0
	for _, id := range ids {
		if err := m.TransitionWithReason(ctx, id, events.StateTerminated, "gateway_restart"); err != nil {
			m.log.Warn("session: repair running session failed", "session_id", id, "err", err)
		} else {
			repaired++
		}
	}
	return repaired, nil
}

// Stats returns the current worker pool counts: total active workers,
// maximum pool size, and number of unique users with active sessions.
func (m *Manager) Stats() (totalWorkers, maxWorkers, uniqueUsers int) {
	total, max, users := m.pool.Stats()
	return total, max, users
}

// ResetExpiry updates ExpiresAt to now + retentionPeriod for active sessions.
// Called after resume so a reactivated session isn't immediately killed by GC max_lifetime.
func (m *Manager) ResetExpiry(ctx context.Context, id string) error {
	m.mu.RLock()
	ms, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return ErrSessionNotFound
	}
	m.mu.RUnlock()
	return m.updateSession(ctx, ms, func(info *SessionInfo) func() {
		prev := info.ExpiresAt
		info.ExpiresAt = ptr(time.Now().Add(m.cfg.Session.RetentionPeriod))
		return func() { info.ExpiresAt = prev }
	})
}

// RecordInputAccepted advances the v2 session-level deadlines after a new
// durable input acceptance. The stored policy revision must match the active
// configuration; older sessions are left unchanged rather than being
// retroactively assigned a different retention policy.
func (m *Manager) RecordInputAccepted(ctx context.Context, id string, acceptedAt time.Time) error {
	if m == nil {
		return ErrSessionNotFound
	}
	if acceptedAt.IsZero() {
		return fmt.Errorf("session: accepted input time must be set")
	}

	cfg := m.cfg
	if m.cfgStore != nil {
		if current := m.cfgStore.Load(); current != nil {
			cfg = current
		}
	}
	if cfg == nil {
		return fmt.Errorf("session: lifecycle configuration is unavailable")
	}

	candidate, err := newInitialLifecycleState(acceptedAt, cfg.Lifecycle)
	if err != nil {
		return err
	}
	if candidate.policy != config.LifecyclePolicyV2 {
		return nil
	}

	ms := m.getManagedSession(ctx, id)
	if ms == nil {
		return ErrSessionNotFound
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	// A confirmed migration can update the durable policy while a legacy
	// session remains cached. Read only its lifecycle projection here; the
	// active Worker and runtime state remain owned by this manager.
	persisted, err := m.store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("session: refresh input lifecycle: %w", err)
	}
	if ms.deleting || persisted.DeletedAt != nil || persisted.State == events.StateDeleted {
		return ErrSessionNotFound
	}
	if persisted.LifecyclePolicy != config.LifecyclePolicyV2 ||
		persisted.LifecyclePolicyRevision == "" {
		return nil
	}
	ms.info.LifecyclePolicy = persisted.LifecyclePolicy
	ms.info.LifecyclePolicyRevision = persisted.LifecyclePolicyRevision
	ms.info.LastInputAt = persisted.LastInputAt
	ms.info.ArchiveAt = persisted.ArchiveAt
	ms.info.ConversationExpiresAt = persisted.ConversationExpiresAt
	ms.info.LastContentExpiresAt = persisted.LastContentExpiresAt
	ms.info.HistoryExpiresAt = persisted.HistoryExpiresAt

	lastInputAt := latestLifecycleTime(ms.info.LastInputAt, acceptedAt)
	archiveAt := latestLifecycleTime(ms.info.ArchiveAt, *candidate.archiveAt)
	conversationExpiresAt := latestLifecycleTime(ms.info.ConversationExpiresAt, *candidate.conversationExpiresAt)
	historyExpiresAt := latestLifecycleTime(ms.info.HistoryExpiresAt, *candidate.historyExpiresAt)
	updatedAt := latestLifecycleTime(&ms.info.UpdatedAt, time.Now())

	store, ok := m.store.(lifecycleDeadlineStore)
	if !ok {
		return fmt.Errorf("session: store does not support lifecycle deadline updates")
	}
	if err := store.AdvanceLifecycleDeadlines(
		ctx,
		id,
		persisted.LifecyclePolicyRevision,
		lastInputAt,
		archiveAt,
		conversationExpiresAt,
		historyExpiresAt,
		updatedAt,
	); err != nil {
		return fmt.Errorf("session: persist input lifecycle deadlines: %w", err)
	}

	ms.info.LastInputAt = ptr(lastInputAt)
	ms.info.ArchiveAt = ptr(archiveAt)
	ms.info.ConversationExpiresAt = ptr(conversationExpiresAt)
	ms.info.HistoryExpiresAt = ptr(historyExpiresAt)
	ms.info.UpdatedAt = updatedAt
	return nil
}

// WorkerHealthStatuses returns a snapshot of health for all active worker processes.
func (m *Manager) WorkerHealthStatuses() []worker.WorkerHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]worker.WorkerHealth, 0, len(m.sessions))
	for _, ms := range m.sessions {
		ms.mu.RLock()
		if ms.worker != nil {
			statuses = append(statuses, ms.worker.Health())
		}
		ms.mu.RUnlock()
	}
	return statuses
}

// TerminateAllWorkers gracefully terminates all actively tracked worker processes.
// This unblocks forwardEvents goroutines that are waiting on worker stdout,
// allowing bridge.Shutdown() to complete without timeout.
// Safe to call multiple times — Terminate is idempotent on exited processes.
func (m *Manager) TerminateAllWorkers() {
	m.mu.Lock()
	var workers []worker.Worker
	for _, ms := range m.sessions {
		ms.mu.Lock()
		if ms.worker != nil {
			workers = append(workers, ms.worker)
		}
		ms.mu.Unlock()
	}
	m.mu.Unlock()

	if len(workers) == 0 {
		return
	}

	eg, ctx := errgroup.WithContext(context.Background())
	for _, w := range workers {
		w := w
		eg.Go(func() error {
			terminateCtx, cancel := context.WithTimeout(ctx, base.GracefulShutdownTimeout)
			defer cancel()
			return w.Terminate(terminateCtx)
		})
	}
	_ = eg.Wait()
}

// Close shuts down the manager: stops GC, terminates workers, and closes the store.
func (m *Manager) Close() error {
	m.gcStop()
	<-m.gcDone

	m.TerminateAllWorkers()

	if err := m.store.Close(); err != nil {
		return err
	}
	return nil
}

// ─── GC ─────────────────────────────────────────────────────────────────────

func (m *Manager) runGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("session: runGC panic", "panic", r, "stack", string(debug.Stack()))
		}
		close(m.gcDone)
	}()
	ticker := time.NewTicker(m.cfg.Session.GCScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case newInterval := <-m.gcReset:
			ticker.Reset(newInterval)
			m.log.Info("session: GC ticker reset", "interval", newInterval)
		case <-ticker.C:
			m.gc(ctx)
		}
	}
}

// ResetGCInterval dynamically adjusts the GC scan interval.
// Safe to call from any goroutine (e.g. a config observer callback).
func (m *Manager) ResetGCInterval(interval time.Duration) {
	if interval <= 0 {
		return
	}
	// Non-blocking send: if a reset is already pending, the GC loop
	// will pick it up on the next iteration.
	select {
	case m.gcReset <- interval:
	default:
	}
}

// Pool returns the PoolManager for external hot-reload updates.
func (m *Manager) Pool() *PoolManager {
	return m.pool
}

func (m *Manager) gc(ctx context.Context) {
	now := time.Now()

	// 0. Zombie IO Polling for RUNNING sessions.
	// Uses runningIndex for O(running) lookup instead of O(total) full scan.
	runningIDs := m.getRunningSessionIDs()
	var runningWorkers []worker.Worker
	if len(runningIDs) > 0 {
		m.mu.RLock()
		for _, id := range runningIDs {
			if ms, ok := m.sessions[id]; ok {
				ms.mu.RLock()
				runningWorkers = append(runningWorkers, ms.worker)
				ms.mu.RUnlock()
			}
		}
		m.mu.RUnlock()
	}

	for i, id := range runningIDs {
		func(id string, w worker.Worker) {
			defer func() {
				if r := recover(); r != nil {
					m.log.Error("session: zombie GC check panic", "session_id", id, "panic", r)
				}
			}()
			if w == nil {
				return
			}
			lastIO := w.LastIO()
			timeout := 30 * time.Minute
			if m.cfg.Worker.ExecutionTimeout > 0 {
				timeout = m.cfg.Worker.ExecutionTimeout
			}
			if !lastIO.IsZero() && now.Sub(lastIO) > timeout {
				m.log.Warn("session: zombie IO polling triggered, terminating ghost process",
					"session_id", id, "worker_type", w.Type(), "last_io", lastIO, "timeout", timeout)
				if err := m.TransitionWithReason(ctx, id, events.StateTerminated, "zombie"); err != nil {
					m.log.Warn("session: zombie GC transition error", "err", err)
				}
			}
		}(id, runningWorkers[i])
	}

	// 1+2. Terminate sessions past max_lifetime and IDLE sessions past idle_timeout.
	// These two independent DB queries run in parallel.
	eg, egCtx := errgroup.WithContext(ctx)
	var maxIds, idleIds []string
	eg.Go(func() error {
		var err error
		maxIds, err = m.store.GetExpiredMaxLifetime(egCtx, now)
		if err != nil {
			m.log.Error("session: gc (max_lifetime) query", "now", now, "err", err)
		}
		return nil // don't propagate — we log and continue
	})
	eg.Go(func() error {
		var err error
		idleIds, err = m.store.GetExpiredIdle(egCtx, now)
		if err != nil {
			m.log.Error("session: gc (idle) query", "now", now, "err", err)
		}
		return nil
	})
	_ = eg.Wait() // errors already logged inside goroutines

	allIds := make([]string, 0, len(maxIds)+len(idleIds))
	allIds = append(allIds, maxIds...)
	allIds = append(allIds, idleIds...)
	if len(allIds) > 0 {
		// Build a reason map for O(1) lookup. max_lifetime wins if a session
		// appears in both lists (unlikely but safe to be explicit).
		reasonMap := make(map[string]string, len(maxIds)+len(idleIds))
		for _, id := range idleIds {
			reasonMap[id] = "idle_timeout"
		}
		for _, id := range maxIds {
			reasonMap[id] = "max_lifetime"
		}

		eg2, egCtx2 := errgroup.WithContext(ctx)
		eg2.SetLimit(5)
		for _, id := range allIds {
			id := id
			eg2.Go(func() error {
				if err := m.TransitionWithReason(egCtx2, id, events.StateTerminated, reasonMap[id]); err != nil {
					m.log.Warn("session: gc transition", "session_id", id, "reason", reasonMap[id], "err", err)
				}
				return nil // don't propagate individual failures
			})
		}
		_ = eg2.Wait()
	}

	// 3. Evict old TERMINATED sessions from in-memory map to prevent unbounded growth.
	// DB records are preserved — resume semantics fall back to store.Get when needed.
	var evicted int
	m.mu.Lock()
	for id, ms := range m.sessions {
		ms.mu.RLock()
		if ms.info.State == events.StateTerminated && now.Sub(ms.info.UpdatedAt) > terminatedSessionTTL {
			ms.mu.RUnlock()
			delete(m.sessions, id)
			evicted++
			continue
		}
		ms.mu.RUnlock()
	}
	m.mu.Unlock()
	if evicted > 0 {
		m.log.Info("session: gc evicted TERMINATED sessions from memory",
			"count", evicted, "ttl", terminatedSessionTTL)
	}
	// 4. Delete old TERMINATED sessions from DB with source-based retention.
	// Cron sessions: CronTermRetention, normal sessions: TermRetention. Events are not cascaded.
	cfg := m.cfg
	if m.cfgStore != nil {
		cfg = m.cfgStore.Load()
	}
	cronCutoff := now.Add(-cfg.Session.CronTermRetention)
	defaultCutoff := now.Add(-cfg.Session.TermRetention)
	deleted, err := m.store.DeleteTerminated(ctx, cronCutoff, defaultCutoff)
	if err != nil {
		m.log.Error("session: gc (delete_terminated) failed", "cron_cutoff", cronCutoff, "default_cutoff", defaultCutoff, "err", err)
	} else {
		for _, info := range deleted {
			m.notifyRuntimeRelease(ctx, info.ID)
		}
		if _, durable := m.store.(CleanupTaskStore); !durable {
			for _, info := range deleted {
				m.notifyDelete(*info)
			}
		}
	}

	// 5. Retire v2 conversations only after both the logical session and all
	// retained history are past their persisted deadlines, and no execution
	// recovery obligation remains. The store rechecks every condition while
	// holding the session lifecycle lock before it creates cleanup outbox work.
	lifecycleStore, ok := m.store.(lifecycleRetirementStore)
	if !ok {
		return
	}
	candidateCfg := m.cfg
	if m.cfgStore != nil {
		if current := m.cfgStore.Load(); current != nil {
			candidateCfg = current
		}
	}
	limit := candidateCfg.Lifecycle.GC.BatchSize
	candidates, err := lifecycleStore.ListExpiredLifecycleSessions(ctx, now, limit)
	if err != nil {
		observability.LifecycleGCFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("phase", "session_query")))
		m.log.Error("session: gc (lifecycle expiry query) failed", "err", err)
		return
	}
	for _, id := range candidates {
		m.retireExpiredLifecycleSession(ctx, lifecycleStore, id, now)
	}
	m.refreshLifecycleGCStatus(ctx, now, candidateCfg.Lifecycle.GC.MaxLag)
}

func (m *Manager) refreshLifecycleGCStatus(ctx context.Context, now time.Time, maxLag time.Duration) {
	statusStore, ok := m.store.(lifecycleGCStatusStore)
	if !ok {
		return
	}
	status, err := statusStore.GetLifecycleGCStatus(ctx, now)
	if err != nil {
		observability.LifecycleGCFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("phase", "status_query")))
		m.log.Warn("session: gc lifecycle status query failed", "err", err)
		return
	}

	eligibleLag := status.eligibleLag
	blockedLag := status.blockedLag
	lifecycleGCGauges.eligibleBacklog.Store(status.eligible)
	lifecycleGCGauges.blockedBacklog.Store(status.blocked)
	lifecycleGCGauges.unknownExecutionHold.Store(status.unknownExecutionHold)
	lifecycleGCGauges.eligibleLagNanos.Store(int64(eligibleLag))
	lifecycleGCGauges.blockedLagNanos.Store(int64(blockedLag))

	if eligibleLag > maxLag || blockedLag > maxLag {
		if m.lifecycleGCLagExceeded.CompareAndSwap(false, true) {
			m.log.Warn(
				"session: lifecycle GC overdue backlog exceeds max lag",
				"eligible_backlog", status.eligible,
				"blocked_backlog", status.blocked,
				"oldest_eligible_lag", eligibleLag,
				"oldest_blocked_lag", blockedLag,
				"max_lag", maxLag,
			)
		}
		return
	}
	m.lifecycleGCLagExceeded.Store(false)
}

func (m *Manager) retireExpiredLifecycleSession(ctx context.Context, store lifecycleRetirementStore, id string, now time.Time) {
	m.mu.Lock()
	ms, cached := m.sessions[id]
	if cached {
		ms.mu.Lock()
		if ms.deleting || ms.worker != nil || ms.info.State == events.StateRunning ||
			ms.info.LifecyclePolicy != config.LifecyclePolicyV2 {
			ms.mu.Unlock()
			m.mu.Unlock()
			return
		}
		ms.deleting = true
		ms.mu.Unlock()
	} else {
		ms = &managedSession{
			info:     SessionInfo{ID: id},
			deleting: true,
			log:      m.log.With("session_id", id),
		}
		m.sessions[id] = ms
	}
	m.mu.Unlock()

	retired, err := store.RetireExpiredLifecycleSession(ctx, id, now)
	if err != nil || retired == nil {
		m.mu.Lock()
		if current, exists := m.sessions[id]; exists && current == ms {
			if cached {
				ms.mu.Lock()
				ms.deleting = false
				ms.mu.Unlock()
			} else {
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		if err != nil {
			observability.LifecycleGCFailures().Add(ctx, 1, metric.WithAttributes(attribute.String("phase", "session_retire")))
			m.log.Warn("session: gc lifecycle retirement failed", "session_id", id, "err", err)
		}
		return
	}

	m.mu.Lock()
	if current, exists := m.sessions[id]; exists && current == ms {
		ms.mu.Lock()
		ms.info.State = events.StateDeleted
		ms.info.DeletedAt = ptr(now)
		ms.info.UpdatedAt = now
		ms.mu.Unlock()
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	m.notifyStateChange(ctx, id, events.StateDeleted, "session retention expired", "retention_expired", retired.LifecyclePolicy)
	m.notifyRuntimeRelease(ctx, id)
	m.emitSessionDeleteAudit(ctx, retired.UserID, retired.BotID, retired.Platform, id,
		string(retired.WorkerType), audit.OutcomeSuccess, retired.EffectiveIdentity(), retired.SpecSnapshot)
	observability.SessionDeleted().Add(ctx, 1)
	observability.RecordLifecycleGCProcessed(ctx, observability.LifecycleGCProcessedSession, 1)
	observability.LifecycleGCRetirementLag().Record(ctx, lifecycleExpirationLag(retired, now).Seconds())
}

func lifecycleExpirationLag(info *SessionInfo, now time.Time) time.Duration {
	if info == nil {
		return 0
	}
	var latestDeadline time.Time
	if info.ConversationExpiresAt != nil {
		latestDeadline = *info.ConversationExpiresAt
	}
	if info.LastContentExpiresAt != nil && info.LastContentExpiresAt.After(latestDeadline) {
		latestDeadline = *info.LastContentExpiresAt
	}
	if latestDeadline.IsZero() || now.Before(latestDeadline) {
		return 0
	}
	return now.Sub(latestDeadline)
}

func lifecycleDeadlineLag(deadline *time.Time, now time.Time) time.Duration {
	if deadline == nil || now.Before(*deadline) {
		return 0
	}
	return now.Sub(*deadline)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// safeGo runs fn in a goroutine with panic recovery. Panics are logged with
// stack trace instead of crashing the entire process.
func (m *Manager) safeGo(fn func()) {
	go m.safeCall(fn)
}

func (m *Manager) safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("session: callback panic",
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn()
}

// notifyStateChange sends state change and termination callbacks.
func (m *Manager) notifyStateChange(ctx context.Context, sessionID string, state events.SessionState, message, terminationReason, lifecyclePolicy string) {
	if m.StateNotifier != nil {
		notify := func() { m.StateNotifier(ctx, sessionID, state, message) }
		if state == events.StateDeleted {
			// Deletion releases the session's sequence counter after this method.
			// Complete state-event allocation/capture first so a delayed callback
			// cannot recreate the counter at 1 after collector flushing.
			m.safeCall(notify)
		} else {
			m.safeGo(notify)
		}
	}
	notifyTermination := state == events.StateDeleted ||
		(state == events.StateTerminated && shouldNotifyTerminationCleanup(lifecyclePolicy, terminationReason))
	if notifyTermination && m.OnTerminate != nil {
		m.safeGo(func() { m.OnTerminate(sessionID) })
	}
}

// notifyDelete runs the worker-runtime cleanup callback after an explicit
// session deletion has been persisted. It intentionally does not run for
// TERMINATED transitions: those sessions must remain resumable.
func (m *Manager) notifyDelete(info SessionInfo) {
	if m.OnDelete != nil {
		m.safeGo(func() { m.OnDelete(context.Background(), info) })
	}
}

func (m *Manager) notifyRuntimeRelease(ctx context.Context, sessionID string) {
	if m.OnRuntimeRelease != nil {
		m.OnRuntimeRelease(ctx, sessionID)
	}
}

func (m *Manager) getManagedSession(ctx context.Context, id string) *managedSession {
	m.mu.RLock()
	ms, ok := m.sessions[id]
	m.mu.RUnlock()
	if ok {
		return ms
	}
	// Load from Store.
	info, err := m.store.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, ErrSessionNotFound) {
			m.log.Error("session: store lookup failed", "session_id", id, "err", err)
		}
		return nil
	}
	m.mu.Lock()
	if ms, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return ms
	}
	ms = &managedSession{info: *info, log: m.log.With("worker_type", info.WorkerType, "channel", info.Platform)}
	m.sessions[id] = ms
	if info.State == events.StateRunning {
		m.addToRunningIndex(id)
	}
	m.mu.Unlock()
	return ms
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }
