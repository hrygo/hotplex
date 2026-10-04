package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"
)

// Bounded reasons a queued input can be settled without ever reaching a
// worker. They mirror the execution package's codes so a console reading the
// ledger and one reading metrics describe the same event.
const (
	queueReasonNotDispatchable = "QUEUE_NOT_DISPATCHABLE"
	queueReasonContentLost     = "QUEUE_CONTENT_UNAVAILABLE"
)

// preacceptedKey carries an execution record that the queue has ALREADY
// durably claimed into the ordinary delivery path.
//
// A queued input cannot simply be replayed as an ordinary one.
// acceptInputExecution recomputes the payload hash from the envelope, and a
// reconstructed envelope does not reproduce the stored hash — we deliberately
// persist content, not arbitrary metadata. Accepting it again would therefore
// return ErrPayloadConflict against the item's own execution row. Injecting the
// claimed record instead reuses the whole delivery path unchanged, including
// the Skill catalog re-validation, MarkRunning, the stop fence, audit and the
// delivery ACKs.
type preacceptedKey struct{}

func withPreaccepted(ctx context.Context, record *execution.Record) context.Context {
	return context.WithValue(ctx, preacceptedKey{}, record)
}

func preacceptedFrom(ctx context.Context) *execution.Record {
	record, _ := ctx.Value(preacceptedKey{}).(*execution.Record)
	return record
}

// queueConfig returns the live queue settings. A nil provider means the
// gateway was built without config, and the zero value disables the queue —
// which is the safe default, since accepting into a queue nothing dispatches
// would silently swallow inputs.
func (h *Handler) queueConfig() config.ExecutionQueueConfig {
	if h.configProvider == nil {
		return config.ExecutionQueueConfig{}
	}
	cfg := h.configProvider()
	if cfg == nil {
		return config.ExecutionQueueConfig{}
	}
	return cfg.Execution.Queue
}

func (h *Handler) queueEnabled() bool {
	return h.executionStore != nil && h.queueConfig().Enabled
}

func (h *Handler) queueLimits() execution.QueueLimits {
	cfg := h.queueConfig()
	return execution.QueueLimits{
		PerSession:      cfg.PerSession,
		Global:          cfg.Global,
		MaxPayloadBytes: cfg.MaxPayloadBytes,
		TTL:             cfg.TTL,
	}
}

// queuePayloadFor renders what a queued input will deliver. A Skill keeps both
// the text it was typed as and the invocation it resolved to: the text is what
// gets re-resolved against the CURRENT catalog at dispatch, and the recorded
// invocation is what that resolution is compared against so a Skill that
// changed identity while it waited is settled instead of silently running a
// different command.
func queuePayloadFor(content string, invocation *worker.NativeCommandInvocation) execution.QueuedPayload {
	payload := execution.QueuedPayload{Content: content}
	if invocation == nil {
		return payload
	}
	payload.Invocation = &execution.QueuedInvocation{
		Name: invocation.Name,
		Args: invocation.Args,
		Path: invocation.Path,
		Mode: string(invocation.Mode),
	}
	return payload
}

// EnqueueBusyInput durably accepts an input that arrived while its session was
// busy. It reports whether the input is now durably queued; a false return
// with a nil error means the caller should fall back to the volatile buffer.
func (h *Handler) EnqueueBusyInput(
	ctx context.Context,
	env *events.Envelope,
	content string,
	invocation *worker.NativeCommandInvocation,
) (*execution.Record, bool, error) {
	if !h.queueEnabled() {
		return nil, false, nil
	}
	payloadHash, err := inputPayloadHash(env)
	if err != nil {
		return nil, false, err
	}
	mode := "text"
	if invocation != nil {
		mode = "native_command"
	}

	record, _, duplicate, err := h.executionStore.AcceptQueued(ctx, execution.QueuedRequest{
		SessionID:       env.SessionID,
		ClientMessageID: clientMessageID(env),
		PayloadHash:     payloadHash,
		Payload:         queuePayloadFor(content, invocation),
		OwnerInstanceID: h.ownerInstanceID,
	}, h.queueLimits())
	if err != nil {
		reason := queueRefusalReason(err)
		observability.ExecutionQueueRefused().Add(ctx, 1, label("reason", reason))
		// A duplicate or a payload conflict is not a capacity problem: the
		// caller already has the record and must be told which it is.
		if duplicate && (errors.Is(err, execution.ErrPayloadConflict) || errors.Is(err, execution.ErrQueueFull)) {
			return record, true, err
		}
		return nil, false, err
	}
	if !duplicate {
		observability.ExecutionQueueAccepted().Add(ctx, 1, label("input_mode", mode))
		observability.ExecutionQueueDepth().Add(ctx, 1)
		h.log.Info("gateway: input durably queued",
			"session_id", env.SessionID,
			observability.KeyExecutionID, record.ExecutionID,
			"client_message_id", record.ClientMessageID,
			"input_mode", mode)
	}
	return record, true, nil
}

