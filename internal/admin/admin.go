package admin

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/skills"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/internal/web"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

const (
	ScopeSessionRead  = "session:read"
	ScopeSessionWrite = "session:write"
	ScopeSessionKill  = "session:delete"
	ScopeStatsRead    = "stats:read"
	ScopeHealthRead   = "health:read"
	ScopeConfigRead   = "config:read"
	ScopeAdminRead    = "admin:read"
	ScopeAdminWrite   = "admin:write"
	// Runtime operation scopes (#877/#946): fence inspection/actions and
	// effective runtime plan reads. Granted per-token via admin.token_scopes;
	// never included in the minimal default scope set.
	ScopeRuntimeRead  = "runtime:read"
	ScopeRuntimeWrite = "runtime:write"
)

// ErrRestartConflict classifies a restart preparation failure caused by an
// already active global restart transaction.
var ErrRestartConflict = errors.New("gateway restart already in progress")

// DBExecutor covers the sql.DB methods used by apiKeyUserStore.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type SessionManagerProvider interface {
	Stats() (total, max, unique int)
	List(ctx context.Context, userID, platform string, limit, offset int) ([]any, error)
	Get(ctx context.Context, id string) (any, error)
	Delete(ctx context.Context, id string) error
	DeletePhysical(ctx context.Context, id string) error
	WorkerHealthStatuses() []worker.WorkerHealth
	DebugSnapshot(id string) (DebugSessionSnapshot, bool)
	Transition(ctx context.Context, id string, to events.SessionState) error
}

type HubProvider interface {
	ConnectionsOpen() int
	NextSeqPeek(sessionID string) int64
}

// BridgeProvider provides session creation capability for the admin API.
// NOTE: The admin adapter hardcodes botName="" since admin-initiated sessions
// have no platform bot context (webchat/API sessions). This is intentional —
// admin sessions use platform-level agent-config, never per-bot configs.
type BridgeProvider interface {
	StartSession(ctx context.Context, p worker.SessionStartParams) error
}

type ConfigProvider interface {
	Get() *config.Config
}

type ConfigWatcherProvider interface {
	Rollback(version int) (*config.Config, int, error)
}

type TurnStatsProvider interface {
	TurnStats(ctx context.Context, sessionID string) (*eventstore.TurnStats, error)
	// LatestSeq returns the max persisted event seq for a session (issue #879 #5).
	LatestSeq(ctx context.Context, sessionID string) (int64, error)
}

// RuntimeExecutionProvider is the narrow execution-store view used by the
// runtime operations admin endpoints (#877). Injected from cmd/hotplex so
// admin stays decoupled from the gateway; errors must preserve
// execution.ErrNotFound / execution.ErrFenceConflict for status mapping.
type RuntimeExecutionProvider interface {
	ListFences(ctx context.Context, sessionID string, limit, offset int) ([]*execution.Record, error)
	ApplyFenceDecision(ctx context.Context, request execution.FenceActionRequest) (*execution.Record, error)
}

// RuntimeEffectProvider is the narrow delivery-ledger view used by the effect
// operator endpoints. Injected from cmd/hotplex so admin stays decoupled from
// the gateway; errors must preserve effect.ErrEffectNotFound /
// effect.ErrLeaseLost for status mapping.
type RuntimeEffectProvider interface {
	ListForOperator(ctx context.Context, f effect.OperatorListFilter) ([]*effect.Effect, error)
	GetByID(ctx context.Context, effectID string) (*effect.Effect, error)
	ListAttempts(ctx context.Context, effectID string) ([]*effect.Attempt, error)
	ApplyOperatorAction(ctx context.Context, req effect.OperatorActionRequest) (*effect.Effect, error)
}

// RuntimeEventNotifier emits the additive runtime.execution.failed event for
// an operator abandon (#877). Best-effort: fenced sessions usually have no
// connected clients, and the store write is already durable when this fires.
type RuntimeEventNotifier interface {
	NotifyExecutionAbandoned(ctx context.Context, sessionID, executionID string)
}

// BuiltinSkillsCatalog is the read-only embedded Agent Skills surface. It is
// intentionally structural so AdminAPI does not depend on reconciliation or
// on a concrete registry implementation.
type BuiltinSkillsCatalog interface {
	List(context.Context, string) ([]skills.Skill, error)
	Read(context.Context, string, string) (*skills.Detail, error)
}

