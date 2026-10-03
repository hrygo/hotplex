package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/hrygo/hotplex/internal/admin"
	"github.com/hrygo/hotplex/internal/assets"
	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/audit/sinks"
	"github.com/hrygo/hotplex/internal/brain"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/gateway"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/skills"
	"github.com/hrygo/hotplex/internal/skills/builtin"
	"github.com/hrygo/hotplex/internal/skills/reconcile"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/internal/webchat"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/acp"
	"github.com/hrygo/hotplex/internal/worker/claudecode"
	"github.com/hrygo/hotplex/internal/worker/codexcli"
	"github.com/hrygo/hotplex/internal/worker/opencodeserver"
	"github.com/hrygo/hotplex/internal/worker/proc"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// eventStoreProvider combines the query and event-store interfaces needed by all consumers.
// Both *eventstore.SQLiteStore and the internal pgEventStore satisfy it. LatestSeq is
// inherited from TurnQuerier (promoted there in issue #879).
type eventStoreProvider interface {
	eventstore.TurnQuerier
	QueryBySession(ctx context.Context, sessionID string, cursor int64, dir eventstore.CursorDirection, limit int) (*eventstore.EventPage, error)
	DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error)
	Close() error
}

// sinkAdapter bridges sinks.Sink (which uses sinks.AlertEvent) to audit.AlertSink
// (which uses audit.AuditEvent). The two event types have identical fields but live
// in different packages to avoid circular imports.
type sinkAdapter struct {
	sink sinks.Sink
}

func (a *sinkAdapter) OnAuditEvent(ctx context.Context, e audit.AuditEvent) error {
	return a.sink.OnAlertEvent(ctx, sinks.AlertEvent{
		EventID:      e.EventID,
		Ts:           e.Ts,
		UserID:       e.UserID,
		UserIDType:   e.UserIDType,
		Platform:     e.Platform,
		SessionID:    e.SessionID,
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		Outcome:      e.Outcome,
		Detail:       e.Detail,
		EventRef:     e.EventRef,
		IP:           e.IP,
		UserAgent:    e.UserAgent,
	})
}

func (a *sinkAdapter) Close(ctx context.Context) error {
	if closer, ok := a.sink.(sinks.Closer); ok {
		return closer.Close(ctx)
	}
	return nil
}

// emitAuditConfigChange returns a ConfigStore observer callback that emits a
// system.audit_config_changed meta-audit row whenever any audit config field
// changes at runtime (spec §5.2). The callback is non-blocking and silently
// ignores enqueue errors to keep the config-reload path fast. Fields requiring
// restart are still recorded so the audit trail is durable.
func emitAuditConfigChange(c *audit.Collector) func(prev, next *config.Config) {
	return func(prev, next *config.Config) {
		changes := auditConfigDiff(prev.Audit, next.Audit)
		if len(changes) == 0 {
			return
		}
		detail, _ := json.Marshal(map[string]any{
			"changes": changes,
		})
		ua := &audit.UserActivity{
			Ts:         time.Now().UnixMilli(),
			UserID:     "system",
			UserIDType: audit.UserIDTypeSystem,
			Platform:   audit.PlatformAdmin,
			Action:     audit.ActionSystemAuditConfigChanged,
			Outcome:    audit.OutcomeSuccess,
			DetailJSON: string(detail),
		}
		_ = c.Enqueue(context.Background(), ua)
	}
}

// auditConfigDiff returns a list of field-level changes between two AuditConfig
// values. Each entry is {field, old, new}. Only top-level + collector + sinks
// fields are tracked — these are the fields a security reviewer needs to see.
func auditConfigDiff(prev, next config.AuditConfig) []map[string]string {
	var changes []map[string]string
	add := func(field, oldV, newV string) {
		if oldV != newV {
			changes = append(changes, map[string]string{
				"field": field, "old": oldV, "new": newV,
			})
		}
	}
	add("audit.enabled", strconv.FormatBool(prev.Enabled), strconv.FormatBool(next.Enabled))
	add("audit.retention", prev.Retention.String(), next.Retention.String())
	add("audit.full_content_retention", prev.FullContentRetention.String(), next.FullContentRetention.String())
	add("audit.collector.channel_cap", strconv.Itoa(prev.Collector.ChannelCap), strconv.Itoa(next.Collector.ChannelCap))
	add("audit.collector.batch_interval", prev.Collector.BatchInterval.String(), next.Collector.BatchInterval.String())
	add("audit.collector.batch_size", strconv.Itoa(prev.Collector.BatchSize), strconv.Itoa(next.Collector.BatchSize))
	add("audit.collector.spill_dir", prev.Collector.SpillDir, next.Collector.SpillDir)
	// Compare raw values so secret-only changes remain observable, but serialize
	// only sanitized copies into the tamper-evident audit trail.
	prevSinksRaw, _ := json.Marshal(prev.Sinks)
	nextSinksRaw, _ := json.Marshal(next.Sinks)
	if string(prevSinksRaw) != string(nextSinksRaw) {
		changes = append(changes, map[string]string{
			"field": "audit.sinks",
			"old":   sanitizedJSONString(prev.Sinks),
			"new":   sanitizedJSONString(next.Sinks),
		})
	}
	return changes
}

func sanitizedJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "null"
	}
	sanitized, err := json.Marshal(audit.SanitizeValue(decoded))
	if err != nil {
		return "null"
	}
	return string(sanitized)
}

// GatewayDeps holds all dependencies constructed during gateway initialization.
// These are passed to various components and registrations.
type GatewayDeps struct {
	Log                *slog.Logger
	Ctx                context.Context // gateway lifecycle context for graceful shutdown
	Config             *config.Config
	ConfigStore        *config.ConfigStore
	Hub                *gateway.Hub
	SessionMgr         *session.Manager
	EventStore         eventStoreProvider
	EventCollector     *eventstore.Collector
	ExecutionStore     execution.Store
	Auth               *security.Authenticator
	Handler            *gateway.Handler
	Bridge             *gateway.Bridge
	ConfigWatcher      *config.Watcher
	CronScheduler      *cron.Scheduler
	WebhookHandler     *gateway.WebhookHandler // non-nil when webhook is enabled
	CookieAuth         *security.CookieAuth    // non-nil when webchat is enabled
	OAuthManager       *security.OAuthManager  // non-nil when SSO providers are configured
	ChatAccessStore    messaging.ChatAccessStorer
	DB                 *sql.DB
	DBResolver         *security.DBResolver
	APIKeyStore        admin.APIKeyUserStorer
	WorkspaceStore     session.UserWorkspaceStore
	WriteMu            *sqlutil.WriteMu
	ConfigPath         string
	DevMode            bool
	RestartCoordinator *gatewayRestartCoordinator
	// Audit subsystem (issue #833 P1). Nil when audit.enabled=false.
	AuditCollector *audit.Collector
	AuditStore     audit.Store
	// SkillsLocator serves the skill management HTTP API (issue #910).
	SkillsLocator *skills.Locator
	// BuiltinSkillsCatalog serves embedded Agent Skills read metadata. It is
	// independent of native projection/inventory state.
	BuiltinSkillsCatalog builtin.PublicCatalog
	// Durable ingress reliability closure (spec 2026-07-14).
	OwnerInstanceID string
	LeaseManager    *execution.LeaseManager
	Repairer        *execution.Repairer
}

func configFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVarP(target, "config", "c", config.DefaultConfigPath(), "config file path")
}

