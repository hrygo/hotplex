package messaging

import (
	"context"
	"time"
)

// SendOutcome is what a provider actually confirmed about one send attempt.
//
// It is deliberately not a success/failure pair. "The request failed" and
// "we do not know whether it was delivered" are different facts, and only the
// first is safe to retry.
type SendOutcome string

const (
	// SendAccepted means the provider returned a receipt for this message.
	// It does NOT mean a human read it.
	SendAccepted SendOutcome = "accepted"
	// SendRejected means the provider refused the message and the refusal
	// says the message was not created.
	SendRejected SendOutcome = "rejected"
	// SendUnknown means the outcome could not be determined — a timeout, a
	// lost response, or a 5xx that does not prove non-commit. The message may
	// or may not exist. This state must never be treated as safe to resend.
	SendUnknown SendOutcome = "unknown"
)

// RejectionClass refines a SendRejected outcome.
type RejectionClass string

const (
	// RejectionUnspecified means the provider refused the message without
	// saying whether a retry could succeed. Treated as permanent.
	RejectionUnspecified RejectionClass = "unspecified"
	// RejectionSafeRetry means the provider explicitly refused without
	// creating anything — a rate limit, for example — so another attempt on
	// the SAME effect is safe.
	RejectionSafeRetry RejectionClass = "safe_retry"
	// RejectionPermanent means retrying cannot help: bad credentials, a
	// missing channel, an invalid payload.
	RejectionPermanent RejectionClass = "permanent"
)

// SendResult is the typed outcome of one controlled send attempt.
//
// Reason is bounded and human-readable; it must never carry a raw provider
// response body, credentials or user content.
type SendResult struct {
	Outcome SendOutcome
	// ProviderRef identifies the message at the provider, e.g. a Slack
	// "channel:timestamp" pair. It is an opaque reference, not an API call
	// recipe: nothing in the system may assume it can be replayed.
	ProviderRef string
	// EvidenceRef points at stored evidence for this receipt, if any.
	EvidenceRef string
	// RejectionClass only carries meaning when Outcome is SendRejected.
	RejectionClass RejectionClass
	// RetryAfter is the provider's own backoff hint, when it gave one.
	RetryAfter time.Duration
	// Reason is a bounded explanation safe to log and show.
	Reason string
}

// Accepted reports whether the provider confirmed the message.
func (r SendResult) Accepted() bool { return r.Outcome == SendAccepted }

// ProviderCapabilities declares what a target adapter can actually promise.
//
// The gateway reads these before choosing a delivery owner. An adapter that
// cannot fill them in truthfully must report the zero value, which keeps the
// job on its existing legacy path instead of granting a guarantee it cannot
// keep.
type ProviderCapabilities struct {
	// Idempotency means the provider honours a caller-supplied idempotency
	// key and will not create a second message for the same key.
	Idempotency bool
	// Lookup means a sent message can be queried afterwards, which is what
	// makes an unknown outcome resolvable instead of merely recordable.
	Lookup bool
	// Receipt means the provider returns a reference for an accepted message.
	Receipt bool
	// IdempotencyWindow bounds how long Idempotency holds, when known.
	IdempotencyWindow time.Duration
}

// ControlledSender is the receipt-bearing delivery interface used by the
// gateway's effect path.
//
// The contract is two-layered on purpose:
//
//   - A non-nil error means the send never reached the provider (bad target
//     projection, unresolved credentials). Nothing was attempted, so the effect
//     stays safe to retry.
//   - A nil error with Outcome SendUnknown means the attempt left this process
//     without knowing whether the provider committed it.
//
// Implementations must not fold the second case into the first.
type ControlledSender interface {
	SendWithReceipt(ctx context.Context, text string, platformKey map[string]string) (SendResult, error)
	// DeliveryCapabilities reports what this adapter can promise today.
	DeliveryCapabilities() ProviderCapabilities
}