type DebugSessionSnapshot struct {
	TurnCount    int
	WorkerHealth worker.WorkerHealth
	HasWorker    bool
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Write captures the implicit WriteHeader(200) call that occurs when handlers
// write data without first calling WriteHeader. Without this, the underlying
// http.response.Write() would call its own WriteHeader, bypassing our recorder.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach optional capabilities such as
// Flusher on the underlying server ResponseWriter.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

type AdminAPI struct {
	log              *slog.Logger
	cfg              ConfigProvider
	sm               SessionManagerProvider
	turnStore        TurnStatsProvider
	hub              HubProvider
	bridge           BridgeProvider
	configWatcher    ConfigWatcherProvider
	cron             CronSchedulerProvider
	botLister        BotListerProvider
	botConfig        BotConfigProvider
	wsStore          session.UserWorkspaceStore // Optional: enables /admin/workspaces console (issue #807); nil when multitenancy/webchat disabled
	logCollector     LogCollector
	akStore          APIKeyUserStorer // nil when DB resolver not enabled
	keyValidator     KeyValidator     // nil when not injected
	rateLimiter      atomic.Value     // *simpleRateLimiter
	allowedCIDRs     atomic.Value     // []string
	allowedOriginsFn func() []string  // returns allowed CORS origins from config
	version          func() string
	newSessionID     func() string
	restart          func() error
	restartPrepare   func(context.Context) (func() error, func() error, error)
	cookieAuth       *security.CookieAuth           // Optional: enables cookie-session fallback (issue #788 A2)
	idp              *security.LocalAccountProvider // Optional: paired with cookieAuth
	startedAt        time.Time
	activityService  *ActivityService         // Optional: enables /admin/activity + /admin/users/{id}/activity (issue #833)
	auditCollector   *audit.Collector         // Optional: emits system.audit_export meta-audit rows (issue #833)
	skillsLocator    *skills.Locator          // Optional: enables /admin/api/skills global skill management (issue #910)
	builtinSkills    BuiltinSkillsCatalog     // Optional: embedded read-only Agent Skills
	runtimeExec      RuntimeExecutionProvider // Optional: enables /admin/executions fence endpoints (#877); nil → 503
	runtimeNotifier  RuntimeEventNotifier     // Optional: emits runtime.execution.failed on abandon (#877)
	runtimeEffects   RuntimeEffectProvider    // Optional: enables /admin/effect endpoints; nil → 503
}

type Deps struct {
	Log              *slog.Logger
	Config           ConfigProvider
	SessionMgr       SessionManagerProvider
	TurnStats        TurnStatsProvider
	Hub              HubProvider
	Bridge           BridgeProvider
	ConfigWatcher    ConfigWatcherProvider
	Cron             CronSchedulerProvider
	BotLister        BotListerProvider
	BotConfig        BotConfigProvider
	WorkspaceStore   session.UserWorkspaceStore // Optional: enables /admin/workspaces console (issue #807)
	LogCollector     LogCollector
	Version          func() string
	NewSessionID     func() string
	Restart          func() error
	RestartPrepare   func(context.Context) (func() error, func() error, error)
	AllowedOriginsFn func() []string  // Optional: returns allowed CORS origins; defaults to ["*"] when nil
	DB               DBExecutor       // Optional: enables API key user CRUD + DB resolver
	DBResolver       cacheInvalidator // Optional: invalidates DBResolver cache after CUD
	WriteMu          *sqlutil.WriteMu // Optional: serializes SQLite writes; nil-safe, PG-safe
	APIKeyStore      APIKeyUserStorer // Optional: pre-built store (e.g. PG); overrides DB-based creation
	KeyValidator     KeyValidator     // Optional: syncs DB keys into auth layer for Phase 1 validation
}

func New(deps Deps) *AdminAPI {
	lc := deps.LogCollector
	if lc == nil {
		lc = LogRing
	}
	a := &AdminAPI{
		log:           deps.Log,
		cfg:           deps.Config,
		sm:            deps.SessionMgr,
		turnStore:     deps.TurnStats,
		hub:           deps.Hub,
		bridge:        deps.Bridge,
		configWatcher: deps.ConfigWatcher,
		cron:          deps.Cron,
		botLister:     deps.BotLister,
		botConfig:     deps.BotConfig,
		wsStore:       deps.WorkspaceStore,
		logCollector:  lc,
		keyValidator:  deps.KeyValidator,
		akStore: func() APIKeyUserStorer {
			if deps.APIKeyStore != nil {
				return deps.APIKeyStore
			}
			return newAPIKeyUserStoreWithInvalidator(deps.DB, deps.DBResolver, deps.WriteMu)
		}(),
		version:        deps.Version,
		newSessionID:   deps.NewSessionID,
		restart:        deps.Restart,
		restartPrepare: deps.RestartPrepare,
		startedAt:      time.Now(),
		allowedOriginsFn: func() []string {
			if deps.AllowedOriginsFn != nil {
				return deps.AllowedOriginsFn()
			}
			return []string{"*"}
		},
	}
	return a
}

// SetSkillsLocator 注入 skill 定位器，启用 /admin/api/skills 全局 skill 管理
// （issue #910）。nil 时 skill 端点返回 503。
func (a *AdminAPI) SetSkillsLocator(l *skills.Locator) { a.skillsLocator = l }

// SetBuiltinSkillsCatalog injects the embedded read-only Agent Skills view.
// Built-ins are merged only into read surfaces; CRUD always targets a real
// user-owned skill and rejects a builtin-only selection.
func (a *AdminAPI) SetBuiltinSkillsCatalog(c BuiltinSkillsCatalog) { a.builtinSkills = c }

// SetRuntimeExecution injects the execution-store provider that backs the
// operator fence endpoints (#877). nil-safe: fence endpoints return 503 until
// wired. The optional notifier emits runtime.execution.failed on abandon.
func (a *AdminAPI) SetRuntimeExecution(p RuntimeExecutionProvider, n RuntimeEventNotifier) {
	a.runtimeExec = p
	a.runtimeNotifier = n
}

// SetRuntimeEffects injects the delivery-ledger provider that backs the
// operator effect endpoints. nil-safe: they return 503 until wired.
func (a *AdminAPI) SetRuntimeEffects(p RuntimeEffectProvider) { a.runtimeEffects = p }

func (a *AdminAPI) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var actor string // resolved during auth, recorded by the audit defer (issue #788 A5)

		// Audit all write operations — successes AND failures (issue #788 review
		// P1-2). Failed/denied writes are the most forensically valuable; the
		// prior status<300 guard dropped them. actor falls back to "anonymous"
		// when auth never resolved (401/403 before actor assignment).
		//
		// Dual-write (issue #833 P2, spec §7): the legacy slog AdminAudit path
		// is retained during the compat period; the same write is also enqueued
		// into the tamper-evident user_activity table when an audit collector is
		// wired. slog retirement is deferred to P3 so existing log dashboards
		// keep working while consumers migrate.
		defer func() {
			if isWriteMethod(r.Method) {
				actorVal := actor
				if actorVal == "" {
					actorVal = "anonymous"
				}
				result := AuditResultOk
				if sw.status >= 400 {
					result = AuditResultFailed
				}
				slogAction := adminActionFor(r.Method, r.URL.Path)
				AdminAudit(actorVal, slogAction, r.URL.Path, result)
				// Best-effort tamper-evident dual-write. Never blocks the
				// response (Enqueue is non-blocking with spill fallback).
				a.enqueueAdminActivity(r, sw.status, actorVal, slogAction)
			}
		}()

		defer func() {
			a.log.Info("admin: request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration", time.Since(start),
				"ip", clientIP(r),
			)
		}()

		security.SetCORSHeaders(sw, a.allowedOriginsFn(), r.Header.Get("Origin"))
		if r.Method == http.MethodOptions {
			sw.WriteHeader(http.StatusOK)
			return
		}

		defer func() {
			if rv := recover(); rv != nil {
				a.log.Error("admin: panic recovered",
					"err", rv,
					"path", r.URL.Path,
					"method", r.Method,
					"stack", string(debug.Stack()),
				)
				web.WriteAppError(sw, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
		}()

		if rl, _ := a.rateLimiter.Load().(*simpleRateLimiter); rl != nil {
			if !rl.Allow() {
				web.WriteAppError(sw, http.StatusTooManyRequests, "RATE_LIMITED", "rate limit exceeded")
				return
			}
		}

		if cidrs, _ := a.allowedCIDRs.Load().([]string); len(cidrs) > 0 {
			addr := clientIP(r)
			if !ipAllowed(addr, cidrs) {
				a.log.Warn("admin: IP not whitelisted", "ip", addr)
				web.WriteAppError(sw, http.StatusForbidden, "FORBIDDEN", "IP not allowed")
				return
			}
		}

		// Health readiness probe exempt from auth (required for k8s/Docker probes).
		if r.URL.Path == "/admin/health/ready" {
			next.ServeHTTP(sw, r)
			return
		}

		token := extractBearerToken(r)
		if token != "" {
			scopes, ok := a.validateToken(token)
			if !ok {
				web.WriteAppError(sw, http.StatusUnauthorized, "UNAUTHORIZED", "invalid admin token")
				return
			}
			actor = "admin-token"
			ctx := context.WithValue(r.Context(), scopeContextKey{}, scopes)
			ctx = context.WithValue(ctx, actorContextKey{}, actor)
			next.ServeHTTP(sw, r.WithContext(ctx))
			return
		}

		// Cookie fallback (issue #788 A2): embedded webchat scenario where the
		// admin is already logged in via the chat session cookie. Mirrors the
		// /api/admin/* requireAdmin check — cookie→uid→role, requires active
		// admin. Granted the full scope set so every requireScope check passes.
		if a.cookieAuth != nil && a.idp != nil {
			// CSRF defense (issue #788 review P1): SameSite=None cookies (needed
			// for cross-subdomain webchat) otherwise let a cross-site form POST
			// ride the admin session on state-changing routes. Require a same-
			// origin proof; Bearer requests take the earlier branch and skip this.
			if isWriteMethod(r.Method) && !a.sameOriginRequest(r) {
				web.WriteAppError(sw, http.StatusForbidden, "FORBIDDEN", "cross-origin write blocked")
				return
			}
			uid, ok := a.cookieAuth.Authenticate(r)
			if !ok {
				web.WriteAppError(sw, http.StatusUnauthorized, "INVALID_CREDENTIALS", "not authenticated")
				return
			}
			u, err := a.idp.Lookup(r.Context(), uid)
			if err != nil || u.Status != "active" {
				web.WriteAppError(sw, http.StatusForbidden, "USER_DISABLED", "user disabled")
				return
			}
			if u.Role != "admin" {
				web.WriteAppError(sw, http.StatusForbidden, "FORBIDDEN", "admin only")
				return
			}
			actor = uid
			scopes := []string{
				ScopeAdminWrite, ScopeAdminRead,
				ScopeSessionRead, ScopeSessionWrite, ScopeSessionKill,
				ScopeStatsRead, ScopeHealthRead, ScopeConfigRead,
				ScopeRuntimeRead, ScopeRuntimeWrite,
			}
			ctx := context.WithValue(r.Context(), scopeContextKey{}, scopes)
			ctx = context.WithValue(ctx, actorContextKey{}, actor)
			next.ServeHTTP(sw, r.WithContext(ctx))
			return
		}

		web.WriteAppError(sw, http.StatusUnauthorized, "UNAUTHORIZED", "missing admin token")
	})
}

