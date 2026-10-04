package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/effect"
)

// ErrTargetChanged means the delivery target an effect was promised against
// is no longer the one the job is configured for.
//
// Recovery stops rather than redirecting: the promise was made to a specific
// target revision, and quietly delivering it somewhere else is not recovery.
var ErrTargetChanged = errors.New("gateway: delivery target changed since the effect was planned")

// ErrTargetUnauthorized means the job behind an effect no longer exists or no
// longer authorizes delivery. The effect stays owed and visible.
var ErrTargetUnauthorized = errors.New("gateway: delivery target is no longer authorized")

// TargetResolver returns the CURRENT authorized delivery target for an
// occurrence's job.
//
// Recovery deliberately re-reads it instead of trusting what the effect
// stored: a job that was disabled, deleted or retargeted since the intent was
// committed must not keep delivering on the strength of an old read.
type TargetResolver func(
	ctx context.Context, occurrenceID string,
) (platform string, platformKey map[string]string, err error)

// RecoveryReport summarises one recovery pass.
type RecoveryReport struct {
	// ExpiredLeases counts effects fenced into unknown because their send
	// lease elapsed. These are not failures: they are outcomes nobody can
	// prove.
	ExpiredLeases int
	// Sent counts attempts actually made.
	Sent int
	// Deferred counts effects that stayed owed for a stated reason.
	Deferred int
	// Skipped counts effects another owner holds or that are already decided.
	Skipped int
}

// RecoverOnce runs one bounded recovery pass.
//
// It does three things in a fixed order. Fence first, so nothing that became
// uncertain can be picked up as fresh work. Then finish what is provably
// owed. Never the reverse: sending before fencing would race an executor
// whose lease merely looks stale.
func (d *EffectDeliverer) RecoverOnce(ctx context.Context, limit int) (RecoveryReport, error) {
	var report RecoveryReport
	now := d.now()

	expired, err := d.effects.ExpireLeases(ctx, now, limit)
	if err != nil {
		return report, fmt.Errorf("effect recovery: expire leases: %w", err)
	}
	report.ExpiredLeases = len(expired)
	for _, e := range expired {
		// An expired lease is recorded as unknown and stays that way. It is
		// never retried automatically: the send may have committed.
		d.log.Warn("effect recovery: send lease expired, delivery is now uncertain",
			"effect_id", e.EffectID, "occurrence_id", e.OccurrenceID,
			"attempt", e.Attempt)
	}

	owed, err := d.effects.ListRecoverable(ctx, limit)
	if err != nil {
		return report, fmt.Errorf("effect recovery: list recoverable: %w", err)
	}
	due, err := d.effects.ListDueRetries(ctx, now, d.budget.MaxAttempts, limit)
	if err != nil {
		return report, fmt.Errorf("effect recovery: list due retries: %w", err)
	}

	for _, e := range append(owed, due...) {
		// Recovery is a batch, not a pipeline: one stuck effect must not
		// consume the whole pass.
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		sent, err := d.recoverOne(ctx, e)
		switch {
		case err != nil:
			report.Deferred++
			d.log.Warn("effect recovery: delivery deferred",
				"effect_id", e.EffectID, "occurrence_id", e.OccurrenceID, "err", err)
		case sent:
			report.Sent++
		default:
			report.Skipped++
		}
	}
	return report, nil
}

// recoverOne attempts a single owed delivery. It reports whether a send was
// actually made.
func (d *EffectDeliverer) recoverOne(ctx context.Context, e *effect.Effect) (bool, error) {
	platform, platformKey, err := d.resolveTarget(ctx, e.OccurrenceID)
	if err != nil {
		return false, err
	}
	if revision := TargetRevision(platform, platformKey); revision != e.TargetRevision {
		// The target moved after this intent was promised. Sending anyway
		// would deliver a promise made about one place to another.
		return false, fmt.Errorf("%w: effect %s", ErrTargetChanged, e.EffectID)
	}

	sender, ok := d.senderFor(platform)
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNoControlledSender, platform)
	}

	payload, err := d.effects.GetPayload(ctx, e.PayloadID)
	if err != nil {
		// Missing content is reported, never substituted. A regenerated or
		// re-run answer would not be what was promised.
		return false, err
	}

	now := d.now()
	var claim *effect.Claim
	switch e.Status {
	case effect.StatusPlanned:
		claim, err = d.effects.ClaimSend(ctx, effect.ClaimRequest{
			EffectID:        e.EffectID,
			OwnerInstanceID: d.ownerInstanceID,
			LeaseUntil:      now.Add(d.lease),
			ExpectedAttempt: e.Attempt,
			Now:             now,
		})
	case effect.StatusStarted:
		claim, err = d.effects.ClaimRetry(ctx, effect.RetryRequest{
			EffectID:        e.EffectID,
			OwnerInstanceID: d.ownerInstanceID,
			LeaseUntil:      now.Add(d.lease),
			ExpectedAttempt: e.Attempt,
			MaxAttempts:     d.budget.MaxAttempts,
			Now:             now,
		})
	default:
		// delivered, failed and unknown are decided. Only an operator moves
		// them, and only with evidence.
		return false, nil
	}
	if err != nil {
		if errors.Is(err, effect.ErrLeaseLost) || errors.Is(err, effect.ErrNotClaimable) {
			// Another owner holds it, or the state moved between the scan and
			// the claim. Both are correct outcomes of a concurrent system.
			return false, nil
		}
		return false, err
	}

	if err := d.sendClaimed(ctx, claim, payload.Content, platformKey, sender, e.OccurrenceID); err != nil {
		return false, err
	}
	return true, nil
}

// StartRecoveryLoop drives RecoverOnce on an interval until the context ends.
//
// It exists so a restart converges without an operator, and it stops on
// cancellation rather than finishing a pass: dispatching sends during
// shutdown is how a duplicate gets created by the very process that is
// trying to hand work over.
func (d *EffectDeliverer) StartRecoveryLoop(ctx context.Context, interval time.Duration, batch int) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				report, err := d.RecoverOnce(ctx, batch)
				if err != nil {
					if ctx.Err() == nil {
						d.log.Error("effect recovery: pass failed", "err", err)
					}
					continue
				}
				if report.Sent > 0 || report.ExpiredLeases > 0 {
					d.log.Info("effect recovery: pass complete",
						"sent", report.Sent, "fenced_unknown", report.ExpiredLeases,
						"deferred", report.Deferred, "skipped", report.Skipped)
				}
			}
		}
	}()
}