func runGateway(configPath string, devMode bool, stopCh <-chan struct{}) (err error) { //nolint:unparam // stopCh used by Windows service wrapper
	defer func() {
		if err != nil {
			removeGatewayState()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ownerInstanceID := "gw-" + uuid.NewString()

	cfg, err := loadConfig(configPath, devMode)
	if err != nil {
		return err
	}

	if validationErrs := cfg.Validate(); len(validationErrs) > 0 {
		return fmt.Errorf("config validation failed: %v", validationErrs)
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(os.Stderr, "config warning: %s\n", w)
	}

	// Extract embedded Python scripts to ~/.hotplex/scripts
	scriptsDir := filepath.Join(config.HotplexHome(), "scripts")
	if err := assets.InstallScripts(scriptsDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: assets: script extraction failed: %s\n", err)
	}

	log, cfgStore, levelVar := initLogging(cfg)
	pidTracker, cleanupWG := initOrphanCleanup(ctx, cfg, log)
	cleanupStaleTempFiles(log)

	obsCfg := observability.DefaultConfig()
	obsCfg.ServiceVersion = versionString()
	observability.Init(ctx, log, obsCfg)
	log.Info("gateway: starting",
		"go", runtime.Version(),
		"addr", cfg.Gateway.Addr,
		"config", configPath,
	)

	stores, err := initStores(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer stores.close(log)

	// Audit subsystem (issue #833 P1): construct Store + Collector + GC + Verifier
	// when audit.enabled=true. The collector batches events and fans out to sinks.
	// GC prunes old rows; Verifier checks hash chain integrity.
	var auditCollector *audit.Collector
	var auditStore audit.Store
	var auditGC *audit.GC
	if cfg.Audit.Enabled {
		auditStore, err = audit.NewStore(stores.sqlDB, stores.dialect, stores.writeMu, log)
		if err != nil {
			return fmt.Errorf("audit store init: %w", err)
		}

		var auditSinks []audit.AlertSink
		for _, sc := range cfg.Audit.Sinks {
			s, sErr := sinks.Build(sc.Type, sc.Config, log)
			if sErr != nil {
				log.Warn("audit: sink build failed; using noop", "name", sc.Name, "type", sc.Type, "err", sErr)
				s = sinks.NewNoopSink()
			}
			auditSinks = append(auditSinks, &sinkAdapter{sink: s})
		}
		if len(auditSinks) == 0 {
			auditSinks = []audit.AlertSink{&sinkAdapter{sink: sinks.NewNoopSink()}}
		}

		spillDir := cfg.Audit.Collector.SpillDir
		if spillDir == "" {
			spillDir = filepath.Join(config.HotplexHome(), "data", "audit-spill")
		}
		if mkErr := os.MkdirAll(spillDir, 0o700); mkErr != nil {
			log.Warn("audit: spill dir create", "dir", spillDir, "err", mkErr)
		}
		spillFile := filepath.Join(spillDir, "audit-spill.wal")
		spill, spillErr := audit.OpenSpill(spillFile)
		if spillErr != nil {
			return fmt.Errorf("audit spill open: %w", spillErr)
		}

		auditCollector = audit.NewCollector(auditStore, spill, auditSinks, log, audit.CollectorConfig{
			ChannelCap:        cfg.Audit.Collector.ChannelCap,
			BatchSize:         cfg.Audit.Collector.BatchSize,
			BatchInterval:     cfg.Audit.Collector.BatchInterval,
			SinkTimeout:       5 * time.Second,
			SpillBlockTimeout: 5 * time.Second,
		})
		auditCollector.Start(ctx)

		auditGC = audit.NewGC(auditStore, audit.GCConfig{
			Retention: cfg.Audit.Retention,
			Interval:  1 * time.Hour,
		}, log)
		auditVerifier := audit.NewVerifier(auditStore, audit.VerifierConfig{
			Interval: 1 * time.Hour,
		}, log)
		go auditGC.Run(ctx)
		go auditVerifier.Run(ctx)

		log.Info("audit: subsystem initialized",
			"retention", cfg.Audit.Retention,
			"channel_cap", cfg.Audit.Collector.ChannelCap,
			"batch_size", cfg.Audit.Collector.BatchSize,
			"sinks", len(auditSinks),
		)
	}

	sm, err := session.NewManager(ctx, log, cfg, cfgStore, stores.session)
	if err != nil {
		return err
	}
	var cronScheduler *cron.Scheduler
	var cronDelivery *cron.Delivery
	var cronAttRouter *cronAttachedRouter
	// cronEffectOwners resolves the delivery owner for gateway-mode cron jobs.
	// It is created before the adapters start and populated afterwards, because
	// the scheduler is wired earlier than the messaging adapters.
	cronEffectOwners := &adapterLookup{}
	var cronEffectDelivery cron.EffectDelivery
	// bridge is forward-declared (and assigned later at NewBridge) so this
	// closure can clear busy-supplement buffers on session end. Mirrors the
	// existing cronScheduler pattern. Nil before assignment — ClearPending is
	// nil-safe via b.pending guard, but the bridge pointer itself is checked
	// here to keep the test path (no gateway_run wiring) honest.
	var bridge *gateway.Bridge

	sm.OnTerminate = func(sessionID string) {
		log.Info("gateway: session terminated", "session_id", sessionID)
		// SESSION_BUSY mid-turn cleanup (Task 11): the session is gone (either
		// TERMINATED or DELETED — see manager.go:notifyStateChange), so any
		// buffered supplements are stale. Clearing here funnels every end-of-
		// life path (client /terminate, /delete, /gc, crash cleanup, GC sweep,
		// HTTP DELETE) through a single hook instead of patching each call site.
		if bridge != nil {
			bridge.ClearPending(sessionID)
		}
		if cronScheduler != nil {
			cronScheduler.CleanupForSession(sessionID)
		}
	}
	// Wait for orphan process cleanup to finish before repairing sessions.
	cleanupWG.Wait()
	if cleanupStore, ok := stores.session.(session.CleanupTaskStore); ok {
		go session.NewCleanupRunner(log, cleanupStore, worker.CleanupSession).Run(ctx)
	} else {
		log.Warn("gateway: durable session cleanup unavailable for session store")
	}

	repaired, repairErr := sm.RepairRunningSessions(ctx)
	if repairErr != nil {
		log.Warn("gateway: session state repair failed", "err", repairErr)
	} else if repaired > 0 {
		log.Info("gateway: repaired orphaned sessions", "count", repaired)
	}

	hub := gateway.NewHub(log, cfgStore)
	hub.LogHandler = func(level, msg, sessionID string) {
		admin.AddLog(level, msg, sessionID)
	}
	// Hydrate SeqGen from persisted events on reconnect so seq continues
	// monotonically instead of restarting from 1 (issue #879).
	hub.SetSeqHydrator(stores.event)
	hub.SetSeqSessionExists(func(sessionID string) bool {
		return sm.IsSeqActive(context.Background(), sessionID)
	})
	// Drain the collector before hydrating SeqGen on reconnect so LatestSeq
	// includes events that were allocated seqs but not yet committed (issue #894).
	if stores.collector != nil {
		hub.SetSeqFlusher(stores.collector)
	}
	var handler *gateway.Handler // set later in DI; prunes session catalog state on release

	sm.OnRuntimeRelease = func(ctx context.Context, sessionID string) {
		// Prune per-session command-catalog state so deleted sessions do not
		// accumulate one catalogGen/entries entry for the gateway lifetime
		// (mirrors hub.ReleaseSeq below).
		if handler != nil {
			handler.ReleaseSession(sessionID)
		}
		err := hub.ReleaseSeq(sessionID, func() error {
			// A zero value means no durable sequence was allocated; remove a
			// possible hydrated-empty entry without forcing a collector flush.
			if hub.NextSeqPeek(sessionID) == 0 || stores.collector == nil {
				return nil
			}
			return stores.collector.FlushSession(sessionID)
		})
		if err != nil {
			log.Warn("gateway: retain seq after session flush failure",
				"session_id", sessionID, "err", err)
		}
	}

	var configWatcher *config.Watcher
	if configPath != "" {
		configWatcher = config.NewWatcher(log, configPath, cfgStore,
			func(newCfg *config.Config) {
				log.Info("config: hot reload applied",
					"gateway_addr", newCfg.Gateway.Addr,
					"pool_max_size", newCfg.Pool.MaxSize,
					"gc_scan_interval", newCfg.Session.GCScanInterval,
				)
			},
			func(field string) {
				log.Warn("config: static field changed, restart required to apply",
					"field", field,
				)
			},
		)
		configWatcher.SetInitial(cfg)
	}

	// Config hot-reload callbacks
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Log.Level != next.Log.Level {
			var newLevel slog.Level
			if err := newLevel.UnmarshalText([]byte(next.Log.Level)); err == nil {
				levelVar.Set(newLevel)
				log.Info("config: log level updated", "old", prev.Log.Level, "new", next.Log.Level)
			}
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Pool != next.Pool {
			sm.Pool().UpdateLimits(next.Pool)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Session.GCScanInterval != next.Session.GCScanInterval {
			sm.ResetGCInterval(next.Session.GCScanInterval)
		}
	})
	// Audit config-change meta-audit (spec §5.2): changes to audit config are
	// themselves audited. Non-blocking; no-op when audit is disabled. Each reload
	// emits a single system.audit_config_changed row with the full field-level
	// diff in detail_json. Fields requiring restart (enabled / spill_dir /
	// channel_cap) are also recorded here so there is a durable trail even though
	// they are not actually applied until restart.
	if auditCollector != nil {
		cfgStore.RegisterFunc(emitAuditConfigChange(auditCollector))
	}

	sm.StateNotifier = func(ctx context.Context, sessionID string, state events.SessionState, message string) {
		var seq int64
		if state == events.StateDeleted {
			seq = hub.NextSeqBeforeRelease(sessionID)
		} else {
			releaseSeq, ok := hub.BeginSeqOperation(sessionID)
			if !ok {
				return
			}
			defer releaseSeq()
			seq = hub.NextSeqHeld(sessionID)
		}
		if seq == 0 {
			return
		}
		env := events.NewEnvelope(aep.NewID(), sessionID, seq, events.State, events.StateData{
			State:   state,
			Message: message,
		})
		_ = hub.SendToSession(ctx, env)
	}

	auth := security.NewAuthenticator(&cfg.Security)
	if auditCollector != nil {
		auth.SetAuditCollector(auditCollector)
	}

	// API key → user identity resolver: YAML config takes priority over DB (Admin API CRUD).
	// ChainResolver tries config map first, falls back to DB. Either source may be empty.
	dbResolver := stores.dbResolver
	if len(cfg.ResolvedAPIKeyUsers) > 0 {
		mapResolver := security.NewMapResolver(cfg.ResolvedAPIKeyUsers)
		auth.SetKeyResolver(security.NewChainResolver(mapResolver, dbResolver))
		log.Info("gateway: API key resolver: config → database",
			"mapped_config_keys", len(cfg.ResolvedAPIKeyUsers))
	} else {
		auth.SetKeyResolver(dbResolver)
		log.Info("gateway: API key resolver: database")
	}

	// Preload database-sourced API keys into Phase 1 validation so that
	// keys created via Admin API are valid immediately after gateway restart.
	if stores.sqlDB != nil {
		rows, err := stores.sqlDB.QueryContext(ctx, "SELECT api_key FROM api_key_users")
		if err != nil {
			log.Warn("gateway: preload DB API keys failed", "err", err)
		} else {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var key string
				if rows.Scan(&key) == nil {
					auth.AddKey(key)
				}
			}
			if err := rows.Err(); err != nil {
				log.Warn("gateway: preload DB API keys incomplete", "err", err)
			}
		}
	}

	retryCtrl := gateway.NewLLMRetryController(cfg.Worker.AutoRetry, log)

	// Durable ingress: terminal-state repair retry. Capacity scales with the
	// session pool so busier deployments buffer more in-flight repairs. Handlers
	// and bridge enqueue best-effort; the repairer is nil-safe to call.
	repairer := execution.NewRepairer(stores.execution, execution.DefaultRepairConfig(cfg.Pool.MaxSize), log)

	agentConfigDir := ""
	if cfg.AgentConfig.Enabled {
		agentConfigDir = cfg.AgentConfig.ConfigDir
		log.Debug("config: agent config resolved", "dir", agentConfigDir)
	}

	bridge = gateway.NewBridge(gateway.BridgeDeps{
		Log:                    log,
		Hub:                    hub,
		SM:                     sm,
		EventCollector:         stores.collector,
		TurnsQuerier:           stores.event, // SQLiteStore implements TurnQuerier
		RetryCtrl:              retryCtrl,
		AgentConfigDir:         agentConfigDir,
		TurnTimeout:            cfg.Worker.TurnTimeout,
		WorkerEnv:              buildWorkerEnv(cfg),
		WorkerEnvBlocklist:     cfg.Worker.EnvBlocklist,
		CronEnv:                buildCronEnv(cfg),
		MCPConfigJSON:          buildMCPConfigJSON(cfg),
		AgentConfigExclude:     buildAgentConfigExclude(cfg),
		DefaultPermissionMode:  cfg.Worker.DefaultPermissionMode,
		WSStore:                stores.wsStore,
		PermissionDedupEnabled: cfg.Worker.PermissionDenyDedup.Enabled,
		PermissionDedupWindow:  cfg.Worker.PermissionDenyDedup.Window,
		ExecutionStore:         stores.execution,
		Repairer:               repairer,
	})
	repairer.SetSuccessHook(bridge.HandleRepairSuccess)

	// One-time validation sweep: surface stale/invalid agent_config_overrides
	// written before spec ② write-time validation (#749). Non-blocking.
	gateway.ScanWorkspaceOverrides(ctx, stores.wsStore, log)

	skillsLocator := skills.NewLocator(log, cfg.Skills.CacheTTL)
	builtinRegistry, err := builtin.NewRegistry()
	if err != nil {
		skillsLocator.Close()
		return fmt.Errorf("initialize built-in skills catalog: %w", err)
	}
	builtinSkillsCatalog := builtin.NewPublicCatalog(builtinRegistry)
	if userHome, homeErr := os.UserHomeDir(); homeErr != nil {
		log.Warn("gateway: built-in skills status skipped", "reason", "user_home_unavailable")
	} else if statusErr := runBuiltinSkillsStatus(ctx, cfg, userHome, config.HotplexHome(), newSkillsRunner, log); statusErr != nil {
		log.Warn("gateway: built-in skills status unavailable", "reason", stableSkillsStatusReason(statusErr))
	}

	handler = gateway.NewHandler(gateway.HandlerDeps{
		Log:             log,
		Hub:             hub,
		SM:              sm,
		Auth:            auth,
		Bridge:          bridge,
		SkillsLocator:   skillsLocator,
		ExecutionStore:  stores.execution,
		Repairer:        repairer,
		OwnerInstanceID: ownerInstanceID,
	})
	handler.SetAuditCollector(auditCollector)
	hub.SetAuditCollector(auditCollector)
	bridge.SetAuditCollector(auditCollector)                // tool.call audit (issue #833 P2)
	bridge.SetPendingReplayer(handler)                      // SESSION_BUSY mid-turn replay (done-time fallback)
	bridge.SetCatalogInvalidator(handler.InvalidateCatalog) // worker attach → session command catalog refresh (spec §5.2)

	if cfg.Worker.AutoRetry.Enabled {
		log.Info("gateway: LLM auto-retry enabled", "max_retries", cfg.Worker.AutoRetry.MaxRetries, "base_delay", cfg.Worker.AutoRetry.BaseDelay)
	}

	opencodeserver.InitSingleton(log, cfg.Worker.OpenCodeServer)
	claudecode.InitConfig(cfg.Worker.ClaudeCode)
	acp.InitConfig(cfg.Worker.ACP)
	codexcli.InitSingleton(log, cfg.Worker.CodexCLI)
	if cfg.Worker.CodexCLI.Sandbox == "danger-full-access" {
		log.Warn("codexcli: sandbox is danger-full-access (YOLO mode) — full filesystem + network access",
			"hint", "set codex_cli.sandbox to workspace-write or read-only for restricted environments")
	}

	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.Worker.AutoRetry, next.Worker.AutoRetry) {
			retryCtrl.UpdateConfig(next.Worker.AutoRetry)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.Security.APIKeys, next.Security.APIKeys) {
			auth.ReloadKeys(&next.Security)
		}
	})

	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.ResolvedAPIKeyUsers, next.ResolvedAPIKeyUsers) {
			dbR := stores.dbResolver
			if len(next.ResolvedAPIKeyUsers) > 0 {
				auth.SetKeyResolver(security.NewChainResolver(security.NewMapResolver(next.ResolvedAPIKeyUsers), dbR))
			} else {
				auth.SetKeyResolver(dbR)
			}
			log.Info("config: API key resolver updated",
				"mapped_config_keys", len(next.ResolvedAPIKeyUsers))
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.Worker.ClaudeCode, next.Worker.ClaudeCode) {
			claudecode.InitConfig(next.Worker.ClaudeCode)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Worker.CodexCLI.Command != next.Worker.CodexCLI.Command {
			codexcli.InitConfig(next.Worker.CodexCLI)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.Worker.ACP, next.Worker.ACP) {
			acp.InitConfig(next.Worker.ACP)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if !reflect.DeepEqual(prev.Worker.ClaudeCode.MCPServers, next.Worker.ClaudeCode.MCPServers) {
			bridge.UpdateMCPConfig(buildMCPConfigJSON(next))
			log.Info("config: MCP servers updated", "count", len(next.Worker.ClaudeCode.MCPServers))
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		prevExcl := buildAgentConfigExclude(prev)
		nextExcl := buildAgentConfigExclude(next)
		if !reflect.DeepEqual(prevExcl, nextExcl) {
			bridge.UpdateAgentConfigExclude(nextExcl)
			log.Info("config: agent config inject_exclude updated")
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Worker.DefaultPermissionMode != next.Worker.DefaultPermissionMode {
			bridge.UpdateDefaultPermissionMode(next.Worker.DefaultPermissionMode)
			log.Info("config: worker default_permission_mode updated", "value", next.Worker.DefaultPermissionMode)
		}
	})
	cfgStore.RegisterFunc(func(prev, next *config.Config) {
		if prev.Audit.Retention != next.Audit.Retention {
			if auditGC != nil {
				auditGC.UpdateRetention(next.Audit.Retention)
				log.Info("audit: retention updated (next GC tick applies)",
					"old", prev.Audit.Retention, "new", next.Audit.Retention)
			} else {
				log.Warn("audit: retention changed but GC is not running (audit disabled?)",
					"old", prev.Audit.Retention, "new", next.Audit.Retention)
			}
		}
		if prev.Audit.Collector.BatchInterval != next.Audit.Collector.BatchInterval {
			log.Warn("audit: batch_interval changed, requires restart to take effect")
		}
	})

	// Assemble deps and start HTTP + messaging

	// Cron scheduler: init after Bridge, before messaging adapters.
	if cfg.Cron.Enabled {
		var cronStore cron.Store
		if stores.cron != nil {
			cronStore = stores.cron
		} else {
			cronStore = cron.NewSQLiteStore(stores.sqlDB, log, stores.writeMu)
		}
		// Occurrences share the job store's database and write mutex so a
		// firing and its durable identity commit against the same ledger.
		var occurrenceStore cron.OccurrenceStore
		if stores.occurrences != nil {
			occurrenceStore = stores.occurrences
		} else {
			occurrenceStore = cron.NewSQLiteOccurrenceStore(stores.sqlDB, log, stores.writeMu)
		}
		cronEffectDelivery = newCronEffectDelivery(
			log, stores, ownerInstanceID, cronEffectOwners, occurrenceStore, cronStore)
		// Unfinished deliveries converge without an operator: the loop fences
		// lapsed leases into unknown first, then finishes what is provably
		// owed. Its first tick is deliberately late so messaging adapters are
		// up before anything can be sent.
		if deliverer, ok := cronEffectDelivery.(*gateway.EffectDeliverer); ok {
			deliverer.StartRecoveryLoop(ctx, 10*time.Second, 50)
		}
		cronDelivery = cron.NewDelivery(log,
			func(ctx context.Context, sessionID string) (string, error) {
				if err := stores.collector.Flush(); err != nil {
					log.Warn("cron: flush before query", "err", err)
				}
				turns, err := stores.event.QueryTurns(ctx, sessionID, 1, 0)
				if err != nil || len(turns) == 0 {
					return "", err
				}
				return turns[len(turns)-1].Content, nil
			},
			nil,
		)
		cronAttRouter = &cronAttachedRouter{bridge: bridge, sm: sm}
		cronScheduler = cron.New(cron.Deps{
			Log:         log,
			Store:       cronStore,
			Occurrences: occurrenceStore,
			// Cron prompts go through the gateway's durable input path
			// (execution ledger, owner lease, execution-correlated completion)
			// rather than calling Worker.Input directly.
			Dispatcher:     handler,
			Bridge:         bridge,
			SessionMgr:     sm,
			Delivery:       cronDelivery,
			EffectDelivery: cronEffectDelivery,
			AttachedRouter: cronAttRouter,
			YAMLDefs:       cronConfigToYAMLDefs(cfg.Cron.Jobs),
			Cfg: cron.Config{
				Enabled:           cfg.Cron.Enabled,
				MaxConcurrentRuns: cfg.Cron.MaxConcurrentRuns,
				MaxJobs:           cfg.Cron.MaxJobs,
				DefaultTimeoutSec: cfg.Cron.DefaultTimeoutSec,
				TickIntervalSec:   cfg.Cron.TickIntervalSec,
				YAMLConfigPath:    cfg.Cron.YAMLConfigPath,
				DefaultSandbox:    cfg.Worker.CodexCLI.Sandbox,
			},
			ResolveWorkDir: func(job *cron.CronJob) string {
				return cfgStore.Load().ResolvePlatformWorkDir(job.Platform)
			},
		})
		if err := cronScheduler.Start(ctx); err != nil {
			log.Warn("cron: scheduler start failed (cron disabled)", "err", err)
			cronScheduler = nil
		} else {
			// Hot-reload cron config at runtime.
			cfgStore.RegisterFunc(func(prev, next *config.Config) {
				if prev.Cron.MaxConcurrentRuns != next.Cron.MaxConcurrentRuns ||
					prev.Cron.MaxJobs != next.Cron.MaxJobs {
					cronScheduler.UpdateConfig(next.Cron.MaxConcurrentRuns, next.Cron.MaxJobs)
				}
			})
		}
	}

	// Cookie auth: created when webchat is enabled or when WebChat address is configured
	// (supporting external dev/production frontends), or when running in devMode.
	var cookieAuth *security.CookieAuth
	if cfg.WebChat.Enabled || cfg.WebChat.Addr != "" || devMode {
		ca, err := security.NewCookieAuth(cfg.Security.CookieSecret)
		if err != nil {
			return fmt.Errorf("create cookie auth: %w", err)
		}
		ca.SetSameSite(cfg.Security.CookieSameSite)
		cookieAuth = ca
		auth.SetCookieAuth(ca)
		log.Info("gateway: webchat cookie auth enabled")
		if strings.EqualFold(cfg.Security.CookieSameSite, "lax") || strings.EqualFold(cfg.Security.CookieSameSite, "strict") || cfg.Security.CookieSameSite == "" {
			log.Warn("gateway: CookieSameSite is set to Lax/Strict (or default); cross-origin requests from other domains will not carry authentication cookies. For cross-origin HTTPS deployments, configure to 'none'")
		}
	}

	// OAuth manager: created when SSO providers are configured (spec ④).
	// Requires cookieAuth for signing state cookies.
	var oauthManager *security.OAuthManager
	if cookieAuth != nil {
		oauthManager = security.NewOAuthManager(cookieAuth)
		if err := cfg.OAuth.Validate(); err != nil {
			log.Warn("oauth config validation failed", "err", err)
		} else if len(cfg.OAuth.Providers) > 0 {
			count, err := oauthManager.Reload(ctx, cfg.OAuth)
			if err != nil {
				log.Error("oauth manager init failed", "err", err)
			}
			log.Info("gateway: oauth SSO providers loaded", "count", count)
		}
		cfgStore.RegisterFunc(func(prev, next *config.Config) {
			if reflect.DeepEqual(prev.OAuth, next.OAuth) {
				return
			}
			if err := next.OAuth.Validate(); err != nil {
				log.Warn("oauth config reload skipped: validation failed", "err", err)
				return
			}
			count, err := oauthManager.Reload(ctx, next.OAuth)
			if err != nil {
				log.Error("oauth config reload completed with provider errors", "count", count, "err", err)
				return
			}
			log.Info("oauth config reloaded", "count", count)
		})
	}

	mux := http.NewServeMux()
	leaseMgr := execution.NewLeaseManager(stores.execution, ownerInstanceID, execution.DefaultLeaseConfig(), log, repairer)
	deps := &GatewayDeps{
		Log:                  log,
		Ctx:                  ctx,
		Config:               cfg,
		ConfigStore:          cfgStore,
		Hub:                  hub,
		SessionMgr:           sm,
		EventStore:           stores.event,
		EventCollector:       stores.collector,
		ExecutionStore:       stores.execution,
		Auth:                 auth,
		Handler:              handler,
		Bridge:               bridge,
		ConfigWatcher:        configWatcher,
		CronScheduler:        cronScheduler,
		CookieAuth:           cookieAuth,
		OAuthManager:         oauthManager,
		ChatAccessStore:      stores.chatAccessOrNew(stores.sqlDB, log),
		DB:                   stores.sqlDB,
		DBResolver:           dbResolver,
		APIKeyStore:          stores.apiKeyStore,
		WorkspaceStore:       stores.wsStore,
		WriteMu:              stores.writeMu,
		ConfigPath:           configPath,
		DevMode:              devMode,
		AuditCollector:       auditCollector,
		AuditStore:           auditStore,
		SkillsLocator:        skillsLocator,
		BuiltinSkillsCatalog: builtinSkillsCatalog,
		OwnerInstanceID:      ownerInstanceID,
		LeaseManager:         leaseMgr,
		Repairer:             repairer,
	}
	deps.RestartCoordinator = newGatewayRestartCoordinator(log, cfgStore, configPath, devMode)

	// Brain: lightweight LLM layer for TTS summarization (fail-open).
	if err := brain.Init(log); err != nil {
		log.Warn("Brain initialization failed (fail-open)", "err", err)
	}

	// Events and turns retain only their configured events.retention window.
	// Audit plaintext has an independent retention policy. The GC captures this
	// value at startup, so changing events.retention requires a gateway restart.
	eventsRetention := config.EffectiveEventsRetention(cfg)
	go runEventsGC(ctx, stores, log, eventsRetention)

	leaseMgr.Start(ctx)
	log.Info("gateway: lease manager started", "owner_instance_id", ownerInstanceID)

	repairer.Start(ctx)
	log.Info("gateway: repairer started")

	msgAdapters, adapterStatuses := startMessagingAdapters(ctx, deps)
	lifecycleBroadcaster := newLifecycleBroadcaster(deps)
	cronEffectOwners.set(msgAdapters...)

	// Wire cron delivery to platform adapters.
	if cronDelivery != nil {
		cronDelivery.SetDeliverer(func(ctx context.Context, platform string, platformKey map[string]string, response string) error {
			for _, a := range msgAdapters {
				if a.Platform() == messaging.PlatformType(platform) {
					if sender, ok := a.(messaging.CronResultSender); ok {
						return sender.SendCronResult(ctx, response, platformKey)
					}
				}
			}
			return fmt.Errorf("cron delivery: no adapter for platform %q", platform)
		})
	}

	adminHandler := setupRoutes(mux, deps)

	// Webchat SPA fallback
	var rootHandler http.Handler = mux
	if cfg.WebChat.Enabled {
		// Warn at boot if the resolved webchat CSP is the package default
		// (no operator override) or is itself permissive (any-host sources).
		// ResolveCSP normalises whitespace-only overrides to the default, so
		// a stray space cannot ship a malformed header silently.
		if resolved := security.ResolveCSP(security.DefaultWebChatCSP, cfg.Security.CSP); security.IsPermissiveCSP(resolved) {
			log.Warn("csp: webchat policy is permissive (any http/https/ws/wss host allowed); set security.csp to restrict in production",
				"service", "webchat")
		}
		spa := webchat.Handler(cfg.Security.CSP, cookieAuth)
		rootHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pattern := mux.Handler(r)
			if pattern != "" {
				mux.ServeHTTP(w, r)
				return
			}
			spa.ServeHTTP(w, r)
		})
	}

	server := &http.Server{
		Addr:         cfg.Gateway.Addr,
		Handler:      rootHandler,
		ReadTimeout:  cfg.Gateway.IdleTimeout, //nolint:staticcheck // Kept for backward compatibility
		WriteTimeout: cfg.Gateway.WriteTimeout,
	}

	if configWatcher != nil {
		if err := configWatcher.Start(ctx); err != nil {
			log.Warn("config: watcher start failed", "err", err)
		}
	}

	serverErr := make(chan error, 2)
	// Admin server: dedicated port for network isolation (always-on when enabled).
	var adminServer *http.Server
	var adminAddr string
	if err := startGatewayHTTPServer(server, serverErr, log, "gateway: server failed", net.Listen); err != nil {
		log.Error("gateway: server failed to start", "err", err)
		cancel()
		shutdownGateway(ctx, log, deps, msgAdapters, server, adminServer, skillsLocator, pidTracker, cleanupWG, cronScheduler)
		return err
	}
	if cfg.Admin.Enabled {
		adminServer = &http.Server{
			Addr:         cfg.Admin.Addr,
			Handler:      adminHandler,
			ReadTimeout:  cfg.Gateway.IdleTimeout, //nolint:staticcheck // Kept for backward compatibility
			WriteTimeout: cfg.Gateway.WriteTimeout,
		}
		adminAddr = adminServer.Addr
		log.Info("admin: starting", "addr", adminAddr)
		if err := startGatewayHTTPServer(adminServer, serverErr, log, "admin: server failed", net.Listen); err != nil {
			log.Error("admin: server failed to start", "err", err)
			cancel()
			shutdownGateway(ctx, log, deps, msgAdapters, server, adminServer, skillsLocator, pidTracker, cleanupWG, cronScheduler)
			return err
		}
	}
	printStartupBanner(os.Stdout, newBuildInfo(), RuntimeStatus{
		GatewayAddr:     cfg.Gateway.Addr,
		AdminAddr:       adminAddr,
		WebChatAddr:     cfg.WebChat.Addr,
		WebChatEmbedded: cfg.WebChat.Enabled,
		TLSEnabled:      cfg.Security.TLSEnabled,
		DBDriver:        cfg.DB.Driver,
		DBPath:          cfg.DB.Path,
		PoolMax:         cfg.Pool.MaxSize,
		PoolIdle:        cfg.Pool.MaxIdlePerUser,
		Adapters:        adapterStatuses,
		RetryEnabled:    cfg.Worker.AutoRetry.Enabled,
		RetryMax:        cfg.Worker.AutoRetry.MaxRetries,
		RetryDelay:      cfg.Worker.AutoRetry.BaseDelay.String(),
	}, configPath)

	// Wait for shutdown signal or SIGHUP reload
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	if runtime.GOOS != "windows" {
		signal.Notify(sig, syscall.SIGHUP)
	}
	defer signal.Stop(sig)
	lifecycleBroadcaster.BroadcastStarted()
	if deps.RestartCoordinator != nil {
		if err := deps.RestartCoordinator.CompleteReady(); err != nil {
			log.Warn("gateway restart: ready lease completion failed", "error_kind", "lease_complete_failed")
		}
	}

