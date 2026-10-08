package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/hrygo/hotplex/internal/admin"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"

	"github.com/hrygo/hotplex/pkg/events"
)

// apiSM is the narrow subset of SessionManager that GatewayAPI needs.
// Composed from canonical sub-interfaces defined in handler.go to avoid
// duplicate method declarations.
type apiSM interface {
	SessionReader
	SessionLifecycle
	SessionTransitioner
	SessionAdmin
}

type GatewayAPI struct {
	auth       *security.Authenticator
	sm         apiSM
	bridge     SessionStarter
	cfgStore   *config.ConfigStore
	turnsStore eventstore.TurnQuerier
	eventStore EventStoreReader
	wsStore    WorkspaceReader // WebChat 多租户 workspace 归属校验（spec ①）；nil = 未启用
	log        *slog.Logger
}

// EventStoreReader defines the subset of EventStore needed by the events API.
type EventStoreReader interface {
	QueryBySession(ctx context.Context, sessionID string, cursor int64, dir eventstore.CursorDirection, limit int) (*eventstore.EventPage, error)
}

// WorkspaceReader defines the subset of UserWorkspaceStore needed by the sessions
// API for workspace ownership validation (spec ①). Kept narrow so unit tests can
// mock a single method instead of the full UserWorkspaceStore.
type WorkspaceReader interface {
	GetWorkspaceByID(ctx context.Context, id string) (*session.Workspace, error)
}

func NewGatewayAPI(log *slog.Logger, auth *security.Authenticator, sm apiSM, bridge SessionStarter, cfgStore *config.ConfigStore, turnsStore eventstore.TurnQuerier, eventStore EventStoreReader, wsStore WorkspaceReader) *GatewayAPI {
	return &GatewayAPI{auth: auth, sm: sm, bridge: bridge, cfgStore: cfgStore, turnsStore: turnsStore, eventStore: eventStore, wsStore: wsStore, log: log.With("component", "api")}
}

func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// authorizeSession performs auth + path ID extraction + session lookup + ownership check.
// Returns (sessionID, sessionInfo, ok). If ok is false, an HTTP error has been written.
func (g *GatewayAPI) authorizeSession(w http.ResponseWriter, r *http.Request) (string, *session.SessionInfo, bool) {
	userID, _, _, err := g.auth.AuthenticateRequest(r)
	if err != nil {
		writeAppError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return "", nil, false
	}
	id := r.PathValue("id")
	if id == "" {
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "session id required")
		return "", nil, false
	}
	si, err := g.sm.Get(r.Context(), id)
	if err != nil {
		writeAppError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return "", nil, false
	}
	if si.State == events.StateDeleted || si.DeletedAt != nil {
		writeAppError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return "", nil, false
	}
	if si.UserID != userID {
		writeAppError(w, http.StatusForbidden, "FORBIDDEN", "ownership required")
		return "", nil, false
	}
	// WebChat 多租户防御深度：会话绑定的 workspace 必须仍属当前用户（spec §9.3）。
	// 平台/cron 会话 workspace_id 为空，跳过此检查（向后兼容）。
	if si.WorkspaceID != "" && g.wsStore != nil {
		ws, wsErr := g.wsStore.GetWorkspaceByID(r.Context(), si.WorkspaceID)
		if wsErr != nil || ws.OwnerUserID != userID {
			writeAppError(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", "ownership required")
			return "", nil, false
		}
	}
	return id, si, true
}

