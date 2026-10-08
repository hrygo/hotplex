// Package admin defines request/response types used exclusively for OpenAPI schema generation.
// These types are referenced by swaggo annotations on HTTP handlers and do not participate
// in runtime serialization — handlers construct responses via respondJSON(map[string]any{...}).
package admin

import "time"

// AppError is the schema-only mirror of web.AppError (internal/web). Defined
// locally rather than imported so swaggo can resolve the type during OpenAPI
// generation; the runtime envelope is produced by web.WriteAppError.
type AppError struct {
	Code    string `json:"code" example:"NOT_FOUND"`
	Message string `json:"message" example:"resource not found"`
}

// ErrorResponse is the standard error envelope returned by all endpoints.
// Shape: {"error":{"code":...,"message":...}} (see web.WriteAppError).
type ErrorResponse struct {
	Error AppError `json:"error"`
}

// StatusResponse is returned for simple ok/status operations.
type StatusResponse struct {
	Status string `json:"status" example:"ok"`
}

// SessionListResponse is returned by GET /admin/sessions.
type SessionListResponse struct {
	Sessions []any `json:"sessions"`
	Limit    int   `json:"limit" example:"100"`
	Offset   int   `json:"offset" example:"0"`
}

// WorkspaceListResponse is returned by GET /admin/workspaces (issue #807). Each
// entry is a session.AdminWorkspaceView (workspace row + owner_display_name +
// owner_username). Schema-only — handlers respond via respondJSON(map[string]any).
type WorkspaceListResponse struct {
	Workspaces []any `json:"workspaces"`
}

// WorkspaceResponse mirrors session.Workspace for the PATCH /admin/workspaces/{id}
// success schema (issue #807). Defined locally because swaggo indexes types only
// within the admin package — it cannot resolve cross-package session.Workspace.
// Runtime serialization is unaffected (handlers return the real *session.Workspace).
type WorkspaceResponse struct {
	ID                   string `json:"id" example:"ws-uuid"`
	OwnerUserID          string `json:"owner_user_id"`
	Name                 string `json:"name" example:"my-project"`
	WorkDir              string `json:"work_dir" example:"/home/u/.hotplex/workspaces/uid/my-project"`
	AgentConfigOverrides string `json:"agent_config_overrides"`
	WorkerPreference     string `json:"worker_preference"`
	PermissionMode       string `json:"permission_mode" example:"workspace"`
	Status               string `json:"status" example:"active"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

// CreateSessionResponse is returned by POST /admin/sessions.
type CreateSessionResponse struct {
	SessionID string `json:"session_id" example:"550e8400-e29b-41d4-a716-446655440000"`
}

// GatewaySessionListResponse is returned by GET /api/sessions.
type GatewaySessionListResponse struct {
	Sessions []any  `json:"sessions"`
	Limit    int    `json:"limit" example:"100"`
	Offset   int    `json:"offset" example:"0"`
	Platform string `json:"platform" example:"webchat"`
}

// GatewaySessionCleanupStatusResponse is the schema for GET /api/sessions/{id}/cleanup.
type GatewaySessionCleanupStatusResponse struct {
	JobID       string                            `json:"job_id"`
	SessionID   string                            `json:"session_id"`
	Status      string                            `json:"status" example:"in_progress"`
	RequestedAt time.Time                         `json:"requested_at"`
	UpdatedAt   time.Time                         `json:"updated_at"`
	CompletedAt *time.Time                        `json:"completed_at,omitempty"`
	Items       []GatewaySessionCleanupStatusItem `json:"items"`
}

// GatewaySessionCleanupStatusItem is a content-free cleanup component status.
type GatewaySessionCleanupStatusItem struct {
	Kind          string     `json:"kind" example:"conversation_content"`
	Status        string     `json:"status" example:"retrying"`
	Attempts      int        `json:"attempts" example:"2"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty" example:"cleanup_failed"`
}

// GatewayCreateSessionResponse is returned by POST /api/sessions.
// Aliased to CreateSessionResponse — same shape, separate name for Gateway API tag grouping.
type GatewayCreateSessionResponse = CreateSessionResponse

// SwitchWorkDirRequest is the request body for POST /api/sessions/{id}/cd.
type SwitchWorkDirRequest struct {
	WorkDir string `json:"work_dir" example:"/home/user/project"`
}