loop:
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGHUP {
				if cronScheduler != nil {
					cronScheduler.ReloadIndex()
				}
				log.Info("gateway: cron index reloaded (SIGHUP)")
				continue
			}
			log.Info("gateway: shutdown", "signal", s)
			break loop
		case err := <-serverErr:
			if err != nil {
				log.Error("gateway: server failed, exiting", "err", err)
				cancel()
				shutdownGateway(ctx, log, deps, msgAdapters, server, adminServer, skillsLocator, pidTracker, cleanupWG, cronScheduler)
				return err
			}
			cancel()
			shutdownGateway(ctx, log, deps, msgAdapters, server, adminServer, skillsLocator, pidTracker, cleanupWG, cronScheduler)
			return nil
		case <-stopCh:
			log.Info("gateway: shutdown", "signal", "stopCh")
			break loop
		}
	}

	runGatewayControlledShutdown(lifecycleBroadcaster, cancel, func() {
		shutdownGateway(ctx, log, deps, msgAdapters, server, adminServer, skillsLocator, pidTracker, cleanupWG, cronScheduler)
	})
	return nil
}

// --- Decomposed helpers ---

type gatewayListenFunc func(network, address string) (net.Listener, error)

func startGatewayHTTPServer(
	server *http.Server,
	serverErr chan<- error,
	log *slog.Logger,
	errorMessage string,
	listen gatewayListenFunc,
) error {
	listener, err := listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", server.Addr, err)
	}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Error(errorMessage, "err", serveErr)
			serverErr <- serveErr
		}
	}()
	return nil
}

