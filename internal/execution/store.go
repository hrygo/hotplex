// Package execution persists the ingress state of user inputs before they are
// delivered to a worker. It intentionally stores only a payload hash, never the
// user-provided content itself.
package execution

import (
	"context"
	"errors"
	"time"
)

// Status is the durable delivery state of an input execution.
type Status string

const (
	StatusAccepted  Status = "accepted"
	StatusDelivered Status = "delivered"
	StatusUnknown   Status = "unknown"
	StatusFailed    Status = "failed"
)

// RuntimeStatus is the execution fact of a Worker turn, independent of delivery
// status. It allows knowledge refinement: unknown → completed/failed is allowed
// when a late Done converges, but terminals never regress.
type RuntimeStatus string

const (
	// RuntimeQueued is accepted, durable, and NOT yet handed to a worker. It
	// holds no owner lease and occupies no active slot, so a session with a
	// backlog still presents exactly one active execution to the dispatcher.
	// Recovery treats it as safely resumable; pending/running never are,
	// because those already crossed the dispatch boundary where a lost
	// response becomes unknown plus a fence.
	RuntimeQueued    RuntimeStatus = "queued"
	RuntimePending   RuntimeStatus = "pending"
	RuntimeRunning   RuntimeStatus = "running"
	RuntimeCompleted RuntimeStatus = "completed"
	RuntimeFailed    RuntimeStatus = "failed"
	RuntimeUnknown   RuntimeStatus = "unknown"
)

// LeaseTTL is the default lease time-to-live for an active execution.
const LeaseTTL = 60 // seconds

// RuntimeErrorCodeOperatorAbandoned is the bounded runtime error code written
// when an operator abandons a fenced execution (#877). It appears in the
// execution store, the runtime.execution.failed AEP event, and metrics only —
// never alongside prompt content or raw worker errors.
const RuntimeErrorCodeOperatorAbandoned = "OPERATOR_ABANDONED"

// FenceDecision is the operator action applied to a fenced execution (#877).
// Both decisions clear the fence; neither marks the execution delivered or
// completed and neither re-dispatches the input.
type FenceDecision string

const (
	// FenceDecisionResolve clears the fence and keeps runtime_status=unknown.
	// The operator determined the ambiguity is harmless; a fresh input can
	// proceed on a new worker.
	FenceDecisionResolve FenceDecision = "resolve"
	// FenceDecisionAbandon clears the fence and terminates the runtime as
	// failed with RuntimeErrorCodeOperatorAbandoned.
	FenceDecisionAbandon FenceDecision = "abandon"
)

// FenceActionRequest is the conditional operator action on a fenced execution.
// ExpectedFenceVersion is the fencing token read by the operator; the update
// only applies when it still matches (optimistic concurrency across gateway
// instances). Actor, reason, and evidence stay in the Admin layer — the store
// persists no operator-supplied free text.
type FenceActionRequest struct {
	ExecutionID          string
	ExpectedFenceVersion int64
	Decision             FenceDecision
}

var (
	// ErrNotFound means the execution does not exist.
	ErrNotFound = errors.New("execution: record not found")
	// ErrPayloadConflict means a client message ID was reused with different data.
	ErrPayloadConflict = errors.New("execution: client message id reused with different payload")
	// ErrSessionBusy means the session already has an active (pending/running or
	// fenced) execution and cannot accept a new one until the current one
	// terminates or the fence is cleared.
	ErrSessionBusy = errors.New("execution: session has an active execution")
	// ErrExecutionFenced means the session has a fence_reason set and the next
	// input requires a fresh Worker session before it can be accepted.
	ErrExecutionFenced = errors.New("execution: session is fenced, fresh worker required")
	// ErrOwnerMismatch means a conditional update did not match the expected owner.
	ErrOwnerMismatch = errors.New("execution: owner mismatch")
	// ErrLeaseExpired means the execution's lease has expired.
	ErrLeaseExpired = errors.New("execution: lease expired")
	// ErrRunMismatch means a conditional update did not match the expected worker_run_id.
	ErrRunMismatch = errors.New("execution: worker run mismatch")
	// ErrFenceConflict means a conditional fence action did not match: the
	// record is missing, no longer fenced, or its fence_version moved on
	// (another operator or gateway instance acted first). The caller must
	// re-inspect; the store never auto-retries.
	ErrFenceConflict = errors.New("execution: fence version conflict")
	// ErrQueueFull means a bounded queue limit would be exceeded. The store
	// refuses the input instead of evicting something already accepted:
	// dropping an acknowledged input to make room for a new one turns a
	// durable promise into a silent loss.
	ErrQueueFull = errors.New("execution: input queue is full")
	// ErrQueuePayloadTooLarge means one payload exceeds the per-item byte
	// bound. It is reported separately from ErrQueueFull because the fix is to
	// send less, not to retry later.
	ErrQueuePayloadTooLarge = errors.New("execution: queued payload exceeds the size limit")
	// ErrQueueNotQueued means the execution is not an undispatched queue item,
	// so a queued cancel cannot apply. It is a conflict, not a failure: the
	// input already crossed the dispatch boundary and is governed by the
	// unknown / fence rules instead.
	ErrQueueNotQueued = errors.New("execution: execution is not queued")
	// ErrQueueLifecycleStale means the queue entry belongs to an older session
	// lifecycle than the dispatcher holds. Dispatching it would resurrect a
	// turn from before a /reset or a delete.
	ErrQueueLifecycleStale = errors.New("execution: queue entry is from an older session lifecycle")
	// ErrQueueHeadMoved means another dispatcher claimed or settled the queue
	// head between the caller reading it and claiming it. The caller must
	// re-read and re-validate rather than dispatching an item it never checked.
	ErrQueueHeadMoved = errors.New("execution: queue head changed since it was read")
)