func label(key, value string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(key, value))
}

// queuedRecordFor finds the durable record behind a retried queued submission.
func (h *Handler) queuedRecordFor(ctx context.Context, sessionID, clientMessageID string) (*execution.Record, error) {
	return h.executionStore.QueuedByClientMessage(ctx, sessionID, clientMessageID)
}

func queueRefusalReason(err error) string {
	switch {
	case errors.Is(err, execution.ErrQueueFull):
		return "full"
	case errors.Is(err, execution.ErrQueuePayloadTooLarge):
		return "payload_too_large"
	case errors.Is(err, execution.ErrPayloadConflict):
		return "conflict"
	case errors.Is(err, execution.ErrPayloadContentUnavailable):
		return "content_unavailable"
	default:
		return "not_dispatchable"
	}
}

// DispatchQueued promotes the session's queue head across the dispatch
// boundary and delivers it.
//
// It runs off the forwarder's goroutine: the delivery path re-enters the seq
// barrier, which the emitting forwarder still holds. Every failure mode here
// leaves the queue usable — a busy session, an empty queue or a head that moved
// simply returns, and the next gate release tries again.
func (h *Handler) DispatchQueued(ctx context.Context, sessionID string) {
	if !h.queueEnabled() || sessionID == "" {
		return
	}
	entries, err := h.executionStore.QueueBySession(ctx, sessionID, 1)
	if err != nil {
		h.log.Warn("gateway: queue peek failed", "session_id", sessionID, "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	head := entries[0]

	// Read the content BEFORE claiming. The claim deletes the queue row, and
	// that delete cascades to the payload: reading afterwards would always find
	// the content already gone, and every dispatch would settle as
	// contentless. It also means we never claim an item we cannot read.
	payload, err := h.executionStore.QueuePayload(ctx, head.ExecutionID)
	if err != nil {
		observability.ExecutionQueueRefused().Add(ctx, 1, label("reason", queueRefusalReason(err)))
		if errors.Is(err, execution.ErrPayloadContentUnavailable) || errors.Is(err, execution.ErrNotFound) {
			// The control row promises recoverability the content cannot back.
			// Settle it so the promise is withdrawn instead of left standing.
			if _, settleErr := h.executionStore.CancelQueued(ctx, head.ExecutionID, queueReasonContentLost); settleErr != nil {
				h.log.Warn("gateway: settle contentless queued input failed",
					"session_id", sessionID,
					observability.KeyExecutionID, head.ExecutionID,
					"err", settleErr)
			} else {
				observability.ExecutionQueueSettled().Add(ctx, 1, label("reason", "cancelled"))
				observability.ExecutionQueueDepth().Add(ctx, -1)
			}
			return
		}
		h.log.Warn("gateway: queue payload read failed",
			"session_id", sessionID,
			observability.KeyExecutionID, head.ExecutionID,
			"err", err)
		return
	}

	workerRunID := ""
	if h.bridge != nil {
		workerRunID, _ = h.bridge.CurrentWorkerRunID(sessionID)
	}
	if workerRunID == "" {
		// The claim records the run that will carry the input. Without a current
		// binding, MarkRunning later rebinds to whatever run actually attaches,
		// so a placeholder is correct here rather than a guess at the real one.
		workerRunID = "run_" + uuid.NewString()
	}

	record, claimed, err := h.executionStore.ClaimQueued(ctx, execution.ClaimQueuedRequest{
		SessionID:           sessionID,
		OwnerInstanceID:     h.ownerInstanceID,
		WorkerRunID:         workerRunID,
		ExpectedExecutionID: head.ExecutionID,
	})
	if err != nil {
		switch {
		case errors.Is(err, execution.ErrNotFound),
			errors.Is(err, execution.ErrSessionBusy),
			errors.Is(err, execution.ErrQueueHeadMoved),
			errors.Is(err, execution.ErrQueueLifecycleStale):
			// Ordinary: nothing to do now, and nothing was lost. A later gate
			// release will try again.
			return
		default:
			h.log.Warn("gateway: queue claim failed", "session_id", sessionID, "err", err)
			return
		}
	}
	observability.ExecutionQueueDepth().Add(ctx, -1)
	observability.ExecutionQueueWaitMs().Record(ctx,
		float64(time.Now().UnixMilli()-claimed.EnqueuedAt))
	h.log.Info("gateway: queued input dispatched",
		"session_id", sessionID,
		observability.KeyExecutionID, record.ExecutionID,
		"client_message_id", record.ClientMessageID,
		"waited_ms", time.Now().UnixMilli()-claimed.EnqueuedAt)

	mode := "text"
	if payload.Invocation != nil {
		mode = "native_command"
	}
	observability.ExecutionQueueDispatched().Add(ctx, 1, label("input_mode", mode))

	env := queuedReplayEnvelope(record, payload.Content)
	dispatchCtx := withPreaccepted(ctx, record)

	invocation, matched, err := h.resolveSkillForSession(dispatchCtx, sessionID, payload.Content)
	if err != nil {
		// A Skill that no longer resolves must NOT be delivered as ordinary
		// text. Degrading it would silently run something the user did not ask
		// for, so the input is settled instead.
		h.log.Info("gateway: queued Skill no longer resolves",
			"session_id", sessionID,
			observability.KeyExecutionID, record.ExecutionID,
			"err", err)
		h.settleUndispatchable(ctx, record, queueReasonNotDispatchable)
		return
	}
	if payload.Invocation != nil {
		if !matched || invocation.Name != payload.Invocation.Name {
			h.log.Info("gateway: queued Skill identity changed while waiting",
				"session_id", sessionID,
				observability.KeyExecutionID, record.ExecutionID,
				"queued_name", payload.Invocation.Name)
			h.settleUndispatchable(ctx, record, queueReasonNotDispatchable)
			return
		}
	}

	if matched {
		if err := h.deliverSkillToWorker(dispatchCtx, env, payload.Content, invocation); err != nil {
			h.log.Warn("gateway: queued Skill dispatch failed",
				"session_id", sessionID,
				observability.KeyExecutionID, record.ExecutionID,
				"err", err)
		}
		return
	}
	if err := h.deliverToWorker(dispatchCtx, env, payload.Content); err != nil {
		h.log.Warn("gateway: queued input dispatch failed",
			"session_id", sessionID,
			observability.KeyExecutionID, record.ExecutionID,
			"err", err)
	}
}

// settleUndispatchable records an input that crossed the claim boundary but
// cannot be delivered. It is written as a terminal failure so the queue does
// not retry forever, with a bounded code that says the input never ran.
func (h *Handler) settleUndispatchable(ctx context.Context, record *execution.Record, reason string) {
	observability.ExecutionQueueSettled().Add(ctx, 1, label("reason", "not_dispatchable"))
	if err := h.finishInputExecution(ctx, record, execution.StatusFailed, events.ErrorCode(reason)); err != nil {
		h.log.Warn("gateway: settle undispatchable queued input failed",
			observability.KeyExecutionID, record.ExecutionID, "err", err)
	}
}

// queuedReplayEnvelope rebuilds the minimum envelope the delivery path needs.
// The client_message_id travels in Event.Data because that is where
// clientMessageID reads it from; without it the ACKs and the inbound capture
// would be attributed to a generated ID instead of the real input.
func queuedReplayEnvelope(record *execution.Record, content string) *events.Envelope {
	env := events.NewEnvelope(record.ExecutionID, record.SessionID, 0, events.Input, map[string]any{
		"content":           content,
		"client_message_id": record.ClientMessageID,
	})
	return env
}

// SweepExpiredQueue settles inputs whose TTL elapsed. It runs on a timer
// because an input nobody comes back for must not wait forever, but it is
// bounded so a large backlog cannot hold the write lock long enough to stall
// live input.
func (h *Handler) SweepExpiredQueue(ctx context.Context) int {
	if !h.queueEnabled() {
		return 0
	}
	cfg := h.queueConfig()
	batch := cfg.SweepBatch
	if batch <= 0 {
		batch = 100
	}
	expired, err := h.executionStore.ExpireQueued(ctx, time.Now(), batch)
	if err != nil {
		h.log.Warn("gateway: queue expiry sweep failed", "err", err)
		return 0
	}
	if len(expired) == 0 {
		return 0
	}
	observability.ExecutionQueueSettled().Add(ctx, int64(len(expired)),
		label("reason", "expired"))
	observability.ExecutionQueueDepth().Add(ctx, int64(-len(expired)))
	h.log.Info("gateway: expired queued inputs settled", "count", len(expired))
	return len(expired)
}

// StartQueueSweeper runs the TTL sweep until ctx is cancelled. A zero or
// negative interval disables it; expired inputs then wait for the next
// enqueue, dispatch or operator action rather than being swept.
func (h *Handler) StartQueueSweeper(ctx context.Context) {
	if !h.queueEnabled() {
		return
	}
	interval := h.queueConfig().SweepInterval
	if interval <= 0 {
		h.log.Info("gateway: queue expiry sweeper disabled")
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.SweepExpiredQueue(context.WithoutCancel(ctx))
			}
		}
	}()
}

