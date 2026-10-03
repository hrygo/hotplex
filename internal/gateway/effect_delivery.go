package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/messaging"
)

// FinalOutputLookback bounds how many recent turns are inspected when
// selecting a run's final answer. It is a ceiling, not a search strategy: the
// selection below stops at the first usable assistant turn.
const FinalOutputLookback = 20

// ErrNoFinalOutput means the run finished without a publishable assistant
// answer. Delivery stops here rather than substituting anything else — an
// invented or stale message is worse than no message.
var ErrNoFinalOutput = errors.New("gateway: no final assistant output for execution")

// ErrNoControlledSender means the target platform has no adapter that can
// return a receipt. The run is left to the legacy path rather than being
// delivered by a sender that cannot report what happened.
var ErrNoControlledSender = errors.New("gateway: no receipt-capable sender for platform")

// FinalOutputExtractor returns the final assistant text for one execution.
type FinalOutputExtractor func(ctx context.Context, sessionID, executionID string) (string, error)

// EffectDeliverer delivers a completed run's final answer through the durable
// effect ledger.
type EffectDeliverer struct {
	log       *slog.Logger
	effects   effect.Store
	extract   FinalOutputExtractor
	senderFor func(platform string) (messaging.ControlledSender, bool)
	// ownerInstanceID fences sends: two instances recovering the same
	// occurrence cannot both claim the effect.
	ownerInstanceID string
	lease           time.Duration
	now             func() time.Time
}

// EffectDelivererConfig wires the deliverer.
type EffectDelivererConfig struct {
	Log             *slog.Logger
	Effects         effect.Store
	Extract         FinalOutputExtractor
	SenderFor       func(platform string) (messaging.ControlledSender, bool)
	OwnerInstanceID string
	// Lease bounds how long this instance is trusted with a send. Zero uses a
	// conservative default.
	Lease time.Duration
	Now   func() time.Time
}

// NewEffectDeliverer creates the gateway-owned delivery path.
func NewEffectDeliverer(cfg EffectDelivererConfig) *EffectDeliverer {
	lease := cfg.Lease
	if lease <= 0 {
		lease = 60 * time.Second
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &EffectDeliverer{
		log:             cfg.Log.With("component", "effect_delivery"),
		effects:         cfg.Effects,
		extract:         cfg.Extract,
		senderFor:       cfg.SenderFor,
		ownerInstanceID: cfg.OwnerInstanceID,
		lease:           lease,
		now:             now,
	}
}

// Compile-time verification that the gateway satisfies cron's contract.
var _ cron.EffectDelivery = (*EffectDeliverer)(nil)

// DeliverEffect records the intent and its content, then sends once.
//
// The order is the guarantee: nothing reaches the provider before the effect
// and its exact text are committed, and nothing is sent at all if that commit
// fails. A restart between the two leaves a planned effect that recovery
// re-claims — never a message whose intent was lost.
func (d *EffectDeliverer) DeliverEffect(ctx context.Context, req cron.EffectDeliveryRequest) error {
	if strings.TrimSpace(req.OccurrenceID) == "" {
		return fmt.Errorf("%w: missing occurrence id", effect.ErrMissingIdentity)
	}
	if strings.TrimSpace(req.ExecutionID) == "" {
		return fmt.Errorf("%w: missing execution id", effect.ErrMissingIdentity)
	}

	sender, ok := d.senderFor(req.Platform)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoControlledSender, req.Platform)
	}

	content, err := d.extract(ctx, req.SessionID, req.ExecutionID)
	if err != nil {
		return fmt.Errorf("effect delivery: select final output: %w", err)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: execution %s", ErrNoFinalOutput, req.ExecutionID)
	}

	targetKind, targetRef, err := deliveryTarget(req)
	if err != nil {
		return err
	}

	planned, created, err := d.effects.PlanOnce(ctx, effect.Plan{
		OccurrenceID:    req.OccurrenceID,
		DeliveryOrdinal: 0,
		TargetRevision:  TargetRevision(req.Platform, req.PlatformKey),
		SessionID:       req.SessionID,
		ExecutionID:     req.ExecutionID,
		Content:         content,
		TargetKind:      targetKind,
		TargetRef:       targetRef,
	}, d.now())
	if err != nil {
		return fmt.Errorf("effect delivery: plan: %w", err)
	}
	if !created {
		// The intent already exists. If it already reached a decided state,
		// this delivery is done — re-sending is exactly what the business key
		// exists to prevent.
		switch planned.Status {
		case effect.StatusPlanned:
			// A previous process committed the intent and stopped before
			// sending. Fall through and claim it.
		default:
			d.log.Info("effect delivery: intent already decided, nothing to send",
				"occurrence_id", req.OccurrenceID, "status", planned.Status,
				"effect_id", planned.EffectID)
			return nil
		}
	}

	now := d.now()
	claimed, err := d.effects.ClaimSend(ctx, effect.ClaimRequest{
		EffectID:        planned.EffectID,
		OwnerInstanceID: d.ownerInstanceID,
		LeaseUntil:      now.Add(d.lease),
		ExpectedAttempt: planned.Attempt,
		Now:             now,
	})
	if err != nil {
		if errors.Is(err, effect.ErrLeaseLost) {
			// Another owner holds it. That is a correct outcome, not a failure:
			// the single-owner guarantee did its job.
			d.log.Info("effect delivery: send already owned elsewhere",
				"occurrence_id", req.OccurrenceID, "effect_id", planned.EffectID)
			return nil
		}
		return fmt.Errorf("effect delivery: claim: %w", err)
	}

	// The send happens OUTSIDE the transaction on purpose: a transaction held
	// across a network call would pin the database for the duration of a
	// provider timeout, and the lease — not the lock — is what protects the
	// effect while we are in flight.
	result, sendErr := sender.SendWithReceipt(ctx, content, req.PlatformKey)
	completion := effect.Completion{
		EffectID:     claimed.EffectID,
		Attempt:      claimed.Attempt,
		LeaseVersion: claimed.LeaseVersion,
		Now:          d.now(),
	}

	switch {
	case sendErr != nil:
		// The request never reached the provider, so nothing was created and
		// the attempt is safe to repeat.
		completion.Outcome = effect.StatusFailed
		completion.ErrorCode = "not_sent"
		completion.Reason = "delivery could not be attempted"
	case result.Outcome == messaging.SendAccepted:
		completion.Outcome = effect.StatusDelivered
		completion.ProviderRef = result.ProviderRef
		completion.EvidenceRef = result.EvidenceRef
		completion.Reason = "provider accepted the message"
	case result.Outcome == messaging.SendRejected:
		completion.Outcome = effect.StatusFailed
		completion.ErrorCode = string(result.RejectionClass)
		completion.Reason = result.Reason
	default:
		// The dangerous case: the request may or may not have committed.
		// Record it as unknown so nobody resends on the assumption it failed.
		completion.Outcome = effect.StatusUnknown
		completion.ErrorCode = "unproven"
		completion.Reason = result.Reason
	}

	if err := d.effects.CompleteSend(ctx, completion); err != nil {
		// Losing the write-back after a send is the worst case: the message
		// exists but the ledger does not say so. It is logged as unknown
		// rather than retried, because retrying the send is what would
		// actually cause the duplicate this ledger exists to prevent.
		d.log.Error("effect delivery: could not record send outcome",
			"effect_id", claimed.EffectID, "occurrence_id", req.OccurrenceID,
			"outcome", completion.Outcome, "err", err)
		return fmt.Errorf("effect delivery: record outcome: %w", err)
	}

	d.log.Info("effect delivery: recorded",
		"effect_id", claimed.EffectID, "occurrence_id", req.OccurrenceID,
		"outcome", completion.Outcome)
	return nil
}

