package session

import (
	"context"
	"errors"
	"time"
)

var (
	ErrPurgeNotFound       = errors.New("session purge job not found")
	ErrPurgeStatusNotReady = errors.New("session purge status unavailable")
)

type PurgeJobState string

const (
	PurgeJobPending    PurgeJobState = "pending"
	PurgeJobInProgress PurgeJobState = "in_progress"
	PurgeJobBlocked    PurgeJobState = "blocked"
	PurgeJobComplete   PurgeJobState = "complete"
)

type PurgeItemState string

const (
	PurgeItemPending     PurgeItemState = "pending"
	PurgeItemRunning     PurgeItemState = "running"
	PurgeItemRetrying    PurgeItemState = "retrying"
	PurgeItemBlocked     PurgeItemState = "blocked"
	PurgeItemUnsupported PurgeItemState = "unsupported"
	PurgeItemComplete    PurgeItemState = "complete"
)

const (
	PurgeItemSessionMetadata     = "session_metadata"
	PurgeItemConversationContent = "conversation_content"
	PurgeItemWorkerSession       = "worker_session"
)

// PurgeItemStatus contains only bounded, non-content cleanup state. ErrorCode
// is an internal enum; raw provider or worker errors are never exposed.
type PurgeItemStatus struct {
	Kind          string         `json:"kind"`
	Status        PurgeItemState `json:"status"`
	Attempts      int            `json:"attempts"`
	NextAttemptAt *time.Time     `json:"next_attempt_at,omitempty"`
	CompletedAt   *time.Time     `json:"completed_at,omitempty"`
	ErrorCode     string         `json:"error_code,omitempty"`
}

// PurgeStatus is the user-visible progress snapshot for one deletion intent.
// WorkspaceID is retained only for the authorization check and never serialized.
type PurgeStatus struct {
	JobID       string            `json:"job_id"`
	SessionID   string            `json:"session_id"`
	Status      PurgeJobState     `json:"status"`
	RequestedAt time.Time         `json:"requested_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Items       []PurgeItemStatus `json:"items"`
	WorkspaceID string            `json:"-"`
}

// PurgeStatusReader reads a deletion job through its immutable ownership
// snapshot, so status remains queryable after the session itself is hidden.
type PurgeStatusReader interface {
	GetPurgeStatus(ctx context.Context, sessionID, userID string) (*PurgeStatus, error)
}

// PurgeItemTask is a lease-fenced unit of durable session-content cleanup.
type PurgeItemTask struct {
	ID            string
	SessionID     string
	Kind          string
	Attempts      int
	NextAttemptAt time.Time
	LeaseUntil    *time.Time
	LeaseToken    string
}

// PurgeItemStore leases and records completion of durable content purge work.
type PurgeItemStore interface {
	ClaimPurgeItems(ctx context.Context, now, leaseUntil time.Time, limit int) ([]PurgeItemTask, error)
	CompletePurgeItem(ctx context.Context, itemID, leaseToken string) error
	RetryPurgeItem(ctx context.Context, itemID, leaseToken string, nextAttemptAt time.Time, errorCode string) error
}