// ListSessions returns sessions belonging to the authenticated user.
//
// @Summary      List sessions
// @Description  Returns paginated list of sessions for the caller's user identity. Default platform filter is "webchat"; pass platform=all to include all platforms.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Param        limit     query    int     false  "Maximum results (1-500)"                                      default(100)
// @Param        offset    query    int     false  "Pagination offset"                                             default(0)
// @Param        platform  query    string  false  "Platform filter (webchat/slack/feishu/all)"  default(webchat)
// @Success      200  {object}  admin.GatewaySessionListResponse
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      500  {object}  admin.ErrorResponse  "Internal error"
// @Router       /api/sessions [get]
func (g *GatewayAPI) ListSessions(w http.ResponseWriter, r *http.Request) {
	userID, _, _, err := g.auth.AuthenticateRequest(r)
	if err != nil {
		g.log.Warn("gateway: list sessions auth failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}
	limit := 100
	offset := 0
	platform := platformWebChat // Default to webchat as requested

	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
			limit = v
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}
	if p := r.URL.Query().Get("platform"); p != "" {
		if p == "all" {
			platform = ""
		} else {
			platform = p
		}
	}

	// workspace_id 可选过滤（WebChat 多 workspace，spec §9.3）。归属校验后下推到 SQL。
	workspaceFilter := r.URL.Query().Get("workspace_id")
	if workspaceFilter != "" && g.wsStore != nil {
		ws, wsErr := g.wsStore.GetWorkspaceByID(r.Context(), workspaceFilter)
		if wsErr != nil || ws.OwnerUserID != userID {
			writeAppError(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", "not your workspace")
			return
		}
	}

	sessions, err := g.sm.List(r.Context(), userID, platform, workspaceFilter, limit, offset)
	if err != nil {
		g.log.Error("gateway: list sessions failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to list sessions")
		return
	}
	respondJSON(w, map[string]any{"sessions": sessions, "limit": limit, "offset": offset, "platform": platform, "workspace_id": workspaceFilter})
}

// CreateSession creates a new AI agent session bound to a workspace (WebChat multi-tenant, spec ①).
//
// @Summary      Create session
// @Description  Creates a new AI agent session. client_session_id is required. workspace_id is optional: omit it for legacy/platform/cron integrations (work_dir falls back to query param or config default); pass it on the WebChat multi-tenant track (caller must own it, 403 WORKSPACE_FORBIDDEN otherwise; work_dir taken from workspace, immutable). Session id is UUIDv5 derived from (userID, workerType, clientKey, workspaceID, workDir) — 方案3.
// @Tags         Gateway API
// @Accept       json
// @Produce      json
// @Security     ApiKeyAuth
// @Param        workspace_id      body     string  false  "Workspace ID (optional; required only on WebChat multi-tenant track, caller must own it)"
// @Param        client_session_id body     string  true   "Client-provided session identifier (max 256 chars)"
// @Param        title             body     string  false  "Human-readable session title"
// @Param        worker_type       body     string  false  "Worker type"      default(claudecode)
// @Success      200  {object}  admin.GatewayCreateSessionResponse
// @Failure      400  {object}  admin.ErrorResponse  "Missing client_session_id"
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "WORKSPACE_FORBIDDEN: not your workspace"
// @Failure      404  {object}  admin.ErrorResponse  "WORKSPACE_NOT_FOUND"
// @Failure      500  {object}  admin.ErrorResponse  "Failed to create session"
// @Router       /api/sessions [post]
func (g *GatewayAPI) CreateSession(w http.ResponseWriter, r *http.Request) {
	userID, botID, _, err := g.auth.AuthenticateRequest(r)
	if err != nil {
		g.log.Warn("gateway: create session auth failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}

	// Parse JSON body (preferred for WebChat multi-tenant) with query fallback
	// for legacy callers (client_session_id / title / worker_type as query params).
	var body struct {
		WorkspaceID     string `json:"workspace_id"`
		ClientSessionID string `json:"client_session_id"`
		Title           string `json:"title"`
		WorkerType      string `json:"worker_type"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // best-effort; query params may be used instead
	}
	clientSessionID := body.ClientSessionID
	if clientSessionID == "" {
		clientSessionID = strings.TrimSpace(r.URL.Query().Get("client_session_id"))
	}
	workspaceID := body.WorkspaceID
	if workspaceID == "" {
		workspaceID = strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	}
	title := body.Title
	if title == "" {
		title = strings.TrimSpace(r.URL.Query().Get("title"))
	}
	clientSessionID = messaging.SanitizeText(clientSessionID)
	if clientSessionID == "" {
		g.log.Warn("gateway: create session missing client_session_id", "method", r.Method, "path", r.URL.Path)
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "client_session_id is required")
		return
	}
	if len(clientSessionID) > session.MaxClientKeyLen {
		g.log.Warn("gateway: create session client_session_id too long", "method", r.Method, "path", r.URL.Path, "len", len(clientSessionID))
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("client_session_id too long (max %d chars)", session.MaxClientKeyLen))
		return
	}
	// Reject "|" in client_session_id: it is client-controlled and flows into
	// DeriveSessionKey's hash name, where it would alias session keys (review P3).
	if err := session.ValidateClientKey(clientSessionID); err != nil {
		g.log.Warn("gateway: create session invalid client_session_id", "method", r.Method, "path", r.URL.Path)
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "client_session_id must not contain '|'")
		return
	}
	title = messaging.SanitizeText(title)
	if len(title) > session.MaxClientKeyLen {
		g.log.Warn("gateway: create session title too long", "method", r.Method, "path", r.URL.Path, "title_len", len(title))
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("title too long (max %d chars)", session.MaxClientKeyLen))
		return
	}

	// workspace_id 可选（对齐 WS init conn.go:334-368 与 ListSessions api.go:89-90）：
	// 传了走多租户归属校验，work_dir 取自 workspace；不传则恢复 v1.29.0 legacy
	// 行为——work_dir 取 query work_dir 或 config 默认值，session key 的
	// workspaceID 维度为空。平台/cron/第三方 REST 集成无需 workspace 仍可建会话。
	var ws *session.Workspace
	var sessionWorkspaceID string
	if workspaceID != "" {
		if g.wsStore == nil {
			g.log.Error("gateway: workspace store not configured", "method", r.Method, "path", r.URL.Path)
			writeAppError(w, http.StatusInternalServerError, "INTERNAL", "workspace store unavailable")
			return
		}
		var wsErr error
		ws, wsErr = g.wsStore.GetWorkspaceByID(r.Context(), workspaceID)
		if wsErr != nil {
			writeAppError(w, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found")
			return
		}
		if ws.OwnerUserID != userID {
			writeAppError(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", "ownership required")
			return
		}
		sessionWorkspaceID = ws.ID
	}

	// work_dir resolution.
	// Workspace track: session-immutable, sourced from the workspace (spec §6.2).
	// Legacy track: client-provided query param or config default (v1.29.0 behavior).
	var workDir string
	if ws != nil {
		workDir = ws.WorkDir
		if err := security.ValidateWorkDir(workDir); err != nil {
			g.log.Error("workspace workDir failed validation", "method", r.Method, "path", r.URL.Path, "workspace_id", workspaceID, "err", err)
			writeAppError(w, http.StatusInternalServerError, "INVALID_WORK_DIR", "workspace work_dir is invalid")
			return
		}
	} else {
		workDir = strings.TrimSpace(r.URL.Query().Get("work_dir"))
		if workDir == "" {
			workDir = g.cfgStore.Load().Worker.DefaultWorkDir
		}
		if workDir != "" {
			expanded, err := validateAndExpandWorkDir(workDir)
			if err != nil {
				g.log.Warn("gateway: create session invalid work_dir", "method", r.Method, "path", r.URL.Path, "work_dir", workDir, "err", err)
				writeAppError(w, http.StatusBadRequest, "INVALID_WORK_DIR", err.Error())
				return
			}
			workDir = expanded
		}
	}

	// worker_type resolution: body/query > workspace.WorkerPreference > default.
	wt := worker.WorkerType(body.WorkerType)
	if wt == "" {
		wt = worker.WorkerType(r.URL.Query().Get("worker_type"))
	}
	// Validate request-supplied worker_type (body/query) at the boundary → 400
	// on unknown types. See spec ③ §7.2.
	if wt != "" {
		if err := worker.ValidateType(wt); err != nil {
			writeAppError(w, http.StatusBadRequest, "INVALID_WORKER_TYPE", err.Error())
			return
		}
	}
	// Fall back to the workspace preference. PATCH validates on write, but a
	// stale row written before that gate existed (spec ①-②), or a bypass write
	// (migration/manual SQL), could hold an unvalidated value. Re-validate and
	// degrade to the default rather than failing late at worker launch (review P2).
	if wt == "" && ws != nil {
		if pref := worker.WorkerType(ws.WorkerPreference); pref != "" {
			if err := worker.ValidateType(pref); err == nil {
				wt = pref
			} else {
				g.log.Info("workspace worker_preference invalid, falling back to default",
					"workspace_id", ws.ID, "worker_preference", pref, "err", err)
			}
		}
	}
	if wt == "" {
		wt = worker.TypeClaudeCode
	}

	// Session key 方案3: workspace_id participates in the UUIDv5 hash (spec §7).
	// Legacy track leaves sessionWorkspaceID empty — same derivation shape as
	// the WS path when initData.WorkspaceID is absent. Same (userID, wt,
	// clientKey, workspaceID, workDir) → same session id.
	id := session.DeriveSessionKey(userID, wt, clientSessionID, sessionWorkspaceID, workDir)

	// Default userID after derivation — bridge expects non-empty.
	if userID == "" {
		userID = "anonymous"
	}

	// Idempotency check: if session exists and is active, just return it.
	if si, err := g.sm.Get(r.Context(), id); err == nil {
		if si.State != events.StateDeleted && si.DeletedAt == nil {
			respondJSON(w, map[string]string{"session_id": id})
			return
		}
		writeAppError(w, http.StatusGone, "SESSION_DELETED", "session is deleted; create a new session")
		return
	}

	startParams := worker.SessionStartParams{
		ID:          id,
		UserID:      userID,
		BotID:       botID,
		WorkerType:  wt,
		WorkDir:     workDir,
		Platform:    platformWebChat,
		Title:       title,
		ClientKey:   clientSessionID,
		WorkspaceID: sessionWorkspaceID,
	}
	// AgentSpec shadow (#847, findings F4/F8): observationally verify the
	// normalized construction agrees with this legacy shape. REST has no
	// AllowedTools source (nil) — the documented WS≠REST divergence. The legacy
	// params stay authoritative in first-cut.
	in := BuildWebChatInput(wt, nil, userID, sessionWorkspaceID)
	ShadowCompareStartParams(g.log, in, startParams)
	ShadowResolvePlan(g.log, in)
	if err := g.bridge.StartSession(r.Context(), startParams); err != nil {
		g.log.Error("gateway: create session failed", "session_id", id, "worker_type", wt, "work_dir", workDir, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to create session")
		return
	}
	respondJSON(w, map[string]string{"session_id": id})
}

// GetSession returns details for a single session.
//
// @Summary      Get session
// @Description  Returns full session details. Ownership check: caller must own the session.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id   path      string  true  "Session ID"
// @Success      200  {object}  admin.SessionDetailResponse
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "Ownership required"
// @Failure      404  {object}  admin.ErrorResponse  "Session not found"
// @Router       /api/sessions/{id} [get]
func (g *GatewayAPI) GetSession(w http.ResponseWriter, r *http.Request) {
	_, si, ok := g.authorizeSession(w, r)
	if !ok {
		return
	}
	respondJSON(w, si)
}

// DeleteSession marks a session deleted and schedules durable cleanup.
//
// @Summary      Delete session
// @Description  Marks the session deleted, immediately hides it from history, and schedules cleanup. Ownership check applies.
// @Tags         Gateway API
// @Security     ApiKeyAuth
// @Param        id   path  string  true  "Session ID"
// @Success      204  "Session deleted"
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "Ownership required"
// @Failure      404  {object}  admin.ErrorResponse  "Session not found"
// @Failure      500  {object}  admin.ErrorResponse  "Failed to delete session"
// @Router       /api/sessions/{id} [delete]
func (g *GatewayAPI) DeleteSession(w http.ResponseWriter, r *http.Request) {
	id, _, ok := g.authorizeSession(w, r)
	if !ok {
		return
	}

	ctx := session.WithDeleteAuditRequest(r.Context(), r.RemoteAddr, r.UserAgent())
	if err := g.sm.Delete(ctx, id); err != nil {
		g.log.Error("gateway: delete session failed", "session_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to delete session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetSessionCleanupStatus returns non-content cleanup progress for a deleted
// session. It authorizes through the purge job's immutable ownership snapshot.
//
// @Summary      Get session cleanup status
// @Description  Returns bounded progress for a deleted session without exposing conversation content or provider error text.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id   path  string  true  "Session ID"
// @Success      200  {object}  admin.GatewaySessionCleanupStatusResponse
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "Ownership required"
// @Failure      404  {object}  admin.ErrorResponse  "Cleanup status not found"
// @Failure      503  {object}  admin.ErrorResponse  "Cleanup status unavailable"
// @Router       /api/sessions/{id}/cleanup [get]
func (g *GatewayAPI) GetSessionCleanupStatus(w http.ResponseWriter, r *http.Request) {
	userID, _, _, err := g.auth.AuthenticateRequest(r)
	if err != nil {
		writeAppError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "session id required")
		return
	}
	reader, ok := g.sm.(session.PurgeStatusReader)
	if !ok {
		writeAppError(w, http.StatusServiceUnavailable, "PURGE_STATUS_UNAVAILABLE", "cleanup status unavailable")
		return
	}
	status, err := reader.GetPurgeStatus(r.Context(), id, userID)
	if errors.Is(err, session.ErrPurgeNotFound) {
		writeAppError(w, http.StatusNotFound, "NOT_FOUND", "cleanup status not found")
		return
	}
	if err != nil {
		g.log.Error("gateway: load session cleanup status failed", "session_id", id, "err", err)
		writeAppError(w, http.StatusServiceUnavailable, "PURGE_STATUS_UNAVAILABLE", "cleanup status unavailable")
		return
	}
	if status.WorkspaceID != "" && g.wsStore != nil {
		ws, wsErr := g.wsStore.GetWorkspaceByID(r.Context(), status.WorkspaceID)
		if wsErr != nil || ws.OwnerUserID != userID {
			writeAppError(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", "ownership required")
			return
		}
	}
	response := admin.GatewaySessionCleanupStatusResponse{
		JobID:       status.JobID,
		SessionID:   status.SessionID,
		Status:      string(status.Status),
		RequestedAt: status.RequestedAt,
		UpdatedAt:   status.UpdatedAt,
		CompletedAt: status.CompletedAt,
		Items:       make([]admin.GatewaySessionCleanupStatusItem, 0, len(status.Items)),
	}
	for _, item := range status.Items {
		response.Items = append(response.Items, admin.GatewaySessionCleanupStatusItem{
			Kind:          item.Kind,
			Status:        string(item.Status),
			Attempts:      item.Attempts,
			NextAttemptAt: item.NextAttemptAt,
			CompletedAt:   item.CompletedAt,
			ErrorCode:     item.ErrorCode,
		})
	}
	respondJSON(w, response)
}

// SwitchWorkDir changes the working directory for a session.
//
// @Summary      Switch working directory
// @Description  Changes the working directory for an active session. Creates a new derived session ID. Ownership check applies.
// @Tags         Gateway API
// @Accept       json
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id    path      string                     true  "Session ID"
// @Param        body  body      admin.SwitchWorkDirRequest  true  "New working directory"
// @Success      200   {object}  admin.SwitchWorkDirResponse
// @Failure      400   {object}  admin.ErrorResponse  "Invalid body or path"
// @Failure      401   {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403   {object}  admin.ErrorResponse  "Ownership required"
// @Failure      409   {object}  admin.ErrorResponse  "Session not active"
// @Failure      500   {object}  admin.ErrorResponse  "Internal error"
// @Router       /api/sessions/{id}/cd [post]
func (g *GatewayAPI) SwitchWorkDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkDir string `json:"work_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		g.log.Warn("gateway: switch workdir invalid body", "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if body.WorkDir == "" {
		g.log.Warn("gateway: switch workdir missing work_dir", "method", r.Method, "path", r.URL.Path)
		writeAppError(w, http.StatusBadRequest, "BAD_REQUEST", "work_dir is required")
		return
	}

	// Expand ~ and resolve to absolute path.
	expanded, err := validateAndExpandWorkDir(body.WorkDir)
	if err != nil {
		g.log.Warn("gateway: switch workdir invalid path", "method", r.Method, "path", r.URL.Path, "work_dir", body.WorkDir, "err", err)
		writeAppError(w, http.StatusBadRequest, "INVALID_WORK_DIR", err.Error())
		return
	}
	body.WorkDir = expanded

	_, si, ok := g.authorizeSession(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	if !si.State.IsActive() {
		g.log.Warn("gateway: switch workdir session not active", "session_id", id, "method", r.Method, "path", r.URL.Path, "state", si.State)
		writeAppError(w, http.StatusConflict, "SESSION_NOT_ACTIVE", "session not active")
		return
	}

	// Workspace-bound WebChat sessions derive work_dir from their workspace, which
	// is immutable (spec §6.2 — enforced at CreateSession, api.go ~L258). Allowing
	// /cd here would start the worker in a directory its workspace doesn't own
	// while keeping the session bound to the original workspace, breaking that
	// invariant — and (because DeleteWorkspaceIfEmpty counts active sessions by
	// workspace_id, not work_dir) could let another workspace be hard-deleted
	// while a worker is still running in its directory (review fix).
	// Platform/messaging sessions (WorkspaceID == "") legitimately support /cd.
	if si.WorkspaceID != "" {
		writeAppError(w, http.StatusBadRequest, "WORK_DIR_IMMUTABLE",
			"work_dir is immutable for workspace-bound sessions; use a different workspace")
		return
	}

	// Delegate to bridge.
	result, err := g.bridge.SwitchWorkDir(r.Context(), id, body.WorkDir)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) || strings.Contains(err.Error(), "not a directory") {
			g.log.Warn("gateway: switch workdir bad path", "session_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
			writeAppError(w, http.StatusBadRequest, "INVALID_WORK_DIR", err.Error())
			return
		}
		g.log.Error("gateway: switch workdir failed", "session_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	respondJSON(w, map[string]string{
		"old_session_id": result.OldSessionID,
		"new_session_id": result.NewSessionID,
		"work_dir":       result.WorkDir,
	})
}

// GetHistory returns turn history for a session.
//
// @Summary      Get session history
// @Description  Returns paginated turn records for a session. Use before_id for cursor-based pagination. Ownership check applies.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id         path      string  true   "Session ID"
// @Param        limit      query     int     false  "Max records (1-200)"  default(50)
// @Param        before_id  query     int     false  "Return records before this turn ID"
// @Success      200  {object}  admin.HistoryResponse
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "Ownership required"
// @Failure      500  {object}  admin.ErrorResponse  "Failed to get history"
// @Router       /api/sessions/{id}/history [get]
func (g *GatewayAPI) GetHistory(w http.ResponseWriter, r *http.Request) {
	id, _, ok := g.authorizeSession(w, r)
	if !ok {
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}

	beforeID := int64(0)
	if bid := r.URL.Query().Get("before_id"); bid != "" {
		if v, err := strconv.ParseInt(bid, 10, 64); err == nil && v > 0 {
			beforeID = v
		}
	}

	if g.turnsStore == nil {
		respondJSON(w, map[string]any{"records": []any{}, "has_more": false})
		return
	}

	fetchLimit := limit + 1
	var (
		records []*eventstore.TurnRecord
		err     error
	)

	if beforeID > 0 {
		records, err = g.turnsStore.QueryTurnsBefore(r.Context(), id, beforeID, fetchLimit)
	} else {
		records, err = g.turnsStore.QueryLatestTurns(r.Context(), id, fetchLimit)
	}

	if err != nil {
		if errors.Is(err, eventstore.ErrNotFound) {
			respondJSON(w, map[string]any{"records": []any{}, "has_more": false})
			return
		}
		g.log.Error("gateway: get history failed", "session_id", id, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to get history")
		return
	}

	hasMore := len(records) > limit
	if hasMore {
		// Store returns ASC (newest at the end). Slicing from the END keeps
		// the latest exchange; `records[:limit]` would drop it (refresh bug).
		records = records[len(records)-limit:]
	}

	respondJSON(w, map[string]any{"records": records, "has_more": hasMore})
}

// GetEvents returns AEP events for a session.
//
// @Summary      Get session events
// @Description  Returns paginated AEP events for a session. Use cursor and direction for navigation. Ownership check applies.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id         path      string  true   "Session ID"
// @Param        limit      query     int     false  "Max events (1-1000)"                              default(200)
// @Param        cursor     query     int     false  "Persisted event row ID cursor"
// @Param        direction  query     string  false  "Cursor direction (after/before/latest)"  default(latest)
// @Success      200  {object}  admin.EventsResponse
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Failure      403  {object}  admin.ErrorResponse  "Ownership required"
// @Failure      500  {object}  admin.ErrorResponse  "Failed to get events"
// @Router       /api/sessions/{id}/events [get]
func (g *GatewayAPI) GetEvents(w http.ResponseWriter, r *http.Request) {
	if g.eventStore == nil {
		respondJSON(w, map[string]any{"events": []any{}, "oldest_id": 0, "newest_id": 0, "oldest_seq": 0, "newest_seq": 0, "has_older": false})
		return
	}

	id, _, ok := g.authorizeSession(w, r)
	if !ok {
		return
	}

	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	var cursor int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		if v, err := strconv.ParseInt(c, 10, 64); err == nil && v > 0 {
			cursor = v
		}
	}

	var dir eventstore.CursorDirection
	switch d := r.URL.Query().Get("direction"); d {
	case "after":
		dir = eventstore.CursorAfter
	case "before":
		dir = eventstore.CursorBefore
	default:
		dir = eventstore.CursorLatest
	}

	page, err := g.eventStore.QueryBySession(r.Context(), id, cursor, dir, limit)
	if err != nil {
		if errors.Is(err, eventstore.ErrNotFound) {
			respondJSON(w, map[string]any{"events": []any{}, "oldest_id": 0, "newest_id": 0, "oldest_seq": 0, "newest_seq": 0, "has_older": false})
			return
		}
		g.log.Error("gateway: get events failed", "session_id", id, "err", err)
		writeAppError(w, http.StatusInternalServerError, "INTERNAL", "failed to get events")
		return
	}

	respondJSON(w, page)
}

// WorkerInstallationStatus represents the installation status of a worker binary.
type WorkerInstallationStatus struct {
	Type      string `json:"type"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
}

// ListWorkers returns a list of all registered worker types and their installation status.
//
// @Summary      List worker installation status
// @Description  Returns a list of all registered worker types and whether their binaries are installed in the host's PATH.
// @Tags         Gateway API
// @Produce      json
// @Security     ApiKeyAuth
// @Success      200  {array}  WorkerInstallationStatus
// @Failure      401  {object}  admin.ErrorResponse  "Unauthorized"
// @Router       /api/workers [get]
func (g *GatewayAPI) ListWorkers(w http.ResponseWriter, r *http.Request) {
	_, _, _, err := g.auth.AuthenticateRequest(r)
	if err != nil {
		g.log.Warn("gateway: list workers auth failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeAppError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}

	cfg := g.cfgStore.Load()
	workerCommands := map[worker.WorkerType]string{
		worker.TypeClaudeCode:  cfg.Worker.ClaudeCode.Command,
		worker.TypeOpenCodeSrv: cfg.Worker.OpenCodeServer.Command,
		worker.TypeCodexCLI:    cfg.Worker.CodexCLI.Command,
		worker.TypeACP:         cfg.Worker.ACP.Command,
	}

	knownTypes := []worker.WorkerType{
		worker.TypeClaudeCode,
		worker.TypeOpenCodeSrv,
		worker.TypeCodexCLI,
		worker.TypeACP,
	}

	var statusList []WorkerInstallationStatus
	for _, wt := range knownTypes {
		cmdStr := workerCommands[wt]
		if cmdStr == "" {
			switch wt {
			case worker.TypeClaudeCode:
				cmdStr = "claude"
			case worker.TypeOpenCodeSrv:
				cmdStr = "opencode"
			case worker.TypeCodexCLI:
				cmdStr = "codex"
			case worker.TypeACP:
				cmdStr = "hermes"
			}
		}

		parts := strings.Fields(cmdStr)
		var binary string
		if len(parts) > 0 {
			binary = parts[0]
		}

		installed := false
		path := ""
		if binary != "" {
			p, err := exec.LookPath(binary)
			if err == nil {
				installed = true
				path = p
			}
		}

		statusList = append(statusList, WorkerInstallationStatus{
			Type:      string(wt),
			Installed: installed,
			Path:      path,
		})
	}

	respondJSON(w, statusList)
}
