package cron

import "context"

// EffectDeliveryRequest identifies one completed run whose final answer the
// gateway owns delivering.
//
// It carries only delivery-relevant references: the occurrence and execution
// identities, and the already-authorized target projection. Credentials are
// never here — the adapter resolves them at send time.
type EffectDeliveryRequest struct {
	JobID        string
	JobName      string
	OccurrenceID string
	SessionID    string
	ExecutionID  string
	Platform     string
	PlatformKey  map[string]string
}

// EffectDelivery delivers a run's final answer through the gateway's durable
// effect ledger instead of asking the Agent to send it.
//
// The contract mirrors what the ledger can promise: the delivery is recorded
// before anything is sent, an accepted send carries a provider reference, and
// an unprovable outcome is reported as unknown rather than retried.
type EffectDelivery interface {
	// DeliverEffect records the intent and its content, then sends under an
	// owner lease. It returns an error only when the run could not be
	// delivered at all; an unprovable provider outcome is not an error.
	DeliverEffect(ctx context.Context, req EffectDeliveryRequest) error
}