func (a *AdminAPI) validateToken(token string) ([]string, bool) {
	cfg := a.cfg.Get()
	tb := []byte(token)
	if cfg.Admin.TokenScopes != nil {
		for t, scopes := range cfg.Admin.TokenScopes {
			if subtle.ConstantTimeCompare(tb, []byte(t)) == 1 {
				return scopes, true
			}
		}
	}
	for _, t := range cfg.Admin.Tokens {
		if subtle.ConstantTimeCompare(tb, []byte(t)) == 1 {
			if len(cfg.Admin.DefaultScopes) > 0 {
				return cfg.Admin.DefaultScopes, true
			}
			return []string{ScopeSessionRead, ScopeStatsRead, ScopeHealthRead}, true
		}
	}
	return nil, false
}

func (a *AdminAPI) SetRateLimiter(rl *simpleRateLimiter) {
	a.rateLimiter.Store(rl)
}

func (a *AdminAPI) SetAllowedCIDRs(cidrs []string) {
	a.allowedCIDRs.Store(cidrs)
}

// SetCookieFallback enables cookie-session authentication on the Bearer admin
// port (issue #788 A2). When a request carries no Bearer token, the middleware
// resolves the chat session cookie and admits active admins, mirroring the
// requireAdmin check on /api/admin/* (user_handlers.go). No-op when either
// argument is nil — standalone/CLI deployments keep Bearer-only behavior.
//
// CORS note (issue #788 review P2): the cookie channel only works cross-origin
// when allowedOriginsFn() lists the webchat origin explicitly (not "*") —
// wildcard CORS suppresses Allow-Credentials and the browser refuses to send
// the session cookie. Same-origin embedded webchat is unaffected.
func (a *AdminAPI) SetCookieFallback(cookieAuth *security.CookieAuth, idp *security.LocalAccountProvider) {
	a.cookieAuth = cookieAuth
	a.idp = idp
}

