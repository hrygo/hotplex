package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
)

// Audit action enumeration (issue #788 A5). Persisted as the "action" field of
// admin_audit slog records. Stable strings — never rename without a consumer
// audit, log dashboards filter on these.
const (
	AuditGatewayRestart                = "gateway.restart"
	AuditBotCreate                     = "bot.create"
	AuditBotUpdate                     = "bot.update"
	AuditBotDelete                     = "bot.delete"
	AuditAPIKeyCreate                  = "apikey.create"
	AuditAPIKeyUpdate                  = "apikey.update"
	AuditAPIKeyDelete                  = "apikey.delete"
	AuditCronCreate                    = "cron.create"
	AuditCronUpdate                    = "cron.update"
	AuditCronDelete                    = "cron.delete"
	AuditCronTrigger                   = "cron.trigger"
	AuditSessionDelete                 = "session.delete"
	AuditSessionTerminate              = "session.terminate"
	AuditSessionPatch                  = "session.patch"
	AuditSessionPut                    = "session.put"
	AuditConfigRollback                = "config.rollback"
	AuditConfigValidate                = "config.validate"
	AuditMemberStatusUpdate            = "member.status.update"
	AuditUserPasswordReset             = "user.password_reset"
	AuditInvitationCreate              = "invitation.create"
	AuditInvitationDelete              = "invitation.delete"
	AuditWorkspacePermissionModeUpdate = "workspace.permission_mode.update" // issue #807 admin console
	AuditIdentityLinkCreate            = "audit.identity_link.create"
	AuditIdentityLinkDelete            = "audit.identity_link.delete"
	AuditAuthDenied                    = "auth.denied"
	AuditSkillCreate                   = "skill.create" // issue #910 global skill install
	AuditSkillUpdate                   = "skill.update" // ?replace=true 覆盖
	AuditSkillDelete                   = "skill.delete"
	AuditRuntimeFenceAction            = "runtime.fence.action"  // #877 decision-agnostic middleware line
	AuditRuntimeFenceResolve           = "runtime.fence.resolve" // #877 specific decision (user_activity)
	AuditRuntimeFenceAbandon           = "runtime.fence.abandon" // #877 specific decision (user_activity)
	AuditRuntimeEffectAction           = "runtime.effect.action" // decision-agnostic middleware line
	AuditRuntimeEffectAbandon          = "runtime.effect.abandon"
	AuditRuntimeEffectMarkDelivered    = "runtime.effect.mark_delivered"
	AuditRuntimeEffectRequeue          = "runtime.effect.requeue"

	// AuditResult* — stable "result" field values. Reuse instead of literals so
	// dashboard filters stay correct (issue #788 review P3).
	AuditResultOk     = "ok"
	AuditResultFailed = "failed"
	AuditResultDenied = "denied"
)

// auditLoggerOverride, when non-nil, redirects admin_audit records to a
// specific logger (tests). Production never sets it, so AdminAudit follows
// slog.Default() at call time — the handler configured by initLogging
// (JSON + optional lumberjack file), NOT the logger captured at package
// init. Historically the package captured slog.Default() into a plain var,
// so production admin_audit lines bypassed the configured pipeline.
var auditLogger atomic.Pointer[slog.Logger]

// SetAuditLogger redirects admin_audit records. Tests use it to capture
// audits without spinning the real slog pipeline; production never calls it.
// Passing nil clears the override so AdminAudit follows slog.Default() again
// (the restore contract for tests that swap the logger).
func SetAuditLogger(l *slog.Logger) {
	auditLogger.Store(l)
}