// ClearSessionQueue settles every undispatched input for a session. It is what
// /reset and session end call, so a queue belonging to an abandoned turn never
// dispatches afterwards. The cancellation fact is kept, not erased.
//
// A disabled queue reports (0, nil): there is nothing to clear, which is not
// an error. Lifecycle callers cannot act on a failure here beyond logging, but
// the operator endpoint must be able to tell "nothing was queued" apart from
// "the clear did not happen", so the error is returned rather than folded into
// the count.
func (h *Handler) ClearSessionQueue(ctx context.Context, sessionID string) (int64, error) {
	if !h.queueEnabled() || sessionID == "" {
		return 0, nil
	}
	cleared, err := h.executionStore.ClearQueue(ctx, sessionID, execution.QueueReasonCancelled)
	if err != nil {
		return 0, fmt.Errorf("gateway: clear queued inputs: %w", err)
	}
	if cleared > 0 {
		observability.ExecutionQueueSettled().Add(ctx, cleared,
			label("reason", "cancelled"))
		observability.ExecutionQueueDepth().Add(ctx, -cleared)
		h.log.Info("gateway: queued inputs cleared", "session_id", sessionID, "count", cleared)
	}
	return cleared, nil
}

// CancelQueuedInput settles one undispatched input on operator or user
// request. It reports false when the item is no longer queued — because it has
// already been dispatched — so the caller can say so plainly instead of
// implying the input never ran.
func (h *Handler) CancelQueuedInput(ctx context.Context, executionID string) (bool, error) {
	if !h.queueEnabled() {
		return false, nil
	}
	record, err := h.executionStore.CancelQueued(ctx, executionID, execution.QueueReasonCancelled)
	if errors.Is(err, execution.ErrQueueNotQueued) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gateway: cancel queued input: %w", err)
	}
	observability.ExecutionQueueSettled().Add(ctx, 1, label("reason", "cancelled"))
	observability.ExecutionQueueDepth().Add(ctx, -1)
	h.log.Info("gateway: queued input cancelled",
		observability.KeyExecutionID, record.ExecutionID,
		"session_id", record.SessionID)
	return true, nil
}