// SwitchWorkDirResponse is returned by POST /api/sessions/{id}/cd.
type SwitchWorkDirResponse struct {
	OldSessionID string `json:"old_session_id"`
	NewSessionID string `json:"new_session_id"`
	WorkDir      string `json:"work_dir"`
}

// HistoryResponse is returned by GET /api/sessions/{id}/history.
type HistoryResponse struct {
	Records []any `json:"records"`
	HasMore bool  `json:"has_more"`
}

// EventsResponse is returned by GET /api/sessions/{id}/events.
type EventsResponse struct {
	Events    []any `json:"events"`
	OldestID  int64 `json:"oldest_id"`
	NewestID  int64 `json:"newest_id"`
	OldestSeq int64 `json:"oldest_seq"`
	NewestSeq int64 `json:"newest_seq"`
	HasOlder  bool  `json:"has_older"`
}

// GatewayStatsDetail holds per-component gateway statistics.
type GatewayStatsDetail struct {
	UptimeSeconds        int `json:"uptime_seconds"`
	WebsocketConnections int `json:"websocket_connections"`
	SessionsActive       int `json:"sessions_active"`
	SessionsTotal        int `json:"sessions_total"`
}

// DatabaseStatsDetail holds database statistics.
type DatabaseStatsDetail struct {
	SessionsCount int `json:"sessions_count"`
}

// StatsResponse is returned by GET /admin/stats.
type StatsResponse struct {
	Gateway  GatewayStatsDetail     `json:"gateway"`
	Workers  map[string]interface{} `json:"workers"`
	Database DatabaseStatsDetail    `json:"database"`
}

// HealthResponse is returned by GET /admin/health.
type HealthResponse struct {
	Status  string                 `json:"status" example:"healthy"`
	Checks  map[string]interface{} `json:"checks"`
	Version string                 `json:"version"`
}

// WorkerHealthResponse is returned by GET /admin/health/workers.
type WorkerHealthResponse struct {
	Status    string `json:"status" example:"ok"`
	Workers   []any  `json:"workers"`
	CheckedAt string `json:"checked_at" example:"2024-01-01T00:00:00Z"`
}

// LogEntry is a single log record returned by GET /admin/logs.
type LogEntry struct {
	Time      string `json:"time"`
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	SessionID string `json:"session_id,omitempty"`
}

// LogsResponse is returned by GET /admin/logs.
type LogsResponse struct {
	Logs  []LogEntry `json:"logs"`
	Total int        `json:"total"`
	Limit int        `json:"limit" example:"100"`
}

// PoolStatsResponse is returned by GET /admin/sessions/pool.
type PoolStatsResponse struct {
	Total int `json:"total"`
	Max   int `json:"max"`
	Users int `json:"users"`
}

// ConfigValidateRequest is the request body for POST /admin/config/validate.
type ConfigValidateRequest struct {
	Gateway  *ConfigValidateGateway  `json:"gateway,omitempty"`
	DB       *ConfigValidateDB       `json:"db,omitempty"`
	Worker   *ConfigValidateWorker   `json:"worker,omitempty"`
	Security *ConfigValidateSecurity `json:"security,omitempty"`
	Session  *ConfigValidateSession  `json:"session,omitempty"`
	Pool     *ConfigValidatePool     `json:"pool,omitempty"`
}

// ConfigValidateGateway holds gateway sub-config for validation.
type ConfigValidateGateway struct {
	Addr               string `json:"addr,omitempty"`
	ReadBufferSize     int    `json:"read_buffer_size,omitempty"`
	WriteBufferSize    int    `json:"write_buffer_size,omitempty"`
	BroadcastQueueSize int    `json:"broadcast_queue_size,omitempty"`
}

// ConfigValidateDB holds DB sub-config for validation.
type ConfigValidateDB struct {
	Path string `json:"path,omitempty"`
}

// ConfigValidateWorker holds worker sub-config for validation.
type ConfigValidateWorker struct {
	IdleTimeout      string `json:"idle_timeout,omitempty"`
	ExecutionTimeout string `json:"execution_timeout,omitempty"`
}

// ConfigValidateSecurity holds security sub-config for validation.
type ConfigValidateSecurity struct {
	TLSEnabled bool `json:"tls_enabled,omitempty"`
}

// ConfigValidateSession holds session sub-config for validation.
type ConfigValidateSession struct {
	RetentionPeriod string `json:"retention_period,omitempty"`
	GCScanInterval  string `json:"gc_scan_interval,omitempty"`
}