// SetActivityService injects the ActivityService for /admin/activity endpoints (issue #833).
func (a *AdminAPI) SetActivityService(s *ActivityService) { a.activityService = s }

// SetAuditCollector injects the audit.Collector for emitting system.audit_export meta-audit rows (issue #833).
func (a *AdminAPI) SetAuditCollector(c *audit.Collector) { a.auditCollector = c }

// buildIdentityLinks resolves a set of user IDs to readable identity via the
// local users table (display_name, falling back to username). The result is
// keyed by user ID and shaped as audit.IdentityLink so the sessions/activity
// admin views can share the same frontend rendering path. Nil-safe: when no
// identity provider is configured (idp == nil) it returns nil and callers fall
// back to showing the raw ID — matching pre-existing behavior. Resolution
// errors are logged and swallowed so the view still renders.
func (a *AdminAPI) buildIdentityLinks(ctx context.Context, userIDs []string) map[string]audit.IdentityLink {
	if a.idp == nil || len(userIDs) == 0 {
		return nil
	}
	users, err := a.idp.LookupMany(ctx, userIDs)
	if err != nil {
		a.log.Warn("admin: identity resolution", "err", err)
		return nil
	}
	if len(users) == 0 {
		return nil
	}
	links := make(map[string]audit.IdentityLink, len(users))
	for id, u := range users {
		display := u.DisplayName
		if display == "" {
			display = u.Username
		}
		if display == "" {
			continue // nothing readable to show; leave the row on its raw ID
		}
		links[id] = audit.IdentityLink{
			Subject:         id,
			PrincipalUserID: id,
			Provider:        "local",
			SubjectType:     audit.UserIDTypeRegistered,
			DisplayName:     display,
		}
	}
	return links
}