// ackQueuedInput tells the client its input is durably queued and not yet
// dispatched. It carries the REAL execution ID, not the synthetic
// supplement-<client_id> the volatile paths use, because this input is now a
// durable record an operator can actually look up.
func (h *Handler) ackQueuedInput(ctx context.Context, source *events.Envelope, record *execution.Record, duplicate bool) {
	if h.hub == nil || record == nil {
		return
	}
	ack := events.NewEnvelope(aep.NewID(), source.SessionID, 0, events.InputAck, events.InputAckData{
		ClientMessageID:   record.ClientMessageID,
		ExecutionID:       record.ExecutionID,
		Status:            events.ExecutionStatusAccepted,
		Duplicate:         duplicate,
		InputMode:         events.InputModeQueued,
		Durability:        events.InputDurabilityDurable,
		ParentExecutionID: h.activeExecutionID(ctx, source.SessionID),
	})
	ack.Priority = events.PriorityControl
	ack.OwnerID = source.OwnerID
	ack.Metadata = map[string]any{
		"client_message_id":          record.ClientMessageID,
		observability.KeyExecutionID: record.ExecutionID,
	}
	if err := h.hub.SendToSession(context.WithoutCancel(ctx), ack); err != nil {
		h.log.Warn("gateway: queued input ack delivery failed", "err", err,
			"session_id", source.SessionID, observability.KeyExecutionID, record.ExecutionID)
	}
}