// ConfigValidatePool holds pool sub-config for validation.
type ConfigValidatePool struct {
	MaxSize int `json:"max_size,omitempty"`
}

// ConfigValidateResponse is returned by POST /admin/config/validate.
type ConfigValidateResponse struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// RollbackRequest is the request body for POST /admin/config/rollback.
type RollbackRequest struct {
	Version int `json:"version" example:"3"`
}

// RollbackResponse is returned by POST /admin/config/rollback.
type RollbackResponse struct {
	OK           bool `json:"ok"`
	RolledBack   int  `json:"rolled_back"`
	HistoryIndex int  `json:"history_index"`
}

// DebugSessionResponse is returned by GET /admin/debug/sessions/{id}.
type DebugSessionResponse struct {
	Session map[string]interface{} `json:"session"`
	Debug   DebugInfo              `json:"debug"`
}

// DebugInfo holds debug details for a session.
type DebugInfo struct {
	Available    bool `json:"available"`
	HasWorker    bool `json:"has_worker"`
	TurnCount    int  `json:"turn_count"`
	LastSeqSent  any  `json:"last_seq_sent"`
	WorkerHealth any  `json:"worker_health"`
}

// RestartResponse is returned by POST /admin/restart.
type RestartResponse struct {
	Status string `json:"status" example:"restarting"`
}

// CreateAPIKeyRequest is the request body for POST /admin/api-keys.
type CreateAPIKeyRequest struct {
	UserID      string `json:"user_id" example:"alice"`
	Description string `json:"description,omitempty" example:"CI bot token"`
}

// SystemPromptPreviewResponse is returned by GET /admin/bots/{name}/preview.
type SystemPromptPreviewResponse struct {
	Preview string `json:"preview"`
}

// WriteAgentConfigRequest is the request body for PUT /admin/bots/{name}/config/{file}.
type WriteAgentConfigRequest struct {
	Content string `json:"content" example:"You are a helpful assistant."`
}

// CreateBotRequest is the request body for POST /admin/bots.
type CreateBotRequest struct {
	Name string `json:"name" example:"my-bot"`
	BotConfigAttrs
}

// CronJobCreateRequest is the request body for POST /admin/cron/jobs.
type CronJobCreateRequest struct {
	Name        string `json:"name" example:"daily-health"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Schedule    struct {
		Kind    string `json:"kind" example:"cron"`
		At      string `json:"at,omitempty" example:"2026-01-01T09:00:00+08:00"`
		EveryMs int64  `json:"every_ms,omitempty" example:"1800000"`
		Expr    string `json:"expr,omitempty" example:"0 9 * * 1-5"`
		TZ      string `json:"tz,omitempty" example:"Asia/Shanghai"`
	} `json:"schedule"`
	Payload struct {
		Kind            string   `json:"kind" example:"isolated_session"`
		Message         string   `json:"message"`
		TargetSessionID string   `json:"target_session_id,omitempty"`
		AllowedTools    []string `json:"allowed_tools,omitempty"`
		WorkerType      string   `json:"worker_type,omitempty"`
	} `json:"payload"`
	WorkDir        string            `json:"work_dir,omitempty"`
	BotID          string            `json:"bot_id" example:"bot_xxx"`
	OwnerID        string            `json:"owner_id" example:"ou_xxx"`
	Platform       string            `json:"platform,omitempty"`
	PlatformKey    map[string]string `json:"platform_key,omitempty"`
	TimeoutSec     int               `json:"timeout_sec,omitempty"`
	DeleteAfterRun bool              `json:"delete_after_run,omitempty"`
	Silent         bool              `json:"silent,omitempty"`
	MaxRetries     int               `json:"max_retries,omitempty"`
	MaxRuns        int               `json:"max_runs,omitempty"`
	ExpiresAt      string            `json:"expires_at,omitempty" example:"2027-01-01T00:00:00+08:00"`
}

// CronJobUpdateRequest is the request body for PATCH /admin/cron/jobs/{id}.
// All fields are optional; only provided fields are updated.
type CronJobUpdateRequest = CronJobCreateRequest

// CronJobResponse is returned by GET /admin/cron/jobs/{id}.
type CronJobResponse struct {
	ID          string `json:"id" example:"cj_abc123"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Schedule    struct {
		Kind    string `json:"kind"`
		At      string `json:"at,omitempty"`
		EveryMs int64  `json:"every_ms,omitempty"`
		Expr    string `json:"expr,omitempty"`
		TZ      string `json:"tz,omitempty"`
	} `json:"schedule"`
	Payload struct {
		Kind            string   `json:"kind"`
		Message         string   `json:"message"`
		TargetSessionID string   `json:"target_session_id,omitempty"`
		AllowedTools    []string `json:"allowed_tools,omitempty"`
		WorkerType      string   `json:"worker_type,omitempty"`
	} `json:"payload"`
	WorkDir        string            `json:"work_dir,omitempty"`
	BotID          string            `json:"bot_id,omitempty"`
	OwnerID        string            `json:"owner_id,omitempty"`
	Platform       string            `json:"platform,omitempty"`
	PlatformKey    map[string]string `json:"platform_key,omitempty"`
	TimeoutSec     int               `json:"timeout_sec,omitempty"`
	DeleteAfterRun bool              `json:"delete_after_run,omitempty"`
	Silent         bool              `json:"silent,omitempty"`
	MaxRetries     int               `json:"max_retries,omitempty"`
	MaxRuns        int               `json:"max_runs,omitempty"`
	ExpiresAt      string            `json:"expires_at,omitempty"`
	State          struct {
		NextRunAtMs     int64  `json:"next_run_at_ms"`
		LastRunAtMs     int64  `json:"last_run_at_ms"`
		RunningAtMs     int64  `json:"running_at_ms"`
		LastStatus      string `json:"last_status,omitempty"`
		ConsecutiveErrs int    `json:"consecutive_errors"`
		RunCount        int    `json:"run_count,omitempty"`
	} `json:"state"`
	CreatedAtMs int64 `json:"created_at_ms"`
	UpdatedAtMs int64 `json:"updated_at_ms"`
}