// currentAuditLogger returns the explicit override if one is installed,
// otherwise the current process-wide default logger.
func currentAuditLogger() *slog.Logger {
	if l := auditLogger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// AdminAudit records a structured admin action for compliance and incident
// forensics (issue #788 A5). No new storage — rides the existing slog pipeline.
// actor is a uid (cookie channel) or "admin-token"/"anonymous" (Bearer/failed);
// target is the request path (with ids); result is one of the AuditResult*.
func AdminAudit(actor, action, target, result string) {
	currentAuditLogger().Info("admin_audit",
		"actor_user_id", actor,
		"action", action,
		"target", target,
		"result", result,
	)
}

// isWriteMethod reports whether the method mutates server state (issue #788 A5).
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// adminActionFor maps a (method, path) to a stable audit action enum. Falls back
// to "<method> <path>" for unmapped routes so no write is silently lost. Order
// matters for sub-resources: /terminate and /run must precede their collection
// cases (/sessions, /cron/jobs) since they also contain those substrings.
func adminActionFor(method, path string) string {
	switch {
	case strings.Contains(path, "/restart"):
		return AuditGatewayRestart
	case strings.Contains(path, "/terminate"):
		return AuditSessionTerminate
	case strings.HasSuffix(path, "/run") && strings.Contains(path, "/cron/"):
		return AuditCronTrigger
	case strings.Contains(path, "/fence-action"):
		return AuditRuntimeFenceAction
	case strings.Contains(path, "/effects/"):
		// GET /admin/effects/{id} is a read; only the write verb reaches the
		// audit table, and it stays decision-agnostic here — the
		// decision-specific row is written by the handler itself.
		return AuditRuntimeEffectAction
	case strings.Contains(path, "/config/rollback"):
		return AuditConfigRollback
	case strings.Contains(path, "/config/validate"):
		return AuditConfigValidate
	case strings.Contains(path, "/audit/identity-links"):
		return auditIdentityLinkAction(method)
	case strings.Contains(path, "/api-keys"):
		return apiKeyAction(method)
	case strings.Contains(path, "/cron/jobs"):
		return cronAction(method)
	case strings.Contains(path, "/bots"):
		return botAction(method)
	case strings.Contains(path, "/sessions"):
		return sessionAction(method)
	case strings.Contains(path, "/skills"):
		return skillAction(method)
	case strings.Contains(path, "/workspaces"):
		return workspaceAction(method)
	}
	return method + " " + path
}

// skillAction 覆盖 /admin/api/skills 全局 skill 写（issue #910）。GET 不审计
// （isWriteMethod 为 false）。POST 区分 create vs update 由 routes 层 ?replace
// 决定，这里按 method 取语义动作；replace=true 的 POST 在 handler 内覆盖同名。
func skillAction(method string) string {
	switch method {
	case http.MethodPost:
		return AuditSkillCreate
	case http.MethodDelete:
		return AuditSkillDelete
	case http.MethodPatch, http.MethodPut:
		return AuditSkillUpdate
	}
	return "skill." + strings.ToLower(method)
}

func auditIdentityLinkAction(method string) string {
	switch method {
	case http.MethodPost:
		return AuditIdentityLinkCreate
	case http.MethodDelete:
		return AuditIdentityLinkDelete
	}
	return "audit.identity_link." + strings.ToLower(method)
}

// workspaceAction covers PATCH /admin/workspaces/{id} (issue #807) — the only
// admin workspace write. GET /admin/workspaces (list) isn't audited
// (isWriteMethod is false). The /workspaces branch sits last in adminActionFor
// so it can't shadow the /sessions, /bots, /cron, /api-keys branches above
// (none of those paths contain "/workspaces" as a substring).
func workspaceAction(method string) string {
	switch method {
	case http.MethodPatch, http.MethodPut:
		return AuditWorkspacePermissionModeUpdate
	}
	return "workspace." + strings.ToLower(method)
}

func botAction(method string) string {
	switch method {
	case http.MethodPost:
		return AuditBotCreate
	case http.MethodPatch, http.MethodPut:
		return AuditBotUpdate
	case http.MethodDelete:
		return AuditBotDelete
	}
	return "bot." + strings.ToLower(method)
}

func apiKeyAction(method string) string {
	switch method {
	case http.MethodPost:
		return AuditAPIKeyCreate
	case http.MethodPatch, http.MethodPut:
		return AuditAPIKeyUpdate
	case http.MethodDelete:
		return AuditAPIKeyDelete
	}
	return "apikey." + strings.ToLower(method)
}

func cronAction(method string) string {
	switch method {
	case http.MethodPost:
		return AuditCronCreate
	case http.MethodPatch, http.MethodPut:
		return AuditCronUpdate
	case http.MethodDelete:
		return AuditCronDelete
	}
	return "cron." + strings.ToLower(method)
}

// sessionAction covers DELETE — the only routed session write
// (DELETE /admin/sessions/{id}, routes.go) — plus PATCH/PUT, which are NOT
// routed as admin session handlers (admin sessions expose only GET / DELETE /
// POST-terminate; see routes.go). The PATCH/PUT branches exist so a 404 on
// those un-routed methods still produces a descriptive audit action: the
// Middleware audit defer (admin.go) calls adminActionFor before the 404
// reaches its handler, yielding session.patch / session.put rather than a
// generic "PATCH /admin/sessions/..". terminate is matched earlier in
// adminActionFor (POST /admin/sessions/{id}/terminate → AuditSessionTerminate).
// Delete the PATCH/PUT branches only if you accept losing those 404 labels.
func sessionAction(method string) string {
	switch method {
	case http.MethodDelete:
		return AuditSessionDelete
	case http.MethodPatch:
		return AuditSessionPatch
	case http.MethodPut:
		return AuditSessionPut
	}
	return "session." + strings.ToLower(method)
}

// resourceTypeFromAction maps a slog-style audit action (e.g. "bot.create") to
// the user_activity.resource_type value ("bot"). Falls back to "admin" for
// unmapped actions so the column is never empty for admin writes.
func resourceTypeFromAction(slogAction string) string {
	// slog actions are "<resource>.<verb>" (see AuditBotCreate = "bot.create").
	if i := strings.IndexByte(slogAction, '.'); i > 0 {
		return slogAction[:i]
	}
	return "admin"
}

// mapOutcome converts the legacy slog AuditResult* string to the user_activity
// outcome enum. The two namespaces differ deliberately: slog uses "ok"/"failed"
// (issue #788 dashboard contract), user_activity uses "success"/"failure"
// (spec §5.1). "denied" passes through unchanged.
func mapOutcome(slogResult string) string {
	switch slogResult {
	case AuditResultOk:
		return audit.OutcomeSuccess
	case AuditResultFailed:
		return audit.OutcomeFailure
	case AuditResultDenied:
		return audit.OutcomeDenied
	default:
		return audit.OutcomeFailure
	}
}

// enqueueAdminActivity records an admin write into the tamper-evident
// user_activity table (issue #833 P2 dual-write, spec §7). Non-blocking;
// no-op when no audit collector is wired (a.auditCollector == nil). Called
// from the Middleware audit defer alongside the legacy slog AdminAudit path.
//
// The user_activity.action is prefixed with "admin." per spec §5.2 (e.g.
// "bot.create" → "admin.bot.create") so admin writes are distinguishable from
// other action namespaces (auth.* / session.* / message.* / tool.*) in the
// unified table. detail_json carries the method, path, and status — enough to
// reconstruct the request without re-reading slog.
func (a *AdminAPI) enqueueAdminActivity(r *http.Request, status int, actor, slogAction string) {
	enqueueAdminActivity(a.auditCollector, r, status, actor, slogAction)
}

// actorIdentity maps an admin actor string to the user_activity identity pair.
// Shared by the middleware dual-write and handler-specific audit rows (#877)
// so both namespaces agree on who acted.
func actorIdentity(actor string) (userID, userIDType string) {
	userID = actor
	userIDType = audit.UserIDTypeRegistered // admin cookie → users.id
	switch actor {
	case "", "anonymous":
		userID = audit.AnonymousUserID
		userIDType = audit.UserIDTypeAnonymous
	case "admin-token":
		userIDType = audit.UserIDTypeSystem
	}
	return userID, userIDType
}

func enqueueAdminActivity(c *audit.Collector, r *http.Request, status int, actor, slogAction string) {
	if c == nil {
		return
	}
	userID, userIDType := actorIdentity(actor)
	outcome := audit.OutcomeSuccess
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		outcome = audit.OutcomeDenied
	} else if status >= 400 {
		outcome = audit.OutcomeFailure
	}
	detail, _ := json.Marshal(map[string]any{
		"method":      r.Method,
		"path":        r.URL.Path,
		"status":      status,
		"slog_action": slogAction, // preserve the legacy label for dashboard migration
	})
	ua := &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   userIDType,
		Platform:     audit.PlatformAdmin,
		Action:       "admin." + slogAction,
		ResourceType: resourceTypeFromAction(slogAction),
		Outcome:      outcome,
		DetailJSON:   string(detail),
		IP:           clientIP(r),
		UserAgent:    r.UserAgent(),
	}
	_ = c.Enqueue(context.Background(), ua)
}
