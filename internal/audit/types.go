// Package audit implements the User Behavior Audit System (issue #833, spec §5).
// It is an independent, append-only audit log with sha256 hash chain integrity,
// zero-loss collector with WAL spill, and pluggable AlertSink fan-out.
package audit

import (
	"context"
	"time"
)

// Action constants for audit events. See spec §5.2.
const (
	ActionAuthLogin                = "auth.login"
	ActionAuthLogout               = "auth.logout"
	ActionAuthTokenValidated       = "auth.token_validated"
	ActionAuthAPIKeyUsed           = "auth.apikey_used"
	ActionAuthDenied               = "auth.denied"
	ActionSessionCreate            = "session.create"
	ActionSessionTerminate         = "session.terminate"
	ActionSessionDelete            = "session.delete"
	ActionMessageInbound           = "message.inbound"
	ActionToolCall                 = "tool.call" // P2 - reserved
	ActionPermissionRequest        = "permission.request"
	ActionPermissionResponse       = "permission.response"
	ActionQuestionResponse         = "question.response"
	ActionElicitationResponse      = "elicitation.response"
	ActionSystemAuditConfigChanged = "system.audit_config_changed"
	ActionSystemAuditExport        = "system.audit_export"
)

// Outcome constants. See spec §5.1.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
)

// UserIDType constants. See spec §5.4.
const (
	UserIDTypePlatform   = "platform"   // feishu open_id, slack user_id
	UserIDTypeRegistered = "registered" // webchat UUID
	UserIDTypeAnonymous  = "anonymous"  // unauthenticated attacker
	UserIDTypeSystem     = "system"     // cronjob, system process
)

// Platform constants. See spec §5.4.
const (
	PlatformWebChat = "webchat"
	PlatformFeishu  = "feishu"
	PlatformSlack   = "slack"
	PlatformCron    = "cron"
	PlatformAdmin   = "admin"
	PlatformAPI     = "api"
)

// AnonymousUserID is the sentinel used for unauthenticated events.
// Per spec §5.4: ip + user_agent MUST be populated alongside this.
const AnonymousUserID = "anonymous"

// UserActivity is the row stored in the user_activity table.
// Mirrors the 16-column schema in spec §5.1.
type UserActivity struct {
	ID           int64  `json:"id"`                    // assigned by DB
	ChainEpoch   string `json:"chain_epoch,omitempty"` // legacy or lifecycle-v2 chain
	Ts           int64  `json:"ts"`                    // Unix ms
	ExpiresAt    int64  `json:"expires_at,omitempty"`  // persisted lifecycle-v2 retention deadline, Unix ms
	UserID       string `json:"user_id"`               // NOT NULL
	UserIDType   string `json:"user_id_type"`          // NOT NULL
	Platform     string `json:"platform"`              // NOT NULL
	SessionID    string `json:"session_id"`            // empty for admin/api
	Action       string `json:"action"`                // NOT NULL
	ResourceType string `json:"resource_type"`         // optional
	ResourceID   string `json:"resource_id"`           // optional
	Outcome      string `json:"outcome"`               // NOT NULL: success/failure/denied
	DetailJSON   string `json:"detail_json"`           // NOT NULL: whitelisted fields per §5.9
	EventRef     string `json:"event_ref"`             // optional: events.id or turns.id
	IP           string `json:"ip"`                    // optional
	UserAgent    string `json:"user_agent"`            // optional
	PrevHash     string `json:"prev_hash"`             // NOT NULL: "" for genesis
	SelfHash     string `json:"self_hash"`             // NOT NULL: sha256(PrevHash || canonical(rest))
}

// IdentityLink maps one immutable audit subject (provider + subject) to a
// canonical principal user. Audit rows are never rewritten; queries expand a
// principal into these linked native IDs.
type IdentityLink struct {
	ID              string `json:"id"`
	PrincipalUserID string `json:"principal_user_id"`
	Provider        string `json:"provider"`
	Subject         string `json:"subject"`
	SubjectType     string `json:"subject_type"`
	DisplayName     string `json:"display_name"`
	Email           string `json:"email"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// Checkpoint is the rebase anchor written before pruning.
// See spec §5.5.
type Checkpoint struct {
	ID           int64
	PrunedAt     time.Time
	LastSelfHash string
	NextID       int64 // id of the first surviving row
}

// AuditEvent is the read-only snapshot passed to AlertSink.
// See spec §5.6.
type AuditEvent struct {
	EventID      string // UUIDv7 - stable, sink dedup
	Ts           time.Time
	UserID       string
	UserIDType   string
	Platform     string
	SessionID    string
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      string
	Detail       map[string]any
	EventRef     string
	IP           string
	UserAgent    string
}

// AlertSink is the extension point for real-time alerting. See spec §5.6.
// Implementations must be non-blocking (internal async) — failures must NOT
// affect the main audit write path.
type AlertSink interface {
	OnAuditEvent(ctx context.Context, e AuditEvent) error
}

// AlertSinkCloser is an optional lifecycle contract. The collector invokes it
// after the sink worker has drained its bounded queue.
type AlertSinkCloser interface {
	Close(ctx context.Context) error
}
