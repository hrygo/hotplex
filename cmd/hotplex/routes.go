package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hrygo/hotplex/internal/admin"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/docs"
	"github.com/hrygo/hotplex/internal/gateway"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/security"
)

func setupRoutes(
	mux *http.ServeMux,
	deps *GatewayDeps,
) http.Handler {
	log := deps.Log
	cfg := deps.Config
	hub := deps.Hub
	sm := deps.SessionMgr
	auth := deps.Auth
	handler := deps.Handler
	bridge := deps.Bridge
	configWatcher := deps.ConfigWatcher

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	gatewayAPI := gateway.NewGatewayAPI(log, auth, sm, bridge, deps.ConfigStore, deps.EventStore, deps.EventStore, deps.WorkspaceStore)
	if deps.AuditCollector != nil {
		gatewayAPI.SetAuditCollector(deps.AuditCollector)
		sm.SetAuditCollector(deps.AuditCollector)
	}

	// CORS middleware reads allowed origins from config (supports hot-reload).
	corsOriginsFn := func() []string {
		return deps.ConfigStore.Load().Security.AllowedOrigins
	}
	corsMw := security.CORSMiddleware(corsOriginsFn)

	if slices.Contains(cfg.Security.AllowedOrigins, "*") {
		log.Warn("cors: allowed_origins contains '*' — all origins permitted; set security.allowed_origins in production")
	}

	mux.Handle("GET /api/sessions", corsMw(http.HandlerFunc(gatewayAPI.ListSessions)))
	mux.Handle("POST /api/sessions", corsMw(http.HandlerFunc(gatewayAPI.CreateSession)))
	mux.Handle("GET /api/sessions/{id}", corsMw(http.HandlerFunc(gatewayAPI.GetSession)))
	mux.Handle("DELETE /api/sessions/{id}", corsMw(http.HandlerFunc(gatewayAPI.DeleteSession)))
	mux.Handle("POST /api/sessions/{id}/cd", corsMw(http.HandlerFunc(gatewayAPI.SwitchWorkDir)))
	mux.Handle("GET /api/sessions/{id}/history", corsMw(http.HandlerFunc(gatewayAPI.GetHistory)))
	mux.Handle("GET /api/sessions/{id}/events", corsMw(http.HandlerFunc(gatewayAPI.GetEvents)))
	mux.Handle("GET /api/workers", corsMw(http.HandlerFunc(gatewayAPI.ListWorkers)))
	mux.Handle("OPTIONS /api/sessions", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.Handle("OPTIONS /api/sessions/", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.Handle("OPTIONS /api/sessions/{id}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.Handle("OPTIONS /api/sessions/{id}/history", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.Handle("OPTIONS /api/sessions/{id}/events", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.Handle("OPTIONS /api/workers", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

	mux.Handle("GET /ws", hub.HandleHTTP(auth, handler, bridge, deps.CookieAuth))

	sessionAdapter := &sessionManagerAdapter{sm: sm}
	hubAdapter := &hubAdapter{hub: hub}
	bridgeAdapter := &bridgeAdapter{bridge: bridge}
	configAdapter := &configAdapter{cfgStore: deps.ConfigStore}
	configWatcherAdapter := &configWatcherAdapter{watcher: configWatcher}
	turnsAdapter := &turnsStoreAdapter{es: deps.EventStore}

	var cronProvider admin.CronSchedulerProvider
	if deps.CronScheduler != nil {
		cronProvider = &cronAdminAdapter{scheduler: deps.CronScheduler, turnsStore: deps.EventStore, sessionMgr: sm}
	}

	adminAPI := admin.New(admin.Deps{
		Log:              log,
		Config:           configAdapter,
		SessionMgr:       sessionAdapter,
		TurnStats:        turnsAdapter,
		Hub:              hubAdapter,
		Bridge:           bridgeAdapter,
		ConfigWatcher:    configWatcherAdapter,
		Cron:             cronProvider,
		BotLister:        &botListerAdapter{registry: messaging.DefaultBotRegistry()},
		BotConfig:        newBotConfigAdapter(deps.ConfigStore, cfg.AgentConfig.ConfigDir, ""),
		WorkspaceStore:   deps.WorkspaceStore,
		Version:          versionString,
		NewSessionID:     newSessionID,
		AllowedOriginsFn: corsOriginsFn,
		Restart: func() error {
			coordinator := deps.RestartCoordinator
			if coordinator == nil {
				coordinator = newGatewayRestartCoordinator(log, deps.ConfigStore, deps.ConfigPath, deps.DevMode)
			}
			ticket, err := coordinator.Prepare(context.Background(), gatewayRestartRequest{
				Platform: "admin",
				Daemon:   true,
			})
			if err != nil {
				return err
			}
			return coordinator.Commit(ticket)
		},
		RestartPrepare: func(ctx context.Context) (func() error, func() error, error) {
			coordinator := deps.RestartCoordinator
			if coordinator == nil {
				coordinator = newGatewayRestartCoordinator(log, deps.ConfigStore, deps.ConfigPath, deps.DevMode)
			}
			ticket, err := coordinator.Prepare(ctx, gatewayRestartRequest{
				Platform: "admin",
				Daemon:   true,
			})
			if err != nil {
				return nil, nil, adminRestartPrepareError(err)
			}
			return func() error { return coordinator.Commit(ticket) }, func() error { return coordinator.Abort(ticket) }, nil
		},
		DB:           deps.DB,
		DBResolver:   deps.DBResolver,
		KeyValidator: deps.Auth,
		APIKeyStore:  deps.APIKeyStore,
		WriteMu:      deps.WriteMu,
	})

	// Wire audit subsystem (issue #833) - activity endpoints + meta-audit emission
	if deps.AuditStore != nil {
		activitySvc := admin.NewActivityService(deps.AuditStore, log)
		adminAPI.SetActivityService(activitySvc)
	}
	if deps.AuditCollector != nil {
		adminAPI.SetAuditCollector(deps.AuditCollector)
	}
	// Skill management API (issue #910): global skill CRUD via /admin/api/skills.
	if deps.SkillsLocator != nil {
		adminAPI.SetSkillsLocator(deps.SkillsLocator)
	}
	if deps.BuiltinSkillsCatalog != nil {
		adminAPI.SetBuiltinSkillsCatalog(deps.BuiltinSkillsCatalog)
	}
	// Operator fence decisions (#877): list fenced executions + resolve/abandon.
	// Nil store → endpoints answer 503 instead of crashing the mux.
	if deps.ExecutionStore != nil {
		adminAPI.SetRuntimeExecution(
			&executionProviderAdapter{store: deps.ExecutionStore},
			&runtimeEventNotifier{hub: hub},
		)
	}
	// Delivery effect console: inspect effects and their per-attempt history,
	// and apply a bounded operator decision to an uncertain delivery.
	// Nil store → endpoints answer 503 instead of crashing the mux.
	if deps.EffectStore != nil {
		adminAPI.SetRuntimeEffects(&effectProviderAdapter{store: deps.EffectStore})
	}
	// Durable input queue operator actions. The gateway Handler owns queue
	// dispatch, so it is the provider; nil → 503 rather than a fake success.
	if handler != nil {
		adminAPI.SetRuntimeQueue(handler)
	}
	// Execution console (#868): a bounded, redacted projection of a run's
	// history. Wired from three independent read views so a missing one
	// degrades its own section instead of the whole projection.
	if deps.ExecutionStore != nil {
		adminAPI.SetExecutionConsole(
			&executionConsoleAdapter{store: deps.ExecutionStore},
			&consoleEventAdapter{store: deps.EventStore},
			nil,
		)
	}
	if deps.EffectStore != nil {
		adminAPI.SetConsoleEffects(&consoleEffectAdapter{store: deps.EffectStore})
	}
	// Runtime-plan diagnostics report what a session's current run was ACTUALLY
	// launched under, not a re-resolution against the live config (#946 D3).
	// Nil bridge → the diagnostic falls back and marks the answer as such.
	if bridge != nil {
		adminAPI.SetLaunchPlanProvider(bridge)
	}

	if cfg.Admin.RateLimitEnabled {
		limiter := admin.NewRateLimiter(cfg.Admin.RequestsPerSec, cfg.Admin.Burst)
		adminAPI.SetRateLimiter(limiter)

		deps.ConfigStore.RegisterFunc(func(prev, next *config.Config) {
			if prev.Admin.RequestsPerSec != next.Admin.RequestsPerSec || prev.Admin.Burst != next.Admin.Burst {
				limiter.UpdateRate(next.Admin.RequestsPerSec, next.Admin.Burst)
			}
		})
	}
	if cfg.Admin.IPWhitelistEnabled {
		adminAPI.SetAllowedCIDRs(cfg.Admin.AllowedCIDRs)

		deps.ConfigStore.RegisterFunc(func(prev, next *config.Config) {
			if !slices.Equal(prev.Admin.AllowedCIDRs, next.Admin.AllowedCIDRs) {
				adminAPI.SetAllowedCIDRs(next.Admin.AllowedCIDRs)
			}
		})
	}

	adminMux := http.NewServeMux()

	adminMux.HandleFunc("GET /admin/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	adminMux.Handle("GET /admin/metrics", observability.MetricsHandler())

	adminMux.HandleFunc("GET /admin/stats", adminAPI.HandleStats)
	adminMux.HandleFunc("GET /admin/health/workers", adminAPI.HandleWorkerHealth)
	adminMux.HandleFunc("GET /admin/health", adminAPI.HandleHealth)
	adminMux.HandleFunc("GET /admin/logs", adminAPI.HandleLogs)
	adminMux.HandleFunc("POST /admin/config/validate", adminAPI.HandleConfigValidate)
	adminMux.HandleFunc("POST /admin/config/rollback", adminAPI.HandleConfigRollback)
	adminMux.HandleFunc("GET /admin/debug/sessions/{id}", adminAPI.HandleDebugSession)
	adminMux.HandleFunc("POST /admin/restart", adminAPI.HandleRestart)

	adminMux.HandleFunc("GET /admin/sessions", adminAPI.ListSessions)
	adminMux.HandleFunc("GET /admin/sessions/pool", adminAPI.PoolStats)
	adminMux.HandleFunc("GET /admin/sessions/{id}", adminAPI.GetSession)
	adminMux.HandleFunc("DELETE /admin/sessions/{id}", adminAPI.DeleteSession)
	adminMux.HandleFunc("POST /admin/sessions/{id}/terminate", adminAPI.TerminateSession)
	adminMux.HandleFunc("GET /admin/sessions/{id}/stats", adminAPI.HandleSessionStats)
	adminMux.HandleFunc("GET /admin/sessions/{id}/runtime-plan", adminAPI.HandleSessionRuntimePlan)

	// Runtime fence API (#877): inspect fenced executions, resolve/abandon with
	// a fencing token. GET needs runtime:read, POST needs runtime:write.
	adminMux.HandleFunc("GET /admin/executions/fences", adminAPI.HandleListFences)
	adminMux.HandleFunc("POST /admin/executions/{id}/fence-action", adminAPI.HandleFenceAction)

	// Execution console (#868): browse runs and open one run's bounded,
	// redacted timeline. Reads only; the decisions live at the fence, queue
	// and effect endpoints above, which stay separate on purpose.
	adminMux.HandleFunc("GET /admin/executions", adminAPI.ListExecutions)
	adminMux.HandleFunc("GET /admin/executions/{id}/timeline", adminAPI.GetExecutionTimeline)

	// Durable input queue operator actions: withdraw promises that have not
	// been dispatched yet. Separate from the fence endpoints because a queued
	// input is not a broken run — cancelling one must never be reported as
	// having unblocked or failed an execution.
	adminMux.HandleFunc("POST /admin/executions/{id}/queue-cancel", adminAPI.HandleCancelQueuedInput)
	adminMux.HandleFunc("POST /admin/sessions/{id}/queue-clear", adminAPI.HandleClearSessionQueue)

	// Delivery effect API: inspect external deliveries and their attempts, and
	// decide an uncertain one. Separate from the fence endpoints on purpose —
	// a delivery decision must never unblock a run, and a fence decision must
	// never rewrite delivery history. GET needs runtime:read, POST needs
	// runtime:write.
	adminMux.HandleFunc("GET /admin/effects", adminAPI.HandleListEffects)
	adminMux.HandleFunc("GET /admin/effects/{id}", adminAPI.HandleGetEffect)
	adminMux.HandleFunc("POST /admin/effects/{id}/action", adminAPI.HandleEffectAction)

	// User activity API (issue #833) - by-user audit query + export
	adminMux.HandleFunc("GET /admin/users/{id}/activity", adminAPI.HandleUserActivity)
	adminMux.HandleFunc("GET /admin/users/{id}/activity/export", adminAPI.HandleUserActivityExport)
	adminMux.HandleFunc("GET /admin/activity", adminAPI.HandleAdminActivity)
	adminMux.HandleFunc("GET /admin/activity/stats", adminAPI.HandleActivityStats)
	adminMux.HandleFunc("GET /admin/activity/export", adminAPI.HandleAdminActivityExport)
	adminMux.HandleFunc("GET /admin/audit/identity-links", adminAPI.HandleAuditIdentityLinks)
	adminMux.HandleFunc("POST /admin/audit/identity-links", adminAPI.HandleCreateAuditIdentityLink)
	adminMux.HandleFunc("DELETE /admin/audit/identity-links/{id}", adminAPI.HandleDeleteAuditIdentityLink)

	// Cron API
	adminMux.HandleFunc("GET /admin/cron/jobs", adminAPI.HandleCronList)
	adminMux.HandleFunc("GET /admin/cron/jobs/{id}", adminAPI.HandleCronGet)
	adminMux.HandleFunc("POST /admin/cron/jobs", adminAPI.HandleCronCreate)
	adminMux.HandleFunc("PATCH /admin/cron/jobs/{id}", adminAPI.HandleCronUpdate)
	adminMux.HandleFunc("DELETE /admin/cron/jobs/{id}", adminAPI.HandleCronDelete)
	adminMux.HandleFunc("POST /admin/cron/jobs/{id}/run", adminAPI.HandleCronTrigger)
	adminMux.HandleFunc("GET /admin/cron/jobs/{id}/runs", adminAPI.HandleCronRunHistory)

	// Bot status API
	adminMux.HandleFunc("GET /admin/bots", adminAPI.HandleListBots)
	adminMux.HandleFunc("GET /admin/bots/{name}", adminAPI.HandleGetBot)

	// Bot config API
	adminMux.HandleFunc("GET /admin/bots/config", adminAPI.HandleListBotConfigs)
	adminMux.HandleFunc("GET /admin/bots/{name}/config", adminAPI.HandleGetBotConfig)
	adminMux.HandleFunc("GET /admin/bots/{name}/config/{file}", adminAPI.HandleGetAgentConfigFile)
	adminMux.HandleFunc("GET /admin/bots/{name}/preview", adminAPI.HandleSystemPromptPreview)
	adminMux.HandleFunc("PATCH /admin/bots/{name}", adminAPI.HandleUpdateBotConfig)
	adminMux.HandleFunc("POST /admin/bots", adminAPI.HandleCreateBot)
	adminMux.HandleFunc("DELETE /admin/bots/{name}", adminAPI.HandleDeleteBot)
	adminMux.HandleFunc("PUT /admin/bots/{name}/config/{file}", adminAPI.HandleWriteAgentConfigFile)

	// Channel default config API (platform-level team defaults). Platforms
	// without a bot instance — webchat owns dir/webchat/ team defaults but
	// never appears in the bot registry — need a dedicated entry point that
	// addresses the dir/{platform}/ layer directly. The literal "platform"
	// segment is more specific than the {name} wildcard above, so these routes
	// never collide with the bot-level routes (different segment counts). #796
	adminMux.HandleFunc("GET /admin/bots/platform/{platform}/config/{file}", adminAPI.HandleGetPlatformAgentConfigFile)
	adminMux.HandleFunc("PUT /admin/bots/platform/{platform}/config/{file}", adminAPI.HandleWritePlatformAgentConfigFile)

	// API key user management
	adminMux.HandleFunc("GET /admin/api-keys", adminAPI.HandleAPIKeyUserList)
	adminMux.HandleFunc("POST /admin/api-keys", adminAPI.HandleAPIKeyUserCreate)
	adminMux.HandleFunc("GET /admin/api-keys/{id}", adminAPI.HandleAPIKeyUserGet)
	adminMux.HandleFunc("PATCH /admin/api-keys/{id}", adminAPI.HandleAPIKeyUserUpdate)
	adminMux.HandleFunc("DELETE /admin/api-keys/{id}", adminAPI.HandleAPIKeyUserDelete)

	// Workspace admin console (issue #807): global list (with owner identity) +
	// inline permission_mode edit. Admin-only via AdminAPI.Middleware; the user
	// self-service /api/workspaces/* endpoints stay separate (registered below at
	// /api/workspaces with Authenticator + CSRF, not admin scope).
	adminMux.HandleFunc("GET /admin/workspaces", adminAPI.HandleListAdminWorkspaces)
	adminMux.HandleFunc("PATCH /admin/workspaces/{id}", adminAPI.HandleUpdateAdminWorkspacePermissionMode)

	// Skill management API (issue #910): global skill list/install/read/update/delete.
	adminMux.HandleFunc("GET /admin/api/skills", adminAPI.HandleListSkills)
	adminMux.HandleFunc("POST /admin/api/skills", adminAPI.HandleInstallSkill)
	adminMux.HandleFunc("GET /admin/api/skills/{name}", adminAPI.HandleGetSkill)
	adminMux.HandleFunc("PUT /admin/api/skills/{name}", adminAPI.HandleUpdateSkill)
	adminMux.HandleFunc("DELETE /admin/api/skills/{name}", adminAPI.HandleDeleteSkill)

	// Documentation
	if resolved := security.ResolveCSP(security.DefaultDocsCSP, cfg.Security.CSP); security.IsPermissiveCSP(resolved) {
		log.Warn("csp: docs policy is permissive (any http/https/ws/wss host allowed); set security.csp to restrict in production",
			"service", "docs")
	}
	mux.Handle("GET /docs/", http.StripPrefix("/docs", docs.Handler(cfg.Security.CSP)))

	// Webhook endpoint (GitHub → HotPlex event-driven triggers)
	if cfg.Webhook.Enabled && deps.CronScheduler != nil {
		if cfg.Webhook.Secret == "" {
			log.Warn("webhook enabled but secret is empty — rejecting for security")
		} else {
			webhookHandler := gateway.NewWebhookHandler(
				deps.Ctx,
				cfg.Webhook,
				deps.CronScheduler,
				log,
			)
			deps.WebhookHandler = webhookHandler
			mux.Handle(cfg.Webhook.Path, webhookHandler)
			log.Info("webhook handler registered", "path", cfg.Webhook.Path)
		}
	}

	// bootstrap-status is intentionally registered OUTSIDE the CookieAuth-gated
	// auth block below: it must stay reachable before any admin exists (the very
	// state the login page needs to detect). Only requires the workspace store.
	if deps.WorkspaceStore != nil {
		mux.Handle("GET /api/auth/bootstrap-status", corsMw(gateway.BootstrapStatus(deps.WorkspaceStore)))
		mux.Handle("OPTIONS /api/auth/bootstrap-status", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	}

	// WebChat multi-tenant auth endpoints (spec ① + spec ④).
	// Wired when cookieAuth is available (webchat enabled).
	if deps.CookieAuth != nil && deps.WorkspaceStore != nil {
		// Account-login handlers (spec ①): requires LocalAccountProvider.
		// LocalAccountProvider is created lazily from WorkspaceStore + bcrypt cost.
		lap := security.NewLocalAccountProvider(deps.WorkspaceStore, security.BcryptCostDefault)
		auth.SetIdentityProvider(lap)
		// Enable cookie-session fallback on the Bearer admin port so embedded
		// webchat admins can reach Dashboard/Bots/Cron without a separate admin
		// token (issue #788 A2). No-op semantics live in AdminAPI.Middleware.
		adminAPI.SetCookieFallback(deps.CookieAuth, lap)

		authHandlers := gateway.NewAuthHandlers(auth, deps.CookieAuth, deps.WorkspaceStore, lap)
		// /api/admin/* handlers (invitations/users CRUD) live in the admin package
		// (P2.6) but mount on the gateway mux with cookie auth — WebChat admin UI
		// uses cookie sessions, not the Bearer+scope tokens of the admin port.
		userAdmin := admin.NewUserAdminHandlers(deps.WorkspaceStore, auth, deps.CookieAuth, lap)
		userAdmin.SetAuditCollector(deps.AuditCollector)
		mux.Handle("POST /api/auth/login", corsMw(http.HandlerFunc(authHandlers.Login)))
		mux.Handle("POST /api/auth/logout", corsMw(http.HandlerFunc(authHandlers.Logout)))
		mux.Handle("GET /api/auth/me", corsMw(http.HandlerFunc(authHandlers.Me)))
		mux.Handle("POST /api/auth/accept-invite", corsMw(http.HandlerFunc(authHandlers.AcceptInvite)))

		// App-level Admin endpoints. /api/admin/* authenticates via the
		// SameSite=None session cookie, so state-changing routes need the
		// same CSRF defense the Bearer admin port applies. csrfMw wraps the
		// write methods (POST/DELETE/PATCH); GETs pass through (issue #788
		// review P0). Inside corsMw so OPTIONS preflight is handled first.
		csrfMw := adminAPI.CSRFMiddleware
		mux.Handle("POST /api/admin/invitations", corsMw(userAdmin.AuditWrite(admin.AuditInvitationCreate, csrfMw(http.HandlerFunc(userAdmin.CreateInvitation)))))
		mux.Handle("GET /api/admin/invitations", corsMw(http.HandlerFunc(userAdmin.ListInvitations)))
		mux.Handle("DELETE /api/admin/invitations/{id}", corsMw(userAdmin.AuditWrite(admin.AuditInvitationDelete, csrfMw(http.HandlerFunc(userAdmin.DeleteInvitation)))))
		mux.Handle("GET /api/admin/users", corsMw(http.HandlerFunc(userAdmin.ListUsers)))
		mux.Handle("PATCH /api/admin/users/{id}", corsMw(userAdmin.AuditWrite(admin.AuditMemberStatusUpdate, csrfMw(http.HandlerFunc(userAdmin.UpdateUserStatus)))))
		mux.Handle("POST /api/admin/users/{id}/password", corsMw(userAdmin.AuditWrite(admin.AuditUserPasswordReset, csrfMw(http.HandlerFunc(userAdmin.ResetUserPassword)))))

		// Embedded admin activity console. The standalone admin server exposes
		// the same handlers at /admin/* for Bearer-token clients; the webchat UI
		// needs same-origin cookie-authenticated routes that do not collide with
		// the SPA /admin/activity page.
		mux.Handle("GET /api/admin/activity", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleAdminActivity))))
		mux.Handle("GET /api/admin/activity/stats", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleActivityStats))))
		mux.Handle("GET /api/admin/activity/export", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleAdminActivityExport))))
		mux.Handle("GET /api/admin/audit/identity-links", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleAuditIdentityLinks))))
		mux.Handle("POST /api/admin/audit/identity-links", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleCreateAuditIdentityLink))))
		mux.Handle("DELETE /api/admin/audit/identity-links/{id}", corsMw(adminAPI.Middleware(http.HandlerFunc(adminAPI.HandleDeleteAuditIdentityLink))))

		// OPTIONS preflight handlers for Auth & Admin APIs
		mux.Handle("OPTIONS /api/auth/login", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/auth/logout", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/auth/me", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/auth/accept-invite", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/invitations", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/invitations/", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/invitations/{id}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/users", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/users/", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/users/{id}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/users/{id}/password", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/activity", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/activity/stats", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/activity/export", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/audit/identity-links", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/admin/audit/identity-links/{id}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

		// Workspaces CRUD endpoints. Write routes (POST/PATCH/DELETE) mount
		// csrfMw for the same reason /api/admin/* does (issue #794 P2-1):
		// WorkspaceHandlers.currentUser → AuthenticateRequest falls back to
		// AuthenticateActiveCookie when no api-key is present — the same
		// SameSite=None cookie channel, so cross-site blind CSRF could create /
		// mutate / delete a workspace. GETs pass through. Inside corsMw so
		// OPTIONS preflight is handled first; csrfMw's 403s are audited
		// (admin.go CSRFMiddleware, issue #794 P2-2).
		wsHandlers := gateway.NewWorkspaceHandlers(deps.WorkspaceStore, auth)
		mux.Handle("POST /api/workspaces", corsMw(csrfMw(http.HandlerFunc(wsHandlers.Create))))
		mux.Handle("GET /api/workspaces", corsMw(http.HandlerFunc(wsHandlers.List)))
		mux.Handle("GET /api/workspaces/{id}", corsMw(http.HandlerFunc(wsHandlers.Get)))
		mux.Handle("PATCH /api/workspaces/{id}", corsMw(csrfMw(http.HandlerFunc(wsHandlers.Update))))
		mux.Handle("DELETE /api/workspaces/{id}", corsMw(csrfMw(http.HandlerFunc(wsHandlers.Delete))))

		// Skill management API (issue #910): user-facing merged list + workspace
		// skill CRUD. Write routes (POST/DELETE) mount csrfMw for the same
		// SameSite=None cookie CSRF reason /api/workspaces does; GETs pass through.
		skillHandlers := gateway.NewSkillHandlers(deps.SkillsLocator, deps.WorkspaceStore, auth, log)
		if deps.BuiltinSkillsCatalog != nil {
			skillHandlers.SetBuiltinSkillsCatalog(deps.BuiltinSkillsCatalog)
		}
		if deps.AuditCollector != nil {
			skillHandlers.SetAuditCollector(deps.AuditCollector)
		}
		mux.Handle("GET /api/skills", corsMw(http.HandlerFunc(skillHandlers.ListMerged)))
		mux.Handle("GET /api/skills/{name}", corsMw(http.HandlerFunc(skillHandlers.GetMerged)))
		mux.Handle("GET /api/workspaces/{wid}/skills", corsMw(http.HandlerFunc(skillHandlers.ListWorkspace)))
		mux.Handle("POST /api/workspaces/{wid}/skills", corsMw(csrfMw(http.HandlerFunc(skillHandlers.InstallWorkspace))))
		mux.Handle("GET /api/workspaces/{wid}/skills/{name}", corsMw(http.HandlerFunc(skillHandlers.GetWorkspace)))
		mux.Handle("DELETE /api/workspaces/{wid}/skills/{name}", corsMw(csrfMw(http.HandlerFunc(skillHandlers.DeleteWorkspace))))
		mux.Handle("OPTIONS /api/skills", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/skills/", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/workspaces/{wid}/skills", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/workspaces/{wid}/skills/{name}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

		// OPTIONS preflight handlers for Workspaces API
		mux.Handle("OPTIONS /api/workspaces", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/workspaces/", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
		mux.Handle("OPTIONS /api/workspaces/{id}", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

		log.Info("auth, admin, and workspaces endpoints registered", "channels", "login,logout,me,accept-invite,workspaces,admin")

		// OAuth SSO handlers (spec ④). Register the full route set whenever
		// OAuthManager exists so runtime config reload can add or remove
		// providers without requiring a gateway restart.
		if deps.OAuthManager != nil {
			oauthHandlers := gateway.NewOAuthHandlers(deps.OAuthManager, deps.CookieAuth, auth, deps.WorkspaceStore, log)
			mux.Handle("GET /api/auth/oauth/providers", corsMw(http.HandlerFunc(oauthHandlers.Providers)))
			mux.Handle("GET /api/auth/oauth/{provider}/login", http.HandlerFunc(oauthHandlers.Login))
			mux.Handle("GET /api/auth/oauth/{provider}/callback", http.HandlerFunc(oauthHandlers.Callback))
			// Note: login/callback are redirect flows, CORS not needed (browser navigates directly).
			log.Info("oauth SSO endpoints registered", "providers", deps.OAuthManager.List())
		} else {
			// Always expose providers discovery so a cross-origin browser gets
			// 200 + CORS headers instead of a CORS-masked 404 spamming the login
			// console when SSO is unconfigured. Returns an empty list.
			mux.Handle("GET /api/auth/oauth/providers", corsMw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"providers":[]}`))
			})))
		}

		// OPTIONS preflight for OAuth providers discovery (cross-origin fetch
		// from the webchat dev server). Without this, ServeMux returns 405
		// (no CORS headers) and the browser blocks getOAuthProviders() with a
		// CORS error. Mirrors the /api/auth/* OPTIONS pattern above; registered
		// once outside the if/else so it covers both the configured-providers
		// branch and the empty-list fallback.
		mux.Handle("OPTIONS /api/auth/oauth/providers", corsMw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	}

	// Global favicon fallback using docs logo
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/assets/logo.png", http.StatusMovedPermanently)
	})

	// security.txt (RFC 9116) — contact info from config, no hardcoded domains.
	mux.Handle("GET /.well-known/security.txt", security.SecurityTxtHandler(
		func() string { return deps.ConfigStore.Load().Security.SecurityContact },
	))

	// Webchat SPA is NOT registered on the mux directly.
	// Instead, the caller wraps the mux with a fallback handler below.
	h := adminAPI.Middleware(adminMux)
	return otelhttp.NewHandler(h, "hotplex-gateway",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

func adminRestartPrepareError(err error) error {
	if err == nil {
		return nil
	}
	if _, conflict := restartConflictRequestID(err); conflict {
		return fmt.Errorf("%w: %w", admin.ErrRestartConflict, err)
	}
	return err
}
