// Package effect implements the durable ledger for external-delivery effects
// (sending a message to Slack, Feishu or a webhook).
//
// The central invariant is that an effect and its content snapshot become
// visible together or not at all: an effect must never be observable as
// planned while the text it would send is missing, and a send must never
// happen before the intent to send is durably recorded.
package effect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/eventstore"
)

// Status is the lifecycle of one delivery effect.
type Status string

const (
	// StatusPlanned means the intent and its content are durably recorded but
	// no send has been attempted. This is the only state a send may start from.
	StatusPlanned Status = "planned"
	// StatusStarted means a send is in flight under an owner lease.
	StatusStarted Status = "started"
	// StatusDelivered means the provider returned a receipt. It does NOT mean a
	// human read the message.
	StatusDelivered Status = "delivered"
	// StatusFailed means the effect terminally failed.
	StatusFailed Status = "failed"
	// StatusUnknown means the outcome could not be determined — typically a
	// lost response. It must never be treated as safe to resend.
	StatusUnknown Status = "unknown"
	// StatusReconciledSucceeded means late evidence proved the provider had
	// accepted the delivery. It is terminal and distinct from delivered:
	// delivered was seen on the attempt, reconciled_succeeded converged later.
	StatusReconciledSucceeded Status = "reconciled_succeeded"
	// StatusReconciledFailed means late evidence proved the provider never
	// committed. Terminal, distinct from failed for the same reason.
	StatusReconciledFailed Status = "reconciled_failed"
	// StatusFenced means an operator quarantined the effect. Terminal: a
	// fenced effect is never claimed, retried, reconciled or decided again.
	StatusFenced Status = "fenced"
)

// Sentinel errors returned by the planner. Callers classify a refusal with
// errors.Is rather than string matching, and every refusal stops before any
// external send — none of them falls back to the legacy CLI delivery path.
var (
	// ErrEmptyContent means the final assistant output was empty. There is
	// nothing to deliver, and substituting something else would send content
	// the Agent never produced.
	ErrEmptyContent = errors.New("effect: empty payload content")
	// ErrPayloadTooLarge means the content exceeded the bounded publish size.
	ErrPayloadTooLarge = errors.New("effect: payload exceeds bounded size")
	// ErrMissingTarget means the delivery target projection was incomplete.
	ErrMissingTarget = errors.New("effect: missing delivery target")
	// ErrMissingIdentity means the occurrence linkage was absent, so the effect
	// could not be correlated back to the run that produced it.
	ErrMissingIdentity = errors.New("effect: missing occurrence identity")
	// ErrPayloadEffectMismatch means the payload snapshot and the planned
	// content for one execution disagrees with the content being planned. The
	// caller must not send: the ledger already holds different text for this
	// execution, so sending the in-memory copy would deliver the wrong message.
	ErrPayloadEffectMismatch = errors.New("effect: payload and effect are out of step")
)

// MaxPayloadBytes bounds the publishable content snapshot. A cron result that
// exceeds this is refused rather than truncated: a truncated message is not
// what the Agent produced.
const MaxPayloadBytes = 64 * 1024

// Payload is the bounded final assistant content snapshot for one execution.
//
// Publishable content only. Raw provider requests, full tool arguments and
// credentials never land here.
type Payload struct {
	PayloadID    string
	OccurrenceID string
	ExecutionID  string
	WorkerRunID  string
	Content      string
	ContentBytes int64
	ContentSHA   string
	CreatedAtMs  int64
}

// Effect is one durable external-delivery intent plus its lifecycle.
type Effect struct {
	EffectID        string
	OccurrenceID    string
	DeliveryOrdinal int64
	TargetRevision  string
	// Attempt counts send attempts. It is deliberately NOT part of the business
	// key: a retry advances the attempt on the same effect.
	Attempt int64

	SessionID   string
	ExecutionID string
	WorkerRunID string

	PayloadID     string
	PayloadSHA256 string

	TargetKind string
	TargetRef  string

	Status    Status
	ErrorCode string
	// Reason is bounded and human-readable. Never a raw provider or worker error.
	Reason string

	OwnerInstanceID string
	LeaseUntilMs    *int64
	LeaseVersion    int64

	ProviderRef string
	EvidenceRef string

	CreatedAtMs int64
	UpdatedAtMs int64
}