// Record is the secret-free durable representation of an input delivery.
type Record struct {
	ExecutionID     string
	SessionID       string
	ClientMessageID string
	PayloadHash     string
	Status          Status
	ErrorCode       string
	CreatedAt       int64
	UpdatedAt       int64
	DeliveredAt     *int64
	// --- Durable ingress reliability closure fields (migration 027) ---
	OwnerInstanceID  string
	WorkerRunID      string
	LeaseUntil       int64
	RuntimeStatus    RuntimeStatus
	RuntimeErrorCode string
	StartedAt        *int64
	FinishedAt       *int64
	FenceReason      string
	// --- Operator fence action fields (migration 031) ---
	// FenceVersion is the fencing token: incremented each time the execution
	// enters the fenced state from a non-fenced state.
	FenceVersion int64
	// FenceCreatedAt is the millisecond timestamp at which the current fence
	// was raised; nil when the execution was never fenced.
	FenceCreatedAt *int64
}

// AcceptRequest describes an input that must be durably accepted before dispatch.
type AcceptRequest struct {
	SessionID       string
	ClientMessageID string
	PayloadHash     string
	// OwnerInstanceID is the Gateway process that owns this execution's lease.
	OwnerInstanceID string
	// WorkerRunID is the Bridge worker run that will dispatch this input.
	WorkerRunID string
}

type LeaseRecoveryResult struct {
	Recovered             int64
	ConvergedExecutionIDs []string
}