func initLogging(cfg *config.Config) (*slog.Logger, *config.ConfigStore, *slog.LevelVar) {
	cfgStore := config.NewConfigStore(cfg, slog.Default())

	levelVar := &slog.LevelVar{}
	if err := levelVar.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		levelVar.Set(slog.LevelInfo)
	}

	opts := &slog.HandlerOptions{
		Level:     levelVar,
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().Format("2006-01-02T15:04:05.0000"))
			}
			// Compact source attribution to "file.go:42 internal/pkg.(Type).Method":
			// the module prefix is constant per binary (pure noise), while the
			// package-qualified function name survives line drift across versions.
			if len(groups) == 0 && a.Key == slog.SourceKey {
				if s, ok := a.Value.Any().(*slog.Source); ok && s != nil {
					fn := strings.TrimPrefix(s.Function, "github.com/hrygo/hotplex/")
					return slog.String(slog.SourceKey, fmt.Sprintf("%s:%d %s", filepath.Base(s.File), s.Line, fn))
				}
			}
			return a
		},
	}

	var logHandler slog.Handler
	writer := buildLogWriter(cfg)
	if cfg.Log.Format == "text" {
		logHandler = slog.NewTextHandler(writer, opts)
	} else {
		logHandler = slog.NewJSONHandler(writer, opts)
	}

	log := slog.New(logHandler).With(
		"service", "hotplex-gateway",
		"version", versionString(),
	)
	slog.SetDefault(log)

	return log, cfgStore, levelVar
}