// SessionDetailResponse is returned by GET /admin/sessions/{id} and GET /api/sessions/{id}.
type SessionDetailResponse struct {
	ID              string            `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	UserID          string            `json:"user_id"`
	OwnerID         string            `json:"owner_id,omitempty"`
	BotID           string            `json:"bot_id,omitempty"`
	WorkerType      string            `json:"worker_type"`
	State           string            `json:"state" example:"running"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at"`
	ExpiresAt       string            `json:"expires_at,omitempty"`
	IdleExpiresAt   string            `json:"idle_expires_at,omitempty"`
	Context         map[string]any    `json:"context,omitempty"`
	WorkerSessionID string            `json:"worker_session_id,omitempty"`
	AllowedTools    []string          `json:"allowed_tools,omitempty"`
	Platform        string            `json:"platform,omitempty"`
	PlatformKey     map[string]string `json:"platform_key,omitempty"`
	WorkDir         string            `json:"work_dir,omitempty"`
	Title           string            `json:"title,omitempty"`
	Source          string            `json:"source,omitempty"`
	ClientKey       string            `json:"client_key,omitempty"`
}

// SessionStatsResponse is returned by GET /admin/sessions/{id}/stats.
type SessionStatsResponse struct {
	SessionID          string             `json:"session_id"`
	Generation         int64              `json:"generation"`
	TotalTurns         int                `json:"total_turns"`
	SuccessTurns       int                `json:"success_turns"`
	FailedTurns        int                `json:"failed_turns"`
	TotalDurMs         int64              `json:"total_duration_ms"`
	TotalCostUSD       float64            `json:"total_cost_usd"`
	TotalTokIn         int64              `json:"total_tokens_in"`
	TotalTokInput      int64              `json:"total_tokens_input"`
	TotalTokCacheWrite int64              `json:"total_tokens_cache_write"`
	TotalTokCacheRead  int64              `json:"total_tokens_cache_read"`
	TotalTokOut        int64              `json:"total_tokens_out"`
	Turns              []SessionStatsItem `json:"turns"`
}

// SessionStatsItem is a single turn in SessionStatsResponse.
type SessionStatsItem struct {
	TurnNum          int     `json:"turn_num"`
	Seq              int64   `json:"seq"`
	Success          bool    `json:"success"`
	DurationMs       int64   `json:"duration_ms"`
	CostUSD          float64 `json:"cost_usd"`
	TokensIn         int64   `json:"tokens_in"`
	TokensInput      int64   `json:"tokens_input"`
	TokensCacheWrite int64   `json:"tokens_cache_write"`
	TokensCacheRead  int64   `json:"tokens_cache_read"`
	TokensOut        int64   `json:"tokens_out"`
	Model            string  `json:"model"`
	Source           string  `json:"source"`
	CreatedAt        int64   `json:"created_at"`
}
