package effect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for lookups. Callers distinguish "absent" from "broken"
// with errors.Is rather than string matching.
var (
	// ErrEffectNotFound means no effect carries that business key.
	ErrEffectNotFound = errors.New("effect: not found")
	// ErrPayloadNotFound means the content snapshot is gone. It is reported
	// separately from ErrPayloadEffectMismatch because it usually means the
	// content retention window closed, not that the ledger disagrees.
	ErrPayloadNotFound = errors.New("effect: payload content unavailable")
)

// Store is the durable home of delivery intents. The read methods exist so
// recovery can converge on the effect a previous process already committed
// instead of planning a second one for the same delivery.
type Store interface {
	// PlanOnce records the content snapshot and the planned effect in ONE
	// transaction and returns the stored effect. created=false means the
	// business key was already taken, which is how a retry or a post-crash
	// recovery converges rather than double-delivering.
	PlanOnce(ctx context.Context, plan Plan, now time.Time) (stored *Effect, created bool, err error)
	// GetByKey returns the effect with the given business key.
	GetByKey(ctx context.Context, occurrenceID string, deliveryOrdinal int64, targetRevision string) (*Effect, error)
	// GetByID returns the effect by its surrogate ID.
	GetByID(ctx context.Context, effectID string) (*Effect, error)
	// GetPayload returns a content snapshot by ID.
	GetPayload(ctx context.Context, payloadID string) (*Payload, error)
	// GetPayloadForExecution returns the snapshot committed for one execution.
	// It is the recovery path: an assistant terminal state may be persisted
	// while the effect intent was not, and the text to send is recovered from
	// here rather than by running the Agent again.
	GetPayloadForExecution(ctx context.Context, occurrenceID, executionID string) (*Payload, error)
	// ClaimSend makes one caller the owner of the next send attempt. Exactly
	// one caller can win it, which is what stops two instances recovering the
	// same occurrence from both sending.
	ClaimSend(ctx context.Context, req ClaimRequest) (*Claim, error)
	// CompleteSend records what an attempt established and moves the effect to
	// the status that outcome implies. A caller that no longer owns the
	// attempt gets ErrLeaseLost and must re-read.
	CompleteSend(ctx context.Context, c Completion) error
	// ClaimRetry opens the next attempt after a rejection the provider
	// explicitly declared safe to repeat. It refuses when the backoff has not
	// elapsed, when no safe rejection was recorded, or at the attempt cap.
	ClaimRetry(ctx context.Context, req RetryRequest) (*Claim, error)
	// ExpireLeases moves sends whose lease elapsed into unknown. A lapsed
	// lease is not proof that nothing was sent, so it never returns an effect
	// to a sendable state.
	ExpireLeases(ctx context.Context, now time.Time, limit int) ([]*Effect, error)
	// ListRecoverable returns effects that are durably owed a send and are not
	// currently owned by anyone.
	ListRecoverable(ctx context.Context, limit int) ([]*Effect, error)
	// ListDueRetries returns effects waiting behind a backoff whose next
	// attempt may now open, excluding any that have reached the attempt cap.
	ListDueRetries(ctx context.Context, now time.Time, maxAttempts int64, limit int) ([]*Effect, error)
	// ListAttempts returns the per-attempt facts for an effect, oldest first.
	ListAttempts(ctx context.Context, effectID string) ([]*Attempt, error)
	// ListForOperator returns effects for the operator console, newest first.
	// Content is never included.
	ListForOperator(ctx context.Context, f OperatorListFilter) ([]*Effect, error)
	// ApplyOperatorAction records one conditional operator decision against an
	// effect that is still unknown. It conflicts rather than overwrites when
	// the effect moved in the meantime.
	ApplyOperatorAction(ctx context.Context, req OperatorActionRequest) (*Effect, error)
}

const effectColumns = `effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		session_id, execution_id, worker_run_id, payload_id, payload_sha256,
		target_kind, target_ref, status, error_code, reason,
		owner_instance_id, lease_until, lease_version, provider_ref, evidence_ref,
		created_at, updated_at`

const payloadColumns = `payload_id, occurrence_id, execution_id, worker_run_id,
		content, content_bytes, content_sha256, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEffect(row rowScanner) (*Effect, error) {
	var (
		e         Effect
		leaseTill sql.NullInt64
	)
	err := row.Scan(
		&e.EffectID, &e.OccurrenceID, &e.DeliveryOrdinal, &e.TargetRevision, &e.Attempt,
		&e.SessionID, &e.ExecutionID, &e.WorkerRunID, &e.PayloadID, &e.PayloadSHA256,
		&e.TargetKind, &e.TargetRef, &e.Status, &e.ErrorCode, &e.Reason,
		&e.OwnerInstanceID, &leaseTill, &e.LeaseVersion, &e.ProviderRef, &e.EvidenceRef,
		&e.CreatedAtMs, &e.UpdatedAtMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEffectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("effect: scan: %w", err)
	}
	if leaseTill.Valid {
		v := leaseTill.Int64
		e.LeaseUntilMs = &v
	}
	return &e, nil
}

func scanPayload(row rowScanner) (*Payload, error) {
	var p Payload
	err := row.Scan(
		&p.PayloadID, &p.OccurrenceID, &p.ExecutionID, &p.WorkerRunID,
		&p.Content, &p.ContentBytes, &p.ContentSHA, &p.CreatedAtMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPayloadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("effect: scan payload: %w", err)
	}
	return &p, nil
}