// Store is the persistence contract for execution ingress records.
type Store interface {
	// Accept creates a durable accepted record with owner, lease, and runtime
	// status set to pending. When the session/message key already exists it
	// returns that record with duplicate=true. Reusing the key with a different
	// payload returns ErrPayloadConflict.
	Accept(ctx context.Context, request AcceptRequest) (record *Record, duplicate bool, err error)

	// SetStatus advances an accepted record to a delivery outcome. Repeating the
	// same update is idempotent; terminal outcomes never regress (sole exception:
	// ConvergeDeliveryFailed rewrites failed → unknown when the runtime already
	// completed for the same worker run).
	//
	// Deprecated: use SetDelivery for owner-conditioned updates.
	SetStatus(ctx context.Context, executionID string, status Status, errorCode string) error

	// SetDelivery advances the delivery status of an execution owned by ownerID.
	// The update is conditional on the current owner; mismatch returns
	// ErrOwnerMismatch. Terminal delivery statuses never regress (sole exception:
	// ConvergeDeliveryFailed rewrites failed → unknown when the runtime already
	// completed for the same worker run).
	SetDelivery(ctx context.Context, executionID, ownerID string, status Status, errorCode string) error

	// MarkRunning transitions runtime_status from pending to running and records
	// the worker_run_id. Called after active gate passes, before Worker.Input.
	MarkRunning(ctx context.Context, executionID, ownerID, workerRunID string) error

	// FinishRuntime sets the terminal runtime status (completed/failed) for an
	// execution matching executionID and workerRunID. Sets finished_at, releases
	// the active gate, and stops lease renewal for this execution. Late Done
	// events with matching workerRunID can refine unknown → completed/failed.
	FinishRuntime(ctx context.Context, executionID, workerRunID string, status RuntimeStatus, errorCode string) error

	// ConvergeDeliveryFailed rewrites a failed delivery to unknown when the
	// runtime completed for the same worker run (single explicit exception to
	// terminal-never-regresses; unknown never triggers redelivery).
	ConvergeDeliveryFailed(ctx context.Context, executionID, workerRunID string) error

	// ActiveBySession returns the current pending/running execution for the
	// session, or ErrNotFound if none exists. Used by the active gate check.
	ActiveBySession(ctx context.Context, sessionID string) (*Record, error)

	// OpenBySession returns the most recent non-terminal execution (pending,
	// running, or unknown) for the session, or ErrNotFound if none exists.
	// Used by the Done-event convergence path to finish a runtime that lease
	// recovery may have already marked unknown (unknown -> completed/failed).
	OpenBySession(ctx context.Context, sessionID string) (*Record, error)

	// LatestBySession returns the most recent execution for the session
	// regardless of runtime status, or ErrNotFound if none exists. Used by the
	// stop path to identify the turn a stop belongs to: stable across the
	// stop's own runtime closure, changes only when a new turn accepts a new
	// execution.
	LatestBySession(ctx context.Context, sessionID string) (*Record, error)

	// FenceBySession returns the fenced execution for the session (if any), or
	// ErrNotFound. A fenced session requires fresh Worker before new input.
	FenceBySession(ctx context.Context, sessionID string) (*Record, error)

	// ClearFenceAfterFreshStart atomically clears the fence_reason and sets a new
	// worker_run_id, only if the current fence_reason matches the given reason.
	// This is the final step of the fresh-Worker flow.
	ClearFenceAfterFreshStart(ctx context.Context, executionID, reason, freshWorkerRunID string) error

	// ApplyFenceDecision applies an operator fence action (resolve/abandon) to a
	// fenced execution, conditional on ExpectedFenceVersion still matching
	// (#877). Returns the updated record. Neither decision marks the execution
	// delivered/completed or re-dispatches it. Missing record returns
	// ErrNotFound; a moved/cleared fence returns ErrFenceConflict.
	ApplyFenceDecision(ctx context.Context, request FenceActionRequest) (*Record, error)

	// ListFences returns currently fenced executions ordered by fence_created_at
	// (newest first), optionally filtered by session. limit<=0 uses the store
	// default; offset supports pagination.
	ListFences(ctx context.Context, sessionID string, limit, offset int) ([]*Record, error)

	// RenewLeases batch-renews all pending/running executions owned by ownerID,
	// extending lease_until to now + ttl. Returns the number of renewed records.
	// No-op when the owner has no active executions.
	RenewLeases(ctx context.Context, ownerID string, ttlSeconds int64, excludeExecutionIDs []string) (int64, error)

	// RecoverExpiredLeases recovers executions whose lease has expired, setting
	// runtime_status to unknown and fence_reason. trackedExecutionIDs are checked
	// after recovery; IDs no longer active are returned so renewal exclusions can
	// be released even when another gateway won the recovery race.
	RecoverExpiredLeases(ctx context.Context, trackedExecutionIDs []string) (LeaseRecoveryResult, error)

	// TerminateOwnerLeases marks all active (pending/running) executions owned by
	// ownerID as unknown with a fence_reason. Used during graceful shutdown.
	TerminateOwnerLeases(ctx context.Context, ownerID, reason string) (int64, error)

	// AcceptQueued durably accepts an input into the bounded persistent queue
	// with runtime status queued. The canonical execution row, the queue row
	// and every capacity check commit in one transaction, so a returned record
	// is already durable and already known to be within bounds.
	//
	// The same (session_id, client_message_id) key with the same payload hash
	// returns the existing record with duplicate=true; with a different hash it
	// returns ErrPayloadConflict. Capacity pressure returns ErrQueueFull rather
	// than evicting anything.
	AcceptQueued(ctx context.Context, request QueuedRequest, limits QueueLimits) (
		record *Record, entry *QueueEntry, duplicate bool, err error)

	// QueueByExecution returns the scheduling state of one queued input, or
	// ErrNotFound when that execution is not queued (including after it has
	// been dispatched, cancelled or expired).
	QueueByExecution(ctx context.Context, executionID string) (*QueueEntry, error)

	// QueueBySession returns a session's undispatched inputs in dispatch order.
	// limit<=0 uses the store default.
	QueueBySession(ctx context.Context, sessionID string, limit int) ([]*QueueEntry, error)

	// QueueDepth returns the number of undispatched inputs across the instance.
	// It is read from the queue itself rather than from a maintained counter,
	// so no delete path can leak capacity by forgetting to report.
	QueueDepth(ctx context.Context) (int64, error)

	// ClaimQueued promotes the session's queue head to pending and takes the
	// owner lease, in one transaction. That transition is the dispatch
	// boundary: past it, a lost response is unknown plus a fence, never a
	// silent resend.
	//
	// ErrNotFound means the queue is empty, ErrSessionBusy means the single
	// active slot is taken (the item stays queued for a later attempt), and
	// ErrQueueLifecycleStale means the item belongs to a superseded lifecycle.
	ClaimQueued(ctx context.Context, request ClaimQueuedRequest) (*Record, *QueueEntry, error)

	// CancelQueued settles one undispatched input as failed with the given
	// bounded reason (QUEUE_CANCELLED, QUEUE_EXPIRED, ...). It refuses with
	// ErrQueueNotQueued once the item has been dispatched: pretending a running
	// input never ran would be a lie, so the caller must use the ordinary stop
	// path instead.
	CancelQueued(ctx context.Context, executionID, reason string) (*Record, error)

	// ClearQueue settles every undispatched input for a session. It is what
	// /reset and session delete call: a queue belonging to a turn the user
	// abandoned must not dispatch afterwards. Returns the number settled.
	ClearQueue(ctx context.Context, sessionID, reason string) (int64, error)

	// ExpireQueued settles undispatched inputs whose TTL has elapsed, oldest
	// first, up to limit. A queued input that waited longer than its bound is
	// no longer what the user asked for, so it is settled rather than sent.
	ExpireQueued(ctx context.Context, now time.Time, limit int) ([]*Record, error)

	// QueueDepthBySession returns the number of undispatched inputs for one
	// session.
	QueueDepthBySession(ctx context.Context, sessionID string) (int64, error)
}