// stderrIsTTY reports whether stderr is connected to a terminal.
// Used to decide whether to tee logs to stderr when file logging is enabled:
// in daemon mode stderr is redirected to a file (not a TTY), so tee-ing would
// duplicate every line into the same file lumberjack manages.
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// buildLogWriter constructs the slog writer based on LogConfig.
//
// When log.file is disabled (default), it returns os.Stderr unchanged,
// preserving historical behavior in both foreground and daemon modes.
//
// When log.file is enabled, logs are written to the file (rotated via
// lumberjack). In a foreground TTY, logs are also teed to stderr for live
// debugging; in daemon/service mode (stderr already redirected to a file),
// stderr is suppressed to avoid duplicating lines into the same file.
func buildLogWriter(cfg *config.Config) io.Writer {
	fc := cfg.Log.File
	if !fc.Enabled {
		return os.Stderr
	}

	path := fc.Path
	if path == "" {
		// Default path mirrors daemon mode: ~/.hotplex/logs/gateway.log.
		logDir := filepath.Join(config.HotplexHome(), "logs")
		path = filepath.Join(logDir, "gateway.log")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		// Cannot create log dir — fall back to stderr rather than aborting startup.
		fmt.Fprintf(os.Stderr, "log: failed to create log dir %q: %v; falling back to stderr\n", filepath.Dir(path), err)
		return os.Stderr
	}

	lw := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    fc.MaxSize,
		MaxAge:     fc.MaxAge,
		MaxBackups: fc.MaxBackups,
		Compress:   fc.Compress,
		LocalTime:  fc.LocalTime,
	}

	// Tee to stderr only in foreground (TTY) mode. In daemon/service mode
	// stderr is already redirected to a log file; writing there too would
	// duplicate every line when path coincides with the daemon's redirect target.
	if stderrIsTTY() {
		return io.MultiWriter(os.Stderr, lw)
	}
	return lw
}