// BusinessKey is the stable identity of an effect.
//
// It deliberately excludes the attempt counter: a retry is another attempt on
// the same effect, never a second effect. Including the attempt would let a
// retry masquerade as independent work and double-deliver.
func BusinessKey(occurrenceID string, deliveryOrdinal int64, targetRevision string) string {
	return fmt.Sprintf("%s|%d|%s", occurrenceID, deliveryOrdinal, targetRevision)
}

// Plan is the request to durably record one delivery intent together with its
// content snapshot.
type Plan struct {
	OccurrenceID string
	// DeliveryOrdinal is the logical delivery index within the occurrence.
	DeliveryOrdinal int64
	// TargetRevision fingerprints the delivery target's configuration, so
	// editing the target yields a NEW effect instead of mutating an in-flight one.
	TargetRevision string

	SessionID   string
	ExecutionID string
	WorkerRunID string

	// Content is the bounded final assistant output to publish.
	Content string

	// TargetKind/TargetRef project the already-authorized job configuration.
	// Credentials are NOT carried here; the adapter resolves them at send time.
	TargetKind string
	TargetRef  string
}

// Validate rejects a plan that cannot produce a trustworthy effect. Every
// failure stops before any external send.
func (p Plan) Validate() error {
	if p.OccurrenceID == "" {
		return ErrMissingIdentity
	}
	if p.TargetRevision == "" {
		return fmt.Errorf("%w: target revision is required", ErrMissingIdentity)
	}
	if p.TargetKind == "" {
		return ErrMissingTarget
	}
	// Content is measured in bytes: the bound protects storage and the provider
	// request, both of which are byte-sized.
	if p.Content == "" {
		return ErrEmptyContent
	}
	if len(p.Content) > MaxPayloadBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrPayloadTooLarge, len(p.Content), MaxPayloadBytes)
	}
	return nil
}

// ContentHash returns the SHA-256 fingerprint of the content snapshot.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Row is the minimal result-row surface the planner needs for verification.
//
// It is an alias of eventstore.Row rather than a second interface with the same
// method set: Go requires identical types in matching method signatures, so two
// independently declared interfaces would make every eventstore.EventTx fail to
// satisfy Tx — exactly the interop this package depends on.
type Row = eventstore.Row

// Tx is the minimal transactional surface the planner needs. Both eventstore
// transaction implementations satisfy it, which is what lets the payload
// snapshot and the planned effect commit atomically in one transaction.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) Row
}

// dbTx adapts a plain *sql.Tx to Tx. *sql.Row satisfies Row structurally, but
// Go compares method signatures by declared type, so the wrapper is what lets
// an ordinary transaction — not just an eventstore transaction — plan effects.
type dbTx struct {
	*sql.Tx
}

func (t dbTx) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	return t.Tx.QueryRowContext(ctx, query, args...)
}

// Planner commits payload snapshots and planned effects atomically.
type Planner struct {
	dialect dbutil.Dialect
}

// NewPlanner creates a Planner for the given SQL dialect.
func NewPlanner(dialect dbutil.Dialect) *Planner {
	return &Planner{dialect: dialect}
}

const insertPayloadSQL = `INSERT INTO effect_payloads
	(payload_id, occurrence_id, execution_id, worker_run_id, content, content_bytes, content_sha256, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(occurrence_id, execution_id) DO NOTHING`

const insertEffectSQL = `INSERT INTO effects
	(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
	 session_id, execution_id, worker_run_id,
	 payload_id, payload_sha256,
	 target_kind, target_ref,
	 status, error_code, reason,
	 owner_instance_id, lease_until, lease_version,
	 provider_ref, evidence_ref,
	 created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(occurrence_id, delivery_ordinal, target_revision) DO NOTHING`

const selectPayloadSHASQL = `SELECT content_sha256 FROM effect_payloads
	WHERE occurrence_id = ? AND execution_id = ?`