// sameOriginRequest reports whether the request originated from the gateway's
// own origin (Sec-Fetch-Site) or an explicitly allowed origin (Origin header).
// Gates cookie-authenticated writes against CSRF — a cross-site form POST
// would otherwise ride the admin's SameSite=None session cookie (issue #788
// review P1). Bearer-authenticated requests never reach this check.
//
// A wildcard "*" in allowedOrigins does NOT satisfy the proof (issue #788
// review P1): CSRF defense must not depend on the operator having narrowed
// the CORS allowlist, and SameSite=None cookies are sent cross-site precisely
// when the allowlist is permissive. Same-origin traffic still passes via the
// Sec-Fetch-Site branch above, so embedded webchat on the gateway origin is
// unaffected; only cross-origin writes need an exact match.
//
// same-site trust assumption (issue #794 P3-2): the Sec-Fetch-Site: same-site
// branch admits a sibling subdomain (e.g. evil.example.com vs
// gateway.example.com — same registrable site) as trusted. This relies on two
// premises: (1) browsers forbid cross-origin documents from forging Fetch
// Metadata headers, so a genuine cross-site attacker cannot claim "same-site";
// (2) within this deploy's registrable site no subdomain hosts user-controlled
// content capable of initiating the write. If either premise fails (multi-
// tenant subdomain hosting, or a compromised sibling subdomain), narrow
// allowedOrigins and rely on the exact Origin-match branch instead.
func (a *AdminAPI) sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "same-site":
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if slices.Contains(a.allowedOriginsFn(), origin) {
		return true
	}
	return false
}

// CSRFMiddleware gates cookie-authenticated state-changing requests against
// cross-origin abuse (issue #788 review P0). /api/admin/* write routes
// (invitations CRUD, user status) and /api/workspaces/* write routes mount on
// the gateway mux and authenticate via the SameSite=None session cookie
// (issue #794 P2-1 extended coverage to workspaces), so they need the same
// defense the Bearer admin port applies via Middleware. GET requests pass
// through; write methods require same-origin proof. Apply inside corsMw so
// OPTIONS is already handled.
//
// Audit (issue #794 P2-2): this middleware runs on the gateway mux, outside
// AdminAPI.Middleware's audit defer, so a 403 would otherwise be silent.
// CSRF denials are high-value security events — log them through the shared
// admin_audit pipeline with the auth-denied action. actor is "anonymous"
// because the cookie is never resolved before the cross-origin proof fails
// (mirrors the Middleware defer's fallback for pre-auth rejections).
func (a *AdminAPI) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) && !a.sameOriginRequest(r) {
			AdminAudit("anonymous", AuditAuthDenied, r.URL.Path, AuditResultDenied)
			web.WriteAppError(w, http.StatusForbidden, "FORBIDDEN", "cross-origin write blocked")
			return
		}
		next.ServeHTTP(w, r)
	})
}