// cleanupStaleTempFiles removes orphaned temp files from previous gateway runs.
// Files younger than 2 hours are preserved (may be in active use by a worker
// from a previous process that hasn't terminated yet).
func cleanupStaleTempFiles(log *slog.Logger) {
	baseDir := config.TempBaseDir()
	workerDir := filepath.Join(baseDir, "worker")
	mediaDir := filepath.Join(baseDir, "media")

	// Clean worker temp directory: remove files older than 2h.
	cleaned := 0
	if entries, err := os.ReadDir(workerDir); err == nil {
		cutoff := time.Now().Add(-2 * time.Hour)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				p := filepath.Join(workerDir, e.Name())
				if err := os.Remove(p); err == nil {
					cleaned++
				}
			}
		}
	}

	// Clean media directory: recursive walk to remove files older than 24h.
	// Adapter-owned subdirectories (e.g., "slack/") are also cleaned here at
	// startup; the adapters' periodic goroutines handle ongoing maintenance.
	cutoff := time.Now().Add(-24 * time.Hour)
	_ = filepath.Walk(mediaDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.ModTime().Before(cutoff) {
			if removeErr := os.Remove(path); removeErr == nil {
				cleaned++
			}
		}
		return nil
	})

	// Also scan legacy temp files scattered in os.TempDir() root (pre-unification).
	cleaned += cleanupLegacyTempFiles()

	if cleaned > 0 {
		log.Info("gateway: cleaned stale temp files", "count", cleaned)
	}
}

// cleanupLegacyTempFiles scans for hotplex temp files from before the unified
// worker/ subdirectory was introduced. Searches both /tmp (hardcoded path used
// by the old TempBaseDir) and os.TempDir() (in case TMPDIR was overridden).
func cleanupLegacyTempFiles() int {
	// Pre-unification files were created by os.CreateTemp("", ...) which uses
	// os.TempDir(). On stock Linux this is /tmp, but TMPDIR overrides change it.
	// Search both /tmp and os.TempDir() to cover all cases.
	searchDirs := []string{"/tmp"}
	if td := os.TempDir(); td != "/tmp" {
		searchDirs = append(searchDirs, td)
	}
	patterns := []string{
		"hotplex-append-system-prompt-*",
		"hotplex-mcp-config-*",
		"hotplex-system-prompt-*",
		"hotplex-update-*",
	}
	cutoff := time.Now().Add(-2 * time.Hour)
	cleaned := 0
	for _, dir := range searchDirs {
		for _, pattern := range patterns {
			matches, err := filepath.Glob(filepath.Join(dir, pattern))
			if err != nil {
				continue
			}
			for _, p := range matches {
				info, err := os.Stat(p)
				if err != nil {
					continue
				}
				if info.ModTime().Before(cutoff) {
					if err := os.Remove(p); err == nil {
						cleaned++
					}
				}
			}
		}
	}
	return cleaned
}

func initOrphanCleanup(ctx context.Context, cfg *config.Config, log *slog.Logger) (*proc.Tracker, *sync.WaitGroup) {
	pidTracker := proc.InitTracker(cfg.Worker.PIDDir, log)
	var cleanupWG sync.WaitGroup
	if err := pidTracker.EnsureDir(); err != nil {
		log.Warn("gateway: pid dir setup failed, orphan cleanup disabled", "dir", cfg.Worker.PIDDir, "err", err)
	} else {
		cleanupWG.Add(1)
		go func() {
			defer cleanupWG.Done()
			cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cleanupCancel()
			results := pidTracker.CleanupOrphans(cleanupCtx, 3, 5*time.Second)
			killed := 0
			for _, r := range results {
				if r.Err != nil {
					log.Warn("gateway: orphan cleanup error", "key", r.Key, "pgid", r.PGID, "err", r.Err)
				} else if r.Killed {
					log.Info("gateway: killed orphan process", "key", r.Key, "pgid", r.PGID)
					killed++
				}
			}
			if len(results) > 0 {
				log.Info("gateway: orphan cleanup complete", "scanned", len(results), "killed", killed)
			}
		}()
	}
	return pidTracker, &cleanupWG
}

type gatewayStores struct {
	session     session.Store
	execution   execution.Store
	event       eventStoreProvider
	turnQuerier eventstore.TurnQuerier
	collector   *eventstore.Collector
	cron        cron.Store
	occurrences cron.OccurrenceStore
	chatAccess  messaging.ChatAccessStorer
	writeMu     *sqlutil.WriteMu // nil when using PostgreSQL (WriteMu is SQLite-only)
	db          *dbutil.DB
	sqlDB       *sql.DB
	dialect     dbutil.Dialect
	apiKeyStore admin.APIKeyUserStorer
	wsStore     session.UserWorkspaceStore
	dbResolver  *security.DBResolver
}

// chatAccessOrNew returns the chat-access store if already initialized (PG path),
// or creates a new SQLite-backed store from the shared connection.
func (s *gatewayStores) chatAccessOrNew(db *sql.DB, log *slog.Logger) messaging.ChatAccessStorer {
	if s.chatAccess != nil {
		return s.chatAccess
	}
	return messaging.NewChatAccessStore(db, log, s.writeMu)
}

func initStores(ctx context.Context, cfg *config.Config, log *slog.Logger) (*gatewayStores, error) {
	switch dbutil.ParseDialect(cfg.DB.Driver) {
	case dbutil.DialectPostgres:
		return initPGStores(ctx, cfg, log)
	default:
		return initSQLiteStores(ctx, cfg, log)
	}
}