// PlanWithPayload writes the content snapshot and the planned effect in the
// CALLER's transaction. The caller commits.
//
// Doing this in one transaction is the point: an effect can never be observed
// as planned while its content is missing, and a crash between the two writes
// leaves neither. Recovery re-plans by the same stable business key and
// converges on the same effect rather than creating a second one.
//
// Returns created=false when the business key was already taken, which is how
// recovery converges instead of double-planning.
func (p *Planner) PlanWithPayload(ctx context.Context, tx Tx, plan Plan, now time.Time) (*Effect, bool, error) {
	if err := plan.Validate(); err != nil {
		return nil, false, err
	}

	at := now.UnixMilli()
	contentSHA := ContentHash(plan.Content)
	payload := &Payload{
		PayloadID:    "pay_" + uuid.NewString(),
		OccurrenceID: plan.OccurrenceID,
		ExecutionID:  plan.ExecutionID,
		WorkerRunID:  plan.WorkerRunID,
		Content:      plan.Content,
		ContentBytes: int64(len(plan.Content)),
		ContentSHA:   contentSHA,
		CreatedAtMs:  at,
	}

	e := &Effect{
		EffectID:        "eff_" + uuid.NewString(),
		OccurrenceID:    plan.OccurrenceID,
		DeliveryOrdinal: plan.DeliveryOrdinal,
		TargetRevision:  plan.TargetRevision,
		Attempt:         0,
		SessionID:       plan.SessionID,
		ExecutionID:     plan.ExecutionID,
		WorkerRunID:     plan.WorkerRunID,
		PayloadID:       payload.PayloadID,
		PayloadSHA256:   contentSHA,
		TargetKind:      plan.TargetKind,
		TargetRef:       plan.TargetRef,
		Status:          StatusPlanned,
		LeaseVersion:    0,
		CreatedAtMs:     at,
		UpdatedAtMs:     at,
	}

	payloadRes, err := tx.ExecContext(ctx, p.dialect.Rebind(insertPayloadSQL),
		payload.PayloadID, payload.OccurrenceID, payload.ExecutionID, payload.WorkerRunID,
		payload.Content, payload.ContentBytes, payload.ContentSHA, payload.CreatedAtMs)
	if err != nil {
		return nil, false, fmt.Errorf("effect: insert payload: %w", err)
	}

	effectRes, err := tx.ExecContext(ctx, p.dialect.Rebind(insertEffectSQL),
		e.EffectID, e.OccurrenceID, e.DeliveryOrdinal, e.TargetRevision, e.Attempt,
		e.SessionID, e.ExecutionID, e.WorkerRunID,
		e.PayloadID, e.PayloadSHA256,
		e.TargetKind, e.TargetRef,
		e.Status, e.ErrorCode, e.Reason,
		e.OwnerInstanceID, e.LeaseUntilMs, e.LeaseVersion,
		e.ProviderRef, e.EvidenceRef,
		e.CreatedAtMs, e.UpdatedAtMs)
	if err != nil {
		return nil, false, fmt.Errorf("effect: insert effect: %w", err)
	}

	payloadInserted, err := rowsAffected(payloadRes)
	if err != nil {
		return nil, false, err
	}
	effectInserted, err := rowsAffected(effectRes)
	if err != nil {
		return nil, false, err
	}

	// An effect can never be committed without its payload — both writes are in
	// this one transaction. The remaining hazard is the opposite one: the
	// payload may already exist from an earlier commit for the same execution
	// while this plan carries different content. The ledger's copy wins, so
	// verify it rather than assume.
	if payloadInserted == 0 {
		var recordedSHA string
		row := tx.QueryRowContext(ctx, p.dialect.Rebind(selectPayloadSHASQL),
			plan.OccurrenceID, plan.ExecutionID)
		switch err := row.Scan(&recordedSHA); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, false, fmt.Errorf("%w: payload %s/%s is absent",
				ErrPayloadEffectMismatch, plan.OccurrenceID, plan.ExecutionID)
		case err != nil:
			return nil, false, fmt.Errorf("effect: verify existing payload: %w", err)
		case recordedSHA != contentSHA:
			return nil, false, fmt.Errorf(
				"%w: ledger holds a different payload for execution %s",
				ErrPayloadEffectMismatch, plan.ExecutionID)
		}
	}
	return e, effectInserted > 0, nil
}

func rowsAffected(res sql.Result) (int64, error) {
	if res == nil {
		return 0, nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("effect: rows affected: %w", err)
	}
	return n, nil
}