// deliveryTarget projects the already-authorized job configuration into the
// target recorded on the effect.
//
// Credentials are never part of it: the adapter resolves them at send time,
// so a stored effect cannot leak them.
func deliveryTarget(req cron.EffectDeliveryRequest) (kind, ref string, err error) {
	if req.Platform == "" || req.Platform == "cron" {
		return "", "", fmt.Errorf("%w: job has no external delivery target", effect.ErrMissingTarget)
	}
	key, ok := cron.RequiredPlatformKey[req.Platform]
	if !ok {
		return "", "", fmt.Errorf("%w: unsupported platform %q", effect.ErrMissingTarget, req.Platform)
	}
	value := req.PlatformKey[key]
	if value == "" {
		return "", "", fmt.Errorf("%w: %s requires %s", effect.ErrMissingTarget, req.Platform, key)
	}
	return req.Platform, value, nil
}

// TargetRevision fingerprints a delivery target's configuration.
//
// Editing the target produces a NEW effect rather than mutating an in-flight
// one, so a message that was already on its way cannot be silently retargeted.
// The key set is sorted so map iteration order cannot change the fingerprint,
// and no free-form prompt or metadata value is included.
func TargetRevision(platform string, platformKey map[string]string) string {
	keys := make([]string, 0, len(platformKey))
	for k := range platformKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(platform)
	for _, k := range keys {
		b.WriteString("\x00")
		b.WriteString(k)
		b.WriteString("\x00")
		b.WriteString(platformKey[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// SelectFinalAssistantOutput returns the assistant answer this execution
// produced, or an error when there is none worth publishing.
//
// It deliberately refuses the tempting shortcuts. The newest turn may be the
// user's own message, an earlier generation's answer, a crash/timeout marker
// the gateway synthesised, or a failed turn — publishing any of those would
// deliver something the Agent never said.
func SelectFinalAssistantOutput(turns []*eventstore.TurnRecord) (string, error) {
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Role != "assistant" {
			continue
		}
		// Synthetic crash/timeout markers describe a failure; they are not
		// something the Agent produced.
		if t.Source == eventstore.SourceCrash || t.Source == eventstore.SourceTimeout {
			continue
		}
		if t.Success != nil && !*t.Success {
			continue
		}
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		return t.Content, nil
	}
	return "", ErrNoFinalOutput
}