// initSQLiteStores initializes all stores using SQLite (existing logic).
func initSQLiteStores(ctx context.Context, cfg *config.Config, log *slog.Logger) (*gatewayStores, error) {
	writeMu := sqlutil.NewWriteMu(sqlutil.DialectSQLite)
	sessionStore, err := session.NewSQLiteStore(ctx, cfg, writeMu)
	if err != nil {
		return nil, err
	}

	// EventStore shares the session store's *sql.DB (schema managed by goose migration 002).
	eventStore := eventstore.NewSQLiteStore(sessionStore.DB(), writeMu)
	executionStore, err := execution.NewSQLStore(ctx, sessionStore.DB(), dbutil.DialectSQLite, writeMu, log)
	if err != nil {
		_ = sessionStore.Close()
		return nil, fmt.Errorf("execution store init: %w", err)
	}
	dbResolver := security.NewDBResolver(sessionStore.DB(), dbutil.DialectSQLite)

	return &gatewayStores{
		session:     sessionStore,
		execution:   executionStore,
		wsStore:     sessionStore,
		event:       eventStore,
		turnQuerier: eventStore,
		collector:   eventstore.NewCollector(eventStore, log),
		writeMu:     writeMu,
		sqlDB:       sessionStore.DB(),
		dialect:     dbutil.DialectSQLite,
		dbResolver:  dbResolver,
	}, nil
}

// initPGStores initializes all stores using PostgreSQL (new driver path).
func initPGStores(ctx context.Context, cfg *config.Config, log *slog.Logger) (*gatewayStores, error) {
	db, err := dbutil.Open(dbutil.DialectPostgres, &cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("pg: open db: %w", err)
	}

	sessionStore, err := session.NewPGStore(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg: session store: %w", err)
	}

	eventStore := eventstore.NewPGStore(db, log)
	executionStore, err := execution.NewSQLStore(ctx, db.DB, dbutil.DialectPostgres, nil, log)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg: execution store: %w", err)
	}
	cronStore := cron.NewPGStore(db, log)
	chatAccessStore := messaging.NewChatAccessPGStore(db, log)
	dbResolver := security.NewDBResolver(db.DB, dbutil.DialectPostgres)

	// NewPGStore returns the narrow Store interface; assert the concrete *pgStore
	// also satisfies UserWorkspaceStore (workspace CRUD, spec ①).
	wsStore, ok := sessionStore.(session.UserWorkspaceStore)
	if !ok {
		_ = db.Close()
		return nil, fmt.Errorf("pg: session store does not implement UserWorkspaceStore")
	}

	return &gatewayStores{
		session:     sessionStore,
		execution:   executionStore,
		wsStore:     wsStore,
		event:       eventStore,
		turnQuerier: eventStore,
		collector:   eventstore.NewCollector(eventStore, log),
		cron:        cronStore,
		occurrences: cron.NewPGOccurrenceStore(db, log),
		chatAccess:  chatAccessStore,
		db:          db,
		sqlDB:       db.DB,
		dialect:     dbutil.DialectPostgres,
		apiKeyStore: admin.NewAPIKeyUserPGStore(db, dbResolver),
		dbResolver:  dbResolver,
	}, nil
}

func (s *gatewayStores) close(log *slog.Logger) {
	if s.collector != nil {
		if err := s.collector.Close(); err != nil {
			log.Warn("gateway: event collector close", "err", err)
		}
	}
	// Stop DBResolver's background cleanup goroutine before closing DB connections.
	if s.dbResolver != nil {
		s.dbResolver.Close()
	}
	// For SQLite: EventStore.Close is a no-op (ownsDB=false); session store owns the shared connection.
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			log.Warn("gateway: session store close", "err", err)
		}
	}
	// For PG: close the shared dbutil.DB connection.
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			log.Warn("gateway: db close", "err", err)
		}
	}
}

// runEventsGC periodically deletes expired events and turns.
func runEventsGC(ctx context.Context, stores *gatewayStores, log *slog.Logger, retention time.Duration) {
	if retention <= 0 {
		retention = 720 * time.Hour // default 30 days
	}
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-retention)
			if n, err := stores.event.DeleteExpired(ctx, cutoff); err == nil && n > 0 {
				log.Info("events gc: deleted expired events", "count", n)
			}
			if n, err := stores.turnQuerier.DeleteExpiredTurns(ctx, cutoff); err == nil && n > 0 {
				log.Info("events gc: deleted expired turns", "count", n)
			}
		}
	}
}

func shutdownGateway(
	_ context.Context,
	log *slog.Logger,
	deps *GatewayDeps,
	msgAdapters []messaging.PlatformAdapterInterface,
	server *http.Server,
	adminServer *http.Server,
	skillsLocator *skills.Locator,
	pidTracker *proc.Tracker,
	cleanupWG *sync.WaitGroup,
	cronScheduler *cron.Scheduler,
) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() {
		if err := observability.Shutdown(shutdownCtx); err != nil {
			log.Warn("observability: shutdown", "err", err)
		}
		shutdownCancel()
	}()

	// Fence supplement replay before closing Hub/adapters. Repairer.Shutdown
	// drains its backlog and may otherwise invoke the runtime-success hook while
	// the gateway is already tearing down, which could start a fresh delivery.
	if deps.Repairer != nil {
		deps.Repairer.SetSuccessHook(nil)
	}
	deps.Bridge.StopPendingReplays()
	deps.Bridge.WaitPendingReplays(shutdownCtx)

	if err := deps.Hub.Shutdown(shutdownCtx); err != nil {
		log.Warn("gateway: hub shutdown", "err", err)
	}

	skillsLocator.Close()

	brain.Close()

	if deps.ConfigWatcher != nil {
		if err := deps.ConfigWatcher.Close(); err != nil {
			log.Warn("config: watcher close", "err", err)
		}
	}

	if deps.WebhookHandler != nil {
		deps.WebhookHandler.Close()
	}

	if cronScheduler != nil {
		cronScheduler.Shutdown(shutdownCtx)
	}

	for _, adapter := range msgAdapters {
		if err := adapter.Close(shutdownCtx); err != nil {
			log.Warn("messaging: adapter close", "err", err)
		}
	}
	messaging.DefaultBotRegistry().UnregisterAll()

	closeSTTCache(shutdownCtx, log)
	closeTTSCache(shutdownCtx, log)

	// Durable ingress: shut down lease manager BEFORE Bridge.MarkClosed so
	// owned active executions are marked unknown before workers are killed.
	if deps.Repairer != nil {
		deps.Repairer.Shutdown(shutdownCtx)
	}
	if deps.LeaseManager != nil {
		if err := deps.LeaseManager.Shutdown(shutdownCtx); err != nil {
			log.Warn("gateway: lease manager shutdown", "err", err)
		}
	}

	// Mark bridge closed FIRST to prevent in-flight message handlers from
	// creating new sessions/workers during shutdown. Without this, a handler
	// can pass the b.closed check in StartSession, start a worker, and have
	// it immediately killed by TerminateAllWorkers below (race #613 defect 2).
	deps.Bridge.MarkClosed()

	// Terminate all workers so forwardEvents goroutines (blocked on worker
	// stdout) can exit.
	deps.SessionMgr.TerminateAllWorkers()
	opencodeserver.ShutdownSingleton(shutdownCtx)
	codexcli.ShutdownSingleton(shutdownCtx)

	// Wait for forwardEvents goroutines to drain.
	deps.Bridge.WaitForwarders(shutdownCtx)

	cleanupWG.Wait()
	pidTracker.RemoveAll()

	// Close eventstore collector BEFORE SessionMgr.Close() to prevent
	// the collector's background writer from writing to a closed DB.
	// Collector.Close() is idempotent (sync.Once), so the deferred
	// stores.close() calling Close() again is safe.
	if deps.EventCollector != nil {
		if err := deps.EventCollector.Close(); err != nil {
			log.Warn("gateway: event collector close", "err", err)
		}
	}

	// Close audit collector AFTER eventstore (so final events can still be
	// processed) but BEFORE SessionMgr (so audit rows persist before session
	// metadata closes). Per AGENTS.md shutdown order.
	if deps.AuditCollector != nil {
		if err := deps.AuditCollector.Close(shutdownCtx); err != nil {
			log.Warn("audit: collector close", "err", err)
		}
	}

	if err := deps.SessionMgr.Close(); err != nil {
		log.Warn("gateway: session manager close", "err", err)
	}

	// Shut down HTTP servers in parallel to share the 30s budget.
	var serverWG sync.WaitGroup
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Warn("gateway: http server shutdown", "err", err)
		}
	}()
	if adminServer != nil {
		serverWG.Add(1)
		go func() {
			defer serverWG.Done()
			if err := adminServer.Shutdown(shutdownCtx); err != nil {
				log.Warn("admin: http server shutdown", "err", err)
			}
		}()
	}
	serverWG.Wait()

	log.Info("gateway: stopped")
}

