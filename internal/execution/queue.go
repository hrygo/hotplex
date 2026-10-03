package execution

import "time"

// Queue defaults. These are the proposed first-slice bounds from the plan; each
// is independently configurable and every one of them is checked inside the
// accepting transaction, so an over-limit input is refused rather than
// admitted and silently trimmed.
const (
	// DefaultQueuePerSession matches the pre-existing in-memory supplement
	// buffer bound, so the durable queue is not a sudden loosening of the
	// limit users already live with.
	DefaultQueuePerSession = 20
	// DefaultQueueGlobal bounds the whole instance so one busy session cannot
	// consume the queue every other session shares.
	DefaultQueueGlobal = 1000
	// DefaultQueueMaxPayloadBytes bounds a single queued payload. The queue row
	// itself never stores content; this bounds the content the reference
	// points at, which is what actually costs storage.
	DefaultQueueMaxPayloadBytes = 64 << 10
	// DefaultQueueTTL bounds how long an undispatched item may sit before it
	// is settled as expired. A queued input that waited a day is no longer the
	// thing the user asked for.
	DefaultQueueTTL = 24 * time.Hour
)

// QueueLimits bounds the persistent input queue. Zero and negative fields fall
// back to the defaults above rather than disabling a limit: an unset bound must
// not silently become an unbounded queue.
type QueueLimits struct {
	// PerSession caps undispatched inputs for one session.
	PerSession int
	// Global caps undispatched inputs across the instance.
	Global int
	// MaxPayloadBytes caps one queued payload's content size.
	MaxPayloadBytes int
	// TTL is how long an undispatched input stays dispatchable.
	TTL time.Duration
}

// DefaultQueueLimits returns the bounds used when nothing is configured.
func DefaultQueueLimits() QueueLimits {
	return QueueLimits{
		PerSession:      DefaultQueuePerSession,
		Global:          DefaultQueueGlobal,
		MaxPayloadBytes: DefaultQueueMaxPayloadBytes,
		TTL:             DefaultQueueTTL,
	}
}

func (l QueueLimits) withDefaults() QueueLimits {
	if l.PerSession <= 0 {
		l.PerSession = DefaultQueuePerSession
	}
	if l.Global <= 0 {
		l.Global = DefaultQueueGlobal
	}
	if l.MaxPayloadBytes <= 0 {
		l.MaxPayloadBytes = DefaultQueueMaxPayloadBytes
	}
	if l.TTL <= 0 {
		l.TTL = DefaultQueueTTL
	}
	return l
}

// QueuedRequest is an input accepted into the durable queue instead of being
// dispatched immediately.
//
// It carries no prompt, metadata or credential: the execution row keeps only a
// payload hash, and PayloadRef points at content held by the content domain
// behind its own ACL and retention policy.
type QueuedRequest struct {
	SessionID       string
	ClientMessageID string
	PayloadHash     string
	// PayloadRef is the opaque content reference a dispatcher will read.
	PayloadRef string
	// PayloadBytes is the size of that content, checked against the queue's
	// per-item bound before anything is written.
	PayloadBytes int
	// OwnerInstanceID records which gateway accepted the input. A queued input
	// holds no lease and is not owned by it until dispatch claims it, so this
	// is provenance, not ownership.
	OwnerInstanceID string
	// LifecycleRevision is the session lifecycle generation this input belongs
	// to. A dispatcher holding an older revision must refuse the item instead
	// of resurrecting a previous turn.
	LifecycleRevision int64
}

// QueueEntry is the scheduling state of one durably accepted, undispatched
// input. Delivery status, runtime status and input idempotency deliberately
// stay on the execution row: two copies of the truth would drift.
type QueueEntry struct {
	ExecutionID string
	SessionID   string
	// QueueSeq is assigned by the database, never by a client clock, so two
	// gateways cannot disagree about dispatch order.
	QueueSeq          int64
	EnqueuedAt        int64
	ExpiresAt         int64
	LifecycleRevision int64
	PayloadRef        string
	PayloadBytes      int64
}

// Bounded reasons a queued input can be settled without ever reaching a
// worker. They are deliberately distinct from any runtime failure code: an
// input that was cancelled or expired never ran, and a console that reports it
// as "the worker failed" is describing an event that did not happen.
const (
	QueueReasonCancelled = "QUEUE_CANCELLED"
	QueueReasonExpired   = "QUEUE_EXPIRED"
)

// ClaimQueuedRequest promotes one queued input to the dispatch boundary.
type ClaimQueuedRequest struct {
	SessionID       string
	OwnerInstanceID string
	// WorkerRunID is the run that will carry the input. It is recorded at the
	// same moment as the pending transition so a terminal event can be
	// correlated to the run that produced it.
	WorkerRunID string
	// LifecycleRevision, when positive, must equal the queue entry's revision.
	// Zero skips the check for callers that already validated it.
	LifecycleRevision int64
	// ExpectedExecutionID, when set, must still be the queue head. A
	// dispatcher validates an item BEFORE claiming it (permission, skill
	// capability, session ownership); without this the atomic claim could
	// promote a different item than the one that was actually checked.
	ExpectedExecutionID string
	// LeaseTTLSeconds overrides LeaseTTL when positive.
	LeaseTTLSeconds int64
}

// IsQueued reports whether a record is durably accepted and awaiting dispatch.
// It is deliberately not "not yet running": an accepted record that already
// crossed the dispatch boundary is pending and is governed by the unknown /
// fence rules instead.
func (r *Record) IsQueued() bool {
	return r != nil && r.RuntimeStatus == RuntimeQueued && r.Status == StatusAccepted
}