// runBuiltinSkillsStatus performs the gateway startup reconciliation check. It
// is intentionally status-only: no inventory publication, projection, or
// receipt write is reachable from this seam.
func runBuiltinSkillsStatus(
	ctx context.Context,
	cfg *config.Config,
	userHome, hotplexHome string,
	newRunner func(userHome, hotplexHome string) (reconcile.Runner, error),
	log *slog.Logger,
) error {
	if cfg == nil {
		return reconcile.ErrNoWorkerTargets
	}
	workers, err := parseReconcileWorkerTypes(cfg.EnabledWorkerTypes())
	if err != nil {
		return err
	}
	if len(workers) == 0 {
		return nil
	}
	if newRunner == nil {
		return fmt.Errorf("skills: runner unavailable")
	}
	runner, err := newRunner(userHome, hotplexHome)
	if err != nil {
		return err
	}
	if runner == nil {
		return fmt.Errorf("skills: runner unavailable")
	}
	report, err := runner.Status(ctx, reconcile.Options{Profile: builtin.ProfileRuntime, WorkerTypes: workers})
	if err != nil {
		return err
	}
	if report.Err() != nil {
		if log != nil {
			log.Warn("gateway: built-in skills drift", "reasons", builtinSkillsDiagnosticReasons(report))
		}
		return nil
	}
	return nil
}

func stableSkillsStatusReason(err error) string {
	switch {
	case errors.Is(err, reconcile.ErrNoWorkerTargets):
		return "no_worker_targets"
	case errors.Is(err, reconcile.ErrUnknownWorker):
		return "unknown_worker"
	case errors.Is(err, reconcile.ErrUnknownProfile):
		return "unknown_profile"
	case errors.Is(err, reconcile.ErrRootOutsideHome):
		return "root_outside_home"
	case errors.Is(err, reconcile.ErrInventoryOutsideHotplexHome):
		return "inventory_outside_hotplex_home"
	case errors.Is(err, reconcile.ErrInvalidReceipt):
		return "invalid_receipt"
	case errors.Is(err, reconcile.ErrReceiptWriteFailed):
		return "receipt_write_failed"
	case errors.Is(err, reconcile.ErrInvalidPackageName):
		return "invalid_package"
	case errors.Is(err, reconcile.ErrReportActionRequired):
		return "action_required"
	default:
		return "status_unavailable"
	}
}

var builtinSkillsDiagnosticReasonSet = map[string]struct{}{
	reconcile.ReasonMissingTarget:      {},
	reconcile.ReasonDrift:              {},
	reconcile.ReasonCollision:          {},
	reconcile.ReasonInvalidReceipt:     {},
	reconcile.ReasonRootOutsideHome:    {},
	reconcile.ReasonReceiptWriteFailed: {},
	reconcile.ReasonInventoryBlocked:   {},
	reconcile.ReasonRollbackFailed:     {},
	reconcile.ReasonUnsupportedWorker:  {},
	reconcile.ReasonMissingReceipt:     {},
	reconcile.ReasonInvalidPackage:     {},
	reconcile.ReasonUnchanged:          {},
	reconcile.ReasonChanged:            {},
}

func builtinSkillsDiagnosticReasons(report reconcile.Report) string {
	reasons := make(map[string]struct{})
	for _, item := range report.Items {
		switch item.Outcome {
		case reconcile.OutcomeConflict, reconcile.OutcomeDrift, reconcile.OutcomeFailed:
			if _, ok := builtinSkillsDiagnosticReasonSet[item.ReasonCode]; ok {
				reasons[item.ReasonCode] = struct{}{}
			} else {
				reasons[reconcile.ReasonDrift] = struct{}{}
			}
		}
	}
	if len(reasons) == 0 {
		reasons[reconcile.ReasonDrift] = struct{}{}
	}
	values := make([]string, 0, len(reasons))
	for reason := range reasons {
		values = append(values, reason)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

// --- Config helpers ---

func loadConfig(configPath string, devMode bool) (*config.Config, error) {
	absPath, err := config.ExpandAndAbs(configPath)
	if err != nil {
		return nil, fmt.Errorf("config: resolve path %q: %w", configPath, err)
	}

	loadEnvFile(filepath.Dir(absPath))

	cfg, err := config.Load(absPath)
	if err != nil {
		return nil, fmt.Errorf("config: load %q: %w", absPath, err)
	}
	if devMode {
		cfg.Security.APIKeys = nil
		cfg.Admin.Tokens = nil
	}

	security.ConfigureFromConfig(&cfg.Security)

	return cfg, nil
}

func loadEnvFile(dir string) {
	envPath := filepath.Join(dir, ".env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		return
	}

	var loaded int
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		if os.Getenv(key) == "" && !security.IsProtected(key) {
			_ = os.Setenv(key, val)
			loaded++
		}
	}
	if loaded > 0 {
		fmt.Fprintf(os.Stderr, "  env loaded %d vars from %s\n", loaded, envPath)
	}
}

// cronConfigToYAMLDefs converts inline job definitions from config to YAMLJobDef slice.
func cronConfigToYAMLDefs(jobs []map[string]any) []cron.YAMLJobDef {
	if len(jobs) == 0 {
		return nil
	}
	data, _ := json.Marshal(jobs)
	var defs []cron.YAMLJobDef
	_ = json.Unmarshal(data, &defs)
	return defs
}

// buildWorkerEnv constructs the worker environment variables.
func buildWorkerEnv(cfg *config.Config) []string {
	return slices.Clone(cfg.Worker.Environment)
}

// buildCronEnv builds env vars injected only into cron platform sessions.
// Separated from buildWorkerEnv to prevent admin credentials from leaking
// to non-cron workers (env.go blocklist only filters os.Environ, not ConfigEnv).
func buildCronEnv(cfg *config.Config) []string {
	if !cfg.Cron.Enabled || !cfg.Admin.Enabled {
		return nil
	}
	var env []string
	env = append(env, "HOTPLEX_ADMIN_API_URL=http://"+cfg.Admin.Addr)
	if len(cfg.Admin.Tokens) > 0 {
		env = append(env, "HOTPLEX_ADMIN_TOKEN="+cfg.Admin.Tokens[0])
	}
	return env
}

// buildMCPConfigJSON serializes configured MCP servers into the JSON format
// expected by Claude Code's --mcp-config flag. Returns "" when no servers are
// configured, which signals the bridge to let Claude Code do default discovery.
func buildMCPConfigJSON(cfg *config.Config) string {
	if len(cfg.Worker.ClaudeCode.MCPServers) == 0 {
		return ""
	}
	// Validate each server config before serializing.
	valid := make(map[string]*config.MCPServerConfig, len(cfg.Worker.ClaudeCode.MCPServers))
	for name, srv := range cfg.Worker.ClaudeCode.MCPServers {
		if err := srv.Validate(); err != nil {
			slog.Error("config: invalid MCP server config, skipping", "server", name, "err", err)
			continue
		}
		valid[name] = srv
	}
	if len(valid) == 0 {
		return ""
	}
	wrapper := map[string]any{"mcpServers": valid}
	b, err := json.Marshal(wrapper)
	if err != nil {
		slog.Error("config: failed to serialize MCP server config", "err", err, "server_count", len(valid))
		return ""
	}
	return string(b)
}

// buildAgentConfigExclude builds the platform → inject_exclude map for non-platform
// sessions (webchat/API/cron). The "" key holds the global default; platform-specific
// keys override it. Nil values are omitted (meaning "not configured, fall through").
//
// NOTE: keep platform keys in sync with resolveInjectExcludeForAdmin (bot_config_adapter.go)
// and applyInjectExclude callers (messaging_init.go).
func buildAgentConfigExclude(cfg *config.Config) map[string][]string {
	m := make(map[string][]string)
	if cfg.AgentConfig.InjectExclude != nil {
		m[""] = cfg.AgentConfig.InjectExclude
	}
	if cfg.Messaging.Slack.InjectExclude != nil {
		m["slack"] = cfg.Messaging.Slack.InjectExclude
	}
	if cfg.Messaging.Feishu.InjectExclude != nil {
		m["feishu"] = cfg.Messaging.Feishu.InjectExclude
	}
	if cfg.Messaging.Yuanxin.InjectExclude != nil {
		m["yuanxin"] = cfg.Messaging.Yuanxin.InjectExclude
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
