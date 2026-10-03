package observability

import (
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/metric"
)

var instrumentLog *slog.Logger

func setInstrumentLogger(l *slog.Logger) {
	instrumentLog = l
}

func warnInstrument(metricName string, err error) {
	if instrumentLog != nil {
		instrumentLog.Warn("observability: instrument creation failed", "metric", metricName, "err", err)
	}
}

// ─── Session Instruments ────────────────────────────────────────────

var (
	sessionCreated     metric.Int64Counter
	sessionCreatedInit sync.Once

	sessionTerminated     metric.Int64Counter
	sessionTerminatedInit sync.Once

	sessionDeleted     metric.Int64Counter
	sessionDeletedInit sync.Once

	sessionStartAttempts     metric.Int64Counter
	sessionStartAttemptsInit sync.Once

	sessionStartErrors     metric.Int64Counter
	sessionStartErrorsInit sync.Once

	sessionStartDuration     metric.Float64Histogram
	sessionStartDurationInit sync.Once
)

func SessionCreated() metric.Int64Counter {
	sessionCreatedInit.Do(func() {
		var err error
		sessionCreated, err = Meter().Int64Counter(
			"hotplex.session.created",
			metric.WithDescription("Total sessions created"),
		)
		if err != nil {
			warnInstrument("hotplex.session.created", err)
		}
	})
	return sessionCreated
}

func SessionTerminated() metric.Int64Counter {
	sessionTerminatedInit.Do(func() {
		var err error
		sessionTerminated, err = Meter().Int64Counter(
			"hotplex.session.terminated",
			metric.WithDescription("Total sessions terminated by reason"),
		)
		if err != nil {
			warnInstrument("hotplex.session.terminated", err)
		}
	})
	return sessionTerminated
}

func SessionDeleted() metric.Int64Counter {
	sessionDeletedInit.Do(func() {
		var err error
		sessionDeleted, err = Meter().Int64Counter(
			"hotplex.session.deleted",
			metric.WithDescription("Total sessions deleted by retention GC"),
		)
		if err != nil {
			warnInstrument("hotplex.session.deleted", err)
		}
	})
	return sessionDeleted
}

func SessionStartAttempts() metric.Int64Counter {
	sessionStartAttemptsInit.Do(func() {
		var err error
		sessionStartAttempts, err = Meter().Int64Counter(
			"hotplex.session.start.attempts",
			metric.WithDescription("Total session start attempts"),
		)
		if err != nil {
			warnInstrument("hotplex.session.start.attempts", err)
		}
	})
	return sessionStartAttempts
}

func SessionStartErrors() metric.Int64Counter {
	sessionStartErrorsInit.Do(func() {
		var err error
		sessionStartErrors, err = Meter().Int64Counter(
			"hotplex.session.start.errors",
			metric.WithDescription("Total session start errors"),
		)
		if err != nil {
			warnInstrument("hotplex.session.start.errors", err)
		}
	})
	return sessionStartErrors
}

func SessionStartDuration() metric.Float64Histogram {
	sessionStartDurationInit.Do(func() {
		var err error
		sessionStartDuration, err = Meter().Float64Histogram(
			"hotplex.session.start.duration",
			metric.WithDescription("Duration of session start in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 30, 60),
		)
		if err != nil {
			warnInstrument("hotplex.session.start.duration", err)
		}
	})
	return sessionStartDuration
}

// ─── Worker Instruments ─────────────────────────────────────────────

var (
	workerStarts     metric.Int64Counter
	workerStartsInit sync.Once

	workerExecDuration     metric.Float64Histogram
	workerExecDurationInit sync.Once

	workerCrashes     metric.Int64Counter
	workerCrashesInit sync.Once

	// Turn-Integrity diagnostics (Fix E): empty-success turns, stale-forwarder
	// event splits, assistant snapshot drift, and platform terminal fallbacks.
	workerEmptySuccess     metric.Int64Counter
	workerEmptySuccessInit sync.Once

	staleForwarderEvents     metric.Int64Counter
	staleForwarderEventsInit sync.Once

	assistantSnapshotDrift     metric.Int64Counter
	assistantSnapshotDriftInit sync.Once

	platformTerminalFallback     metric.Int64Counter
	platformTerminalFallbackInit sync.Once

	workerMemory     metric.Int64ObservableGauge
	workerMemoryInit sync.Once

	workerCreationDuration     metric.Float64Histogram
	workerCreationDurationInit sync.Once
)

func WorkerStarts() metric.Int64Counter {
	workerStartsInit.Do(func() {
		var err error
		workerStarts, err = Meter().Int64Counter(
			"hotplex.worker.starts",
			metric.WithDescription("Total worker starts"),
		)
		if err != nil {
			warnInstrument("hotplex.worker.starts", err)
		}
	})
	return workerStarts
}

func WorkerExecDuration() metric.Float64Histogram {
	workerExecDurationInit.Do(func() {
		var err error
		workerExecDuration, err = Meter().Float64Histogram(
			"hotplex.worker.execution.duration",
			metric.WithDescription("Worker execution duration in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(1, 5, 15, 30, 60, 120, 300, 600, 1800),
		)
		if err != nil {
			warnInstrument("hotplex.worker.execution.duration", err)
		}
	})
	return workerExecDuration
}

func WorkerCrashes() metric.Int64Counter {
	workerCrashesInit.Do(func() {
		var err error
		workerCrashes, err = Meter().Int64Counter(
			"hotplex.worker.crashes",
			metric.WithDescription("Total worker crashes"),
		)
		if err != nil {
			warnInstrument("hotplex.worker.crashes", err)
		}
	})
	return workerCrashes
}

// WorkerEmptySuccess counts successful Done events that delivered no displayable
// assistant content and no tool calls (Turn-Integrity Fix E). Labeled by
// worker_type and platform so empty-card incidents can be correlated to a
// worker × platform pair.
func WorkerEmptySuccess() metric.Int64Counter {
	workerEmptySuccessInit.Do(func() {
		var err error
		workerEmptySuccess, err = Meter().Int64Counter(
			"hotplex.worker.empty_success_total",
			metric.WithDescription("Successful turns that produced no displayable assistant content and no tool calls"),
		)
		if err != nil {
			warnInstrument("hotplex.worker.empty_success_total", err)
		}
	})
	return workerEmptySuccess
}

// StaleForwarderEvents counts events observed by a stale forwarder after a
// reset replaced its Conn (Turn-Integrity Fix E). Non-zero indicates the frozen
// binding failed to prevent a dual consumer.
func StaleForwarderEvents() metric.Int64Counter {
	staleForwarderEventsInit.Do(func() {
		var err error
		staleForwarderEvents, err = Meter().Int64Counter(
			"hotplex.gateway.stale_forwarder_event_total",
			metric.WithDescription("Events dropped by stale forwarders after a reset replaced their conn"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.stale_forwarder_event_total", err)
		}
	})
	return staleForwarderEvents
}

// AssistantSnapshotDrift counts mapper snapshot identity divergences — a full
// assistant snapshot that was not a strict prefix extension of the prior text
// (Turn-Integrity Fix E). Non-zero means snapshots were re-emitted in full
// rather than silently swallowed.
func AssistantSnapshotDrift() metric.Int64Counter {
	assistantSnapshotDriftInit.Do(func() {
		var err error
		assistantSnapshotDrift, err = Meter().Int64Counter(
			"hotplex.worker.assistant_snapshot_drift_total",
			metric.WithDescription("Assistant snapshots re-emitted in full after prefix drift instead of being swallowed"),
		)
		if err != nil {
			warnInstrument("hotplex.worker.assistant_snapshot_drift_total", err)
		}
	})
	return assistantSnapshotDrift
}

// PlatformTerminalFallback counts synthetic terminal messages emitted by a
// platform adapter when the worker produced nothing displayable (e.g. a Feishu
// placeholder replaced by an empty-success terminal, Turn-Integrity Fix E) or
// when a streaming terminal close failed before the body was presented and the
// platform delivered the fixed short fallback text (Slack terminal fallback).
func PlatformTerminalFallback() metric.Int64Counter {
	platformTerminalFallbackInit.Do(func() {
		var err error
		platformTerminalFallback, err = Meter().Int64Counter(
			"hotplex.messaging.platform_terminal_fallback_total",
			metric.WithDescription("Platform-side terminal fallbacks replacing an empty/undelivered streaming terminal"),
		)
		if err != nil {
			warnInstrument("hotplex.messaging.platform_terminal_fallback_total", err)
		}
	})
	return platformTerminalFallback
}

func WorkerMemory() metric.Int64ObservableGauge {
	workerMemoryInit.Do(func() {
		var err error
		workerMemory, err = Meter().Int64ObservableGauge(
			"hotplex.worker.memory.bytes",
			metric.WithDescription("Estimated worker memory by worker_type"),
			metric.WithUnit("By"),
		)
		if err != nil {
			warnInstrument("hotplex.worker.memory.bytes", err)
		}
	})
	return workerMemory
}

func WorkerCreationDuration() metric.Float64Histogram {
	workerCreationDurationInit.Do(func() {
		var err error
		workerCreationDuration, err = Meter().Float64Histogram(
			"hotplex.worker.creation.duration",
			metric.WithDescription("Duration of worker creation in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 30, 60),
		)
		if err != nil {
			warnInstrument("hotplex.worker.creation.duration", err)
		}
	})
	return workerCreationDuration
}

// ─── Gateway Instruments ────────────────────────────────────────────

var (
	gatewayConnections     metric.Int64ObservableGauge
	gatewayConnectionsInit sync.Once

	gatewayWebChatSessionOwnerConnections     metric.Int64ObservableGauge
	gatewayWebChatSessionOwnerConnectionsInit sync.Once

	gatewayMessages     metric.Int64Counter
	gatewayMessagesInit sync.Once

	gatewayEvents     metric.Int64Counter
	gatewayEventsInit sync.Once

	gatewayDeltasDropped     metric.Int64Counter
	gatewayDeltasDroppedInit sync.Once

	gatewayPlatformDropped     metric.Int64Counter
	gatewayPlatformDroppedInit sync.Once

	gatewayNoSubscribersDropped     metric.Int64Counter
	gatewayNoSubscribersDroppedInit sync.Once

	gatewayDeltaCoalesced     metric.Int64Counter
	gatewayDeltaCoalescedInit sync.Once

	gatewayDeltaFlush     metric.Int64Counter
	gatewayDeltaFlushInit sync.Once

	gatewayErrors     metric.Int64Counter
	gatewayErrorsInit sync.Once

	gatewayForwarderPanics     metric.Int64Counter
	gatewayForwarderPanicsInit sync.Once

	gatewayInitHandshakeDuration     metric.Float64Histogram
	gatewayInitHandshakeDurationInit sync.Once

	gatewayWebChatDuplicateConnectionRejected     metric.Int64Counter
	gatewayWebChatDuplicateConnectionRejectedInit sync.Once

	gatewayWebChatNonOwnerIngressRejected     metric.Int64Counter
	gatewayWebChatNonOwnerIngressRejectedInit sync.Once

	gatewayWebChatOwnerReleaseNotCurrent     metric.Int64Counter
	gatewayWebChatOwnerReleaseNotCurrentInit sync.Once
)

func GatewayConnections() metric.Int64ObservableGauge {
	gatewayConnectionsInit.Do(func() {
		var err error
		gatewayConnections, err = Meter().Int64ObservableGauge(
			"hotplex.gateway.connections",
			metric.WithDescription("Current WebSocket connections"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.connections", err)
		}
	})
	return gatewayConnections
}

// GatewayWebChatSessionOwnerConnections reports the number of sessions with a
// currently acquired WebChat WebSocket owner.
func GatewayWebChatSessionOwnerConnections() metric.Int64ObservableGauge {
	gatewayWebChatSessionOwnerConnectionsInit.Do(func() {
		var err error
		gatewayWebChatSessionOwnerConnections, err = Meter().Int64ObservableGauge(
			"hotplex.gateway.webchat.session_owner_connections",
			metric.WithDescription("Current sessions with an initialized WebChat WebSocket owner"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.webchat.session_owner_connections", err)
		}
	})
	return gatewayWebChatSessionOwnerConnections
}

func GatewayMessages() metric.Int64Counter {
	gatewayMessagesInit.Do(func() {
		var err error
		gatewayMessages, err = Meter().Int64Counter(
			"hotplex.gateway.messages",
			metric.WithDescription("Total messages by direction and event type"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.messages", err)
		}
	})
	return gatewayMessages
}

var (
	gatewayPermissionDedupHits     metric.Int64Counter
	gatewayPermissionDedupHitsInit sync.Once
)

// GatewayPermissionDedupHits counts permission requests suppressed because the
// same owner+fingerprint was denied within the dedup window. See
// docs/specs/Permission-Deny-Dedup-Spec.md.
func GatewayPermissionDedupHits() metric.Int64Counter {
	gatewayPermissionDedupHitsInit.Do(func() {
		var err error
		gatewayPermissionDedupHits, err = Meter().Int64Counter(
			"hotplex.gateway.permission_dedup.hits",
			metric.WithDescription("Permission requests suppressed after a recent user denial"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.permission_dedup.hits", err)
		}
	})
	return gatewayPermissionDedupHits
}

func GatewayEvents() metric.Int64Counter {
	gatewayEventsInit.Do(func() {
		var err error
		gatewayEvents, err = Meter().Int64Counter(
			"hotplex.gateway.events",
			metric.WithDescription("Total pass-through events by type and direction"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.events", err)
		}
	})
	return gatewayEvents
}

func GatewayDeltasDropped() metric.Int64Counter {
	gatewayDeltasDroppedInit.Do(func() {
		var err error
		gatewayDeltasDropped, err = Meter().Int64Counter(
			"hotplex.gateway.deltas.dropped",
			metric.WithDescription("Delta events dropped due to backpressure"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.deltas.dropped", err)
		}
	})
	return gatewayDeltasDropped
}

func GatewayPlatformDropped() metric.Int64Counter {
	gatewayPlatformDroppedInit.Do(func() {
		var err error
		gatewayPlatformDropped, err = Meter().Int64Counter(
			"hotplex.gateway.platform.dropped",
			metric.WithDescription("Events dropped at platform buffer level"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.platform.dropped", err)
		}
	})
	return gatewayPlatformDropped
}

func GatewayNoSubscribersDropped() metric.Int64Counter {
	gatewayNoSubscribersDroppedInit.Do(func() {
		var err error
		gatewayNoSubscribersDropped, err = Meter().Int64Counter(
			"hotplex.gateway.no_subscribers.dropped",
			metric.WithDescription("Events dropped due to no subscribers"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.no_subscribers.dropped", err)
		}
	})
	return gatewayNoSubscribersDropped
}

func GatewayDeltaCoalesced() metric.Int64Counter {
	gatewayDeltaCoalescedInit.Do(func() {
		var err error
		gatewayDeltaCoalesced, err = Meter().Int64Counter(
			"hotplex.gateway.delta.coalesced",
			metric.WithDescription("Delta events merged by coalescer"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.delta.coalesced", err)
		}
	})
	return gatewayDeltaCoalesced
}

func GatewayDeltaFlush() metric.Int64Counter {
	gatewayDeltaFlushInit.Do(func() {
		var err error
		gatewayDeltaFlush, err = Meter().Int64Counter(
			"hotplex.gateway.delta.flush",
			metric.WithDescription("Merged delta flushes sent to platform"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.delta.flush", err)
		}
	})
	return gatewayDeltaFlush
}

func GatewayErrors() metric.Int64Counter {
	gatewayErrorsInit.Do(func() {
		var err error
		gatewayErrors, err = Meter().Int64Counter(
			"hotplex.gateway.errors",
			metric.WithDescription("Gateway errors by error code"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.errors", err)
		}
	})
	return gatewayErrors
}

// GatewayForwarderPanics counts recovered panics in the per-session worker
// event forwarder. The worker_type label is a bounded worker enum.
func GatewayForwarderPanics() metric.Int64Counter {
	gatewayForwarderPanicsInit.Do(func() {
		var err error
		gatewayForwarderPanics, err = Meter().Int64Counter(
			"hotplex.gateway.forwarder.panics",
			metric.WithDescription("Recovered panics in worker event forwarders"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.forwarder.panics", err)
		}
	})
	return gatewayForwarderPanics
}

func GatewayInitHandshakeDuration() metric.Float64Histogram {
	gatewayInitHandshakeDurationInit.Do(func() {
		var err error
		gatewayInitHandshakeDuration, err = Meter().Float64Histogram(
			"hotplex.gateway.init.handshake.duration",
			metric.WithDescription("WebSocket init handshake duration in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.init.handshake.duration", err)
		}
	})
	return gatewayInitHandshakeDuration
}

// GatewayWebChatDuplicateConnectionRejected counts init handshakes rejected
// because their session already has a WebChat owner.
func GatewayWebChatDuplicateConnectionRejected() metric.Int64Counter {
	gatewayWebChatDuplicateConnectionRejectedInit.Do(func() {
		var err error
		gatewayWebChatDuplicateConnectionRejected, err = Meter().Int64Counter(
			"hotplex.gateway.webchat.duplicate_connection_rejected",
			metric.WithDescription("WebChat init attempts rejected because a session already has an owner"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.webchat.duplicate_connection_rejected", err)
		}
	})
	return gatewayWebChatDuplicateConnectionRejected
}

// GatewayWebChatNonOwnerIngressRejected counts owner-sensitive messages
// rejected after the sender lost or never acquired WebChat ownership.
func GatewayWebChatNonOwnerIngressRejected() metric.Int64Counter {
	gatewayWebChatNonOwnerIngressRejectedInit.Do(func() {
		var err error
		gatewayWebChatNonOwnerIngressRejected, err = Meter().Int64Counter(
			"hotplex.gateway.webchat.non_owner_ingress_rejected",
			metric.WithDescription("Owner-sensitive WebChat ingress rejected for a non-owner connection"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.webchat.non_owner_ingress_rejected", err)
		}
	})
	return gatewayWebChatNonOwnerIngressRejected
}

// GatewayWebChatOwnerReleaseNotCurrent counts close paths that attempted to
// release a session owner after ownership had already changed or was absent.
func GatewayWebChatOwnerReleaseNotCurrent() metric.Int64Counter {
	gatewayWebChatOwnerReleaseNotCurrentInit.Do(func() {
		var err error
		gatewayWebChatOwnerReleaseNotCurrent, err = Meter().Int64Counter(
			"hotplex.gateway.webchat.owner_release_not_current",
			metric.WithDescription("WebChat owner release attempts made by a non-current connection"),
		)
		if err != nil {
			warnInstrument("hotplex.gateway.webchat.owner_release_not_current", err)
		}
	})
	return gatewayWebChatOwnerReleaseNotCurrent
}

// ─── Pool Instruments ───────────────────────────────────────────────

var (
	poolAcquire     metric.Int64Counter
	poolAcquireInit sync.Once

	poolReleaseErrors     metric.Int64Counter
	poolReleaseErrorsInit sync.Once
)

func PoolAcquire() metric.Int64Counter {
	poolAcquireInit.Do(func() {
		var err error
		poolAcquire, err = Meter().Int64Counter(
			"hotplex.pool.acquire",
			metric.WithDescription("Pool acquire attempts by result"),
		)
		if err != nil {
			warnInstrument("hotplex.pool.acquire", err)
		}
	})
	return poolAcquire
}

func PoolReleaseErrors() metric.Int64Counter {
	poolReleaseErrorsInit.Do(func() {
		var err error
		poolReleaseErrors, err = Meter().Int64Counter(
			"hotplex.pool.release.errors",
			metric.WithDescription("Pool release errors (double-release bugs)"),
		)
		if err != nil {
			warnInstrument("hotplex.pool.release.errors", err)
		}
	})
	return poolReleaseErrors
}

// ─── Cron Instruments ───────────────────────────────────────────────

var (
	cronFires     metric.Int64Counter
	cronFiresInit sync.Once

	cronErrors     metric.Int64Counter
	cronErrorsInit sync.Once

	cronDuration     metric.Float64Histogram
	cronDurationInit sync.Once

	cronAttached     metric.Int64Counter
	cronAttachedInit sync.Once
)

func CronFires() metric.Int64Counter {
	cronFiresInit.Do(func() {
		var err error
		cronFires, err = Meter().Int64Counter(
			"hotplex.cron.fires",
			metric.WithDescription("Cron job fire attempts"),
		)
		if err != nil {
			warnInstrument("hotplex.cron.fires", err)
		}
	})
	return cronFires
}

func CronErrors() metric.Int64Counter {
	cronErrorsInit.Do(func() {
		var err error
		cronErrors, err = Meter().Int64Counter(
			"hotplex.cron.errors",
			metric.WithDescription("Cron job execution errors"),
		)
		if err != nil {
			warnInstrument("hotplex.cron.errors", err)
		}
	})
	return cronErrors
}

func CronDuration() metric.Float64Histogram {
	cronDurationInit.Do(func() {
		var err error
		cronDuration, err = Meter().Float64Histogram(
			"hotplex.cron.duration",
			metric.WithDescription("Cron job execution duration in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(1, 5, 15, 30, 60, 120, 300, 600, 1800),
		)
		if err != nil {
			warnInstrument("hotplex.cron.duration", err)
		}
	})
	return cronDuration
}

func CronAttached() metric.Int64Counter {
	cronAttachedInit.Do(func() {
		var err error
		cronAttached, err = Meter().Int64Counter(
			"hotplex.cron.attached",
			metric.WithDescription("Session callback attempts by result"),
		)
		if err != nil {
			warnInstrument("hotplex.cron.attached", err)
		}
	})
	return cronAttached
}

var (
	cronDeliveryRetry     metric.Int64Counter
	cronDeliveryRetryInit sync.Once
)

func CronDeliveryRetry() metric.Int64Counter {
	cronDeliveryRetryInit.Do(func() {
		var err error
		cronDeliveryRetry, err = Meter().Int64Counter(
			"hotplex.cron.delivery.result",
			metric.WithDescription("Cron delivery outcomes by status (success|exhausted|permanent)"),
		)
		if err != nil {
			warnInstrument("hotplex.cron.delivery.result", err)
		}
	})
	return cronDeliveryRetry
}

// ─── Streaming Card Instruments ─────────────────────────────────────

var (
	streamingCardRotations     metric.Int64Counter
	streamingCardRotationsInit sync.Once

	streamingCardRotationFailures     metric.Int64Counter
	streamingCardRotationFailuresInit sync.Once

	streamingCardFlushFallbacks     metric.Int64Counter
	streamingCardFlushFallbacksInit sync.Once

	streamingTerminalFailures     metric.Int64Counter
	streamingTerminalFailuresInit sync.Once
)

func StreamingCardRotations() metric.Int64Counter {
	streamingCardRotationsInit.Do(func() {
		var err error
		streamingCardRotations, err = Meter().Int64Counter(
			"hotplex.streaming.card.rotations",
			metric.WithDescription("Streaming card TTL rotations"),
		)
		if err != nil {
			warnInstrument("hotplex.streaming.card.rotations", err)
		}
	})
	return streamingCardRotations
}

func StreamingCardRotationFailures() metric.Int64Counter {
	streamingCardRotationFailuresInit.Do(func() {
		var err error
		streamingCardRotationFailures, err = Meter().Int64Counter(
			"hotplex.streaming.card.rotation_failures",
			metric.WithDescription("Streaming card rotation failures by phase"),
		)
		if err != nil {
			warnInstrument("hotplex.streaming.card.rotation_failures", err)
		}
	})
	return streamingCardRotationFailures
}

func StreamingCardFlushFallbacks() metric.Int64Counter {
	streamingCardFlushFallbacksInit.Do(func() {
		var err error
		streamingCardFlushFallbacks, err = Meter().Int64Counter(
			"hotplex.streaming.card.flush_fallbacks",
			metric.WithDescription("CardKit-to-IM Patch degradations"),
		)
		if err != nil {
			warnInstrument("hotplex.streaming.card.flush_fallbacks", err)
		}
	})
	return streamingCardFlushFallbacks
}

// StreamingTerminalFailures counts platform streaming terminal finalization
// failures (Feishu streaming cards and Slack streaming writers). The
// fallback_result attribute records whether the connection later sent the
// short static terminal fallback, could not send it, or skipped it because the
// body was already visible; the platform attribute identifies the messaging
// platform.
func StreamingTerminalFailures() metric.Int64Counter {
	streamingTerminalFailuresInit.Do(func() {
		var err error
		streamingTerminalFailures, err = newStreamingTerminalFailures(Meter())
		if err != nil {
			warnInstrument("hotplex.streaming.terminal_failures", err)
		}
	})
	return streamingTerminalFailures
}

func newStreamingTerminalFailures(meter metric.Meter) (metric.Int64Counter, error) {
	return meter.Int64Counter(
		"hotplex.streaming.terminal_failures",
		metric.WithDescription("Platform streaming terminal delivery failures by fallback result and platform"),
	)
}

// ─── ACP Instruments ────────────────────────────────────────────────

var (
	acpPromptTokens     metric.Int64Counter
	acpPromptTokensInit sync.Once

	acpToolCalls     metric.Int64Counter
	acpToolCallsInit sync.Once

	acpPermissionRequests     metric.Int64Counter
	acpPermissionRequestsInit sync.Once

	acpHandshakeDuration     metric.Float64Histogram
	acpHandshakeDurationInit sync.Once
)

func ACPPromptTokens() metric.Int64Counter {
	acpPromptTokensInit.Do(func() {
		var err error
		acpPromptTokens, err = Meter().Int64Counter(
			"hotplex.acp.prompt_tokens",
			metric.WithDescription("ACP token usage by type"),
		)
		if err != nil {
			warnInstrument("hotplex.acp.prompt_tokens", err)
		}
	})
	return acpPromptTokens
}

func ACPToolCalls() metric.Int64Counter {
	acpToolCallsInit.Do(func() {
		var err error
		acpToolCalls, err = Meter().Int64Counter(
			"hotplex.acp.tool_calls",
			metric.WithDescription("ACP tool calls by kind"),
		)
		if err != nil {
			warnInstrument("hotplex.acp.tool_calls", err)
		}
	})
	return acpToolCalls
}

func ACPPermissionRequests() metric.Int64Counter {
	acpPermissionRequestsInit.Do(func() {
		var err error
		acpPermissionRequests, err = Meter().Int64Counter(
			"hotplex.acp.permission_requests",
			metric.WithDescription("ACP permission requests by outcome"),
		)
		if err != nil {
			warnInstrument("hotplex.acp.permission_requests", err)
		}
	})
	return acpPermissionRequests
}

func ACPHandshakeDuration() metric.Float64Histogram {
	acpHandshakeDurationInit.Do(func() {
		var err error
		acpHandshakeDuration, err = Meter().Float64Histogram(
			"hotplex.acp.handshake.duration",
			metric.WithDescription("ACP agent initialize handshake duration in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5, 10),
		)
		if err != nil {
			warnInstrument("hotplex.acp.handshake.duration", err)
		}
	})
	return acpHandshakeDuration
}

// ─── Session Guard Instruments ────────────────────────────────────

var (
	sessionGuardRePersistFailures     metric.Int64Counter
	sessionGuardRePersistFailuresInit sync.Once

	sessionGuardRePersistConcurrentOverwrite     metric.Int64Counter
	sessionGuardRePersistConcurrentOverwriteInit sync.Once
)

func SessionGuardRePersistFailures() metric.Int64Counter {
	sessionGuardRePersistFailuresInit.Do(func() {
		var err error
		sessionGuardRePersistFailures, err = Meter().Int64Counter(
			"hotplex.session.transition.guard.repersist.failures",
			metric.WithDescription("transitionState guard re-persist failures (WorkerSessionID)"),
		)
		if err != nil {
			warnInstrument("hotplex.session.transition.guard.repersist.failures", err)
		}
	})
	return sessionGuardRePersistFailures
}

func SessionGuardRePersistConcurrentOverwrites() metric.Int64Counter {
	sessionGuardRePersistConcurrentOverwriteInit.Do(func() {
		var err error
		sessionGuardRePersistConcurrentOverwrite, err = Meter().Int64Counter(
			"hotplex.session.transition.guard.repersist.overwrites",
			metric.WithDescription("transitionState guard detected WorkerSessionID changed by another goroutine after re-persist"),
		)
		if err != nil {
			warnInstrument("hotplex.session.transition.guard.repersist.overwrites", err)
		}
	})
	return sessionGuardRePersistConcurrentOverwrite
}

// ─── Retry Instruments ──────────────────────────────────────────────

var (
	retryAttempts     metric.Int64Counter
	retryAttemptsInit sync.Once

	retryExhaustion     metric.Int64Counter
	retryExhaustionInit sync.Once
)

func RetryAttempts() metric.Int64Counter {
	retryAttemptsInit.Do(func() {
		var err error
		retryAttempts, err = Meter().Int64Counter(
			"hotplex.retry.attempts",
			metric.WithDescription("Auto-retry attempts by reason"),
		)
		if err != nil {
			warnInstrument("hotplex.retry.attempts", err)
		}
	})
	return retryAttempts
}

func RetryExhaustion() metric.Int64Counter {
	retryExhaustionInit.Do(func() {
		var err error
		retryExhaustion, err = Meter().Int64Counter(
			"hotplex.retry.exhaustion",
			metric.WithDescription("Retry exhaustion events"),
		)
		if err != nil {
			warnInstrument("hotplex.retry.exhaustion", err)
		}
	})
	return retryExhaustion
}

// ─── Audit Instruments ──────────────────────────────────────────────

var (
	auditEvents            metric.Int64Counter
	auditEventsInit        sync.Once
	auditChainBreaks       metric.Int64Counter
	auditChainBreaksInit   sync.Once
	auditSpill             metric.Int64Counter
	auditSpillInit         sync.Once
	auditWriteFailures     metric.Int64Counter
	auditWriteFailuresInit sync.Once
	auditSinkFailures      metric.Int64Counter
	auditSinkFailuresInit  sync.Once
)

func AuditEvents() metric.Int64Counter {
	auditEventsInit.Do(func() {
		var err error
		auditEvents, err = Meter().Int64Counter(
			"hotplex.audit.events",
			metric.WithDescription("User activity audit events by action and outcome"),
		)
		if err != nil {
			warnInstrument("hotplex.audit.events", err)
		}
	})
	return auditEvents
}

func AuditChainBreaks() metric.Int64Counter {
	auditChainBreaksInit.Do(func() {
		var err error
		auditChainBreaks, err = Meter().Int64Counter(
			"hotplex.audit.chain_breaks",
			metric.WithDescription("Hash chain break detections"),
		)
		if err != nil {
			warnInstrument("hotplex.audit.chain_breaks", err)
		}
	})
	return auditChainBreaks
}

func AuditSpill() metric.Int64Counter {
	auditSpillInit.Do(func() {
		var err error
		auditSpill, err = Meter().Int64Counter(
			"hotplex.audit.spill",
			metric.WithDescription("Audit events spilled to WAL on channel full"),
		)
		if err != nil {
			warnInstrument("hotplex.audit.spill", err)
		}
	})
	return auditSpill
}

func AuditWriteFailures() metric.Int64Counter {
	auditWriteFailuresInit.Do(func() {
		var err error
		auditWriteFailures, err = Meter().Int64Counter(
			"hotplex.audit.write_failures",
			metric.WithDescription("Audit DB write failures"),
		)
		if err != nil {
			warnInstrument("hotplex.audit.write_failures", err)
		}
	})
	return auditWriteFailures
}

func AuditSinkFailures() metric.Int64Counter {
	auditSinkFailuresInit.Do(func() {
		var err error
		auditSinkFailures, err = Meter().Int64Counter(
			"hotplex.audit.sink_failures",
			metric.WithDescription("AlertSink fan-out failures"),
		)
		if err != nil {
			warnInstrument("hotplex.audit.sink_failures", err)
		}
	})
	return auditSinkFailures
}

// ─── Durable Ingress Instruments (spec 2026-07-14) ──────────────────

var (
	executionAccept     metric.Int64Counter
	executionAcceptInit sync.Once

	executionDuplicate     metric.Int64Counter
	executionDuplicateInit sync.Once

	executionConflict     metric.Int64Counter
	executionConflictInit sync.Once

	executionSessionBusy     metric.Int64Counter
	executionSessionBusyInit sync.Once

	midTurnInjected     metric.Int64Counter
	midTurnInjectedInit sync.Once

	supplementBuffered     metric.Int64Counter
	supplementBufferedInit sync.Once

	executionQueueAccepted     metric.Int64Counter
	executionQueueAcceptedInit sync.Once

	executionQueueRefused     metric.Int64Counter
	executionQueueRefusedInit sync.Once

	executionQueueDispatched     metric.Int64Counter
	executionQueueDispatchedInit sync.Once

	executionQueueSettled     metric.Int64Counter
	executionQueueSettledInit sync.Once

	executionQueueDepth     metric.Int64UpDownCounter
	executionQueueDepthInit sync.Once

	executionQueueWaitMs     metric.Float64Histogram
	executionQueueWaitMsInit sync.Once

	runtimeQueueOperatorActions     metric.Int64Counter
	runtimeQueueOperatorActionsInit sync.Once

	executionDeliveryOutcome     metric.Int64Counter
	executionDeliveryOutcomeInit sync.Once

	executionRuntimeOutcome     metric.Int64Counter
	executionRuntimeOutcomeInit sync.Once

	executionDeliveryLatency     metric.Float64Histogram
	executionDeliveryLatencyInit sync.Once

	executionRuntimeDuration     metric.Float64Histogram
	executionRuntimeDurationInit sync.Once

	turnTTFT     metric.Float64Histogram
	turnTTFTInit sync.Once

	turnFirstTextLatency     metric.Float64Histogram
	turnFirstTextLatencyInit sync.Once

	turnStageDuration     metric.Float64Histogram
	turnStageDurationInit sync.Once

	turnWithoutOutput     metric.Int64Counter
	turnWithoutOutputInit sync.Once

	leaseRenewFailure     metric.Int64Counter
	leaseRenewFailureInit sync.Once

	leaseExpiredRecovery     metric.Int64Counter
	leaseExpiredRecoveryInit sync.Once

	repairAttempts     metric.Int64Counter
	repairAttemptsInit sync.Once

	repairSuccess     metric.Int64Counter
	repairSuccessInit sync.Once

	repairTimeout     metric.Int64Counter
	repairTimeoutInit sync.Once

	repairDropped     metric.Int64Counter
	repairDroppedInit sync.Once

	runtimeFenceActions     metric.Int64Counter
	runtimeFenceActionsInit sync.Once

	runtimeFenceConflicts     metric.Int64Counter
	runtimeFenceConflictsInit sync.Once

	runtimeEffectActions     metric.Int64Counter
	runtimeEffectActionsInit sync.Once

	runtimeEffectConflicts     metric.Int64Counter
	runtimeEffectConflictsInit sync.Once

	runtimePlanResolutions     metric.Int64Counter
	runtimePlanResolutionsInit sync.Once

	runtimePlanBlocked     metric.Int64Counter
	runtimePlanBlockedInit sync.Once

	runtimePlanObserved     metric.Int64Counter
	runtimePlanObservedInit sync.Once
)

func ExecutionAccept() metric.Int64Counter {
	executionAcceptInit.Do(func() {
		var err error
		executionAccept, err = Meter().Int64Counter(
			"hotplex.execution.accept",
			metric.WithDescription("Execution accepts (new input durably recorded)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.accept", err)
		}
	})
	return executionAccept
}

func ExecutionDuplicate() metric.Int64Counter {
	executionDuplicateInit.Do(func() {
		var err error
		executionDuplicate, err = Meter().Int64Counter(
			"hotplex.execution.duplicate",
			metric.WithDescription("Execution duplicate suppressions"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.duplicate", err)
		}
	})
	return executionDuplicate
}

func ExecutionConflict() metric.Int64Counter {
	executionConflictInit.Do(func() {
		var err error
		executionConflict, err = Meter().Int64Counter(
			"hotplex.execution.conflict",
			metric.WithDescription("Execution payload conflicts (same client_message_id, different hash)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.conflict", err)
		}
	})
	return executionConflict
}

func ExecutionSessionBusy() metric.Int64Counter {
	executionSessionBusyInit.Do(func() {
		var err error
		executionSessionBusy, err = Meter().Int64Counter(
			"hotplex.execution.session_busy",
			metric.WithDescription("Execution active-gate rejections (session has pending/running execution)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.session_busy", err)
		}
	})
	return executionSessionBusy
}

// MidTurnInjected counts user supplements injected into a running turn
// (worker implements MidTurnInjector; CC headless stream-json / codex turn-steer).
func MidTurnInjected() metric.Int64Counter {
	midTurnInjectedInit.Do(func() {
		var err error
		midTurnInjected, err = Meter().Int64Counter(
			"hotplex.execution.mid_turn_injected",
			metric.WithDescription("User supplements injected into a running turn (mid-turn passthrough)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.mid_turn_injected", err)
		}
	})
	return midTurnInjected
}

// SupplementBuffered counts user supplements buffered for replay when the
// worker does not support mid-turn injection (acp/ocs fallback).
func SupplementBuffered() metric.Int64Counter {
	supplementBufferedInit.Do(func() {
		var err error
		supplementBuffered, err = Meter().Int64Counter(
			"hotplex.execution.supplement_buffered",
			metric.WithDescription("User supplements buffered for replay (worker lacks mid-turn support)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.supplement_buffered", err)
		}
	})
	return supplementBuffered
}

// ExecutionQueueAccepted counts inputs durably accepted into the bounded
// queue. Separate from supplement_buffered because that one is process-local
// and lost on restart; conflating them would make a volatile backlog look
// durable in the metrics.
func ExecutionQueueAccepted() metric.Int64Counter {
	executionQueueAcceptedInit.Do(func() {
		var err error
		executionQueueAccepted, err = Meter().Int64Counter(
			"hotplex.execution.queue_accepted",
			metric.WithDescription("Inputs durably accepted into the bounded queue. Labels: input_mode (text|native_command)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_accepted", err)
		}
	})
	return executionQueueAccepted
}

// ExecutionQueueRefused counts queue rejections by reason. Labels are bounded
// enums only — a session ID or client message ID would blow up cardinality.
func ExecutionQueueRefused() metric.Int64Counter {
	executionQueueRefusedInit.Do(func() {
		var err error
		executionQueueRefused, err = Meter().Int64Counter(
			"hotplex.execution.queue_refused",
			metric.WithDescription("Queue rejections. Labels: reason (full|payload_too_large|conflict|content_unavailable|not_dispatchable)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_refused", err)
		}
	})
	return executionQueueRefused
}

// ExecutionQueueDispatched counts queue heads promoted across the dispatch
// boundary.
func ExecutionQueueDispatched() metric.Int64Counter {
	executionQueueDispatchedInit.Do(func() {
		var err error
		executionQueueDispatched, err = Meter().Int64Counter(
			"hotplex.execution.queue_dispatched",
			metric.WithDescription("Queued inputs claimed and dispatched. Labels: input_mode (text|native_command)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_dispatched", err)
		}
	})
	return executionQueueDispatched
}

// ExecutionQueueSettled counts queued inputs settled without ever reaching a
// worker. Labels: reason (cancelled|expired|not_dispatchable).
func ExecutionQueueSettled() metric.Int64Counter {
	executionQueueSettledInit.Do(func() {
		var err error
		executionQueueSettled, err = Meter().Int64Counter(
			"hotplex.execution.queue_settled",
			metric.WithDescription("Queued inputs settled without dispatch. Labels: reason (cancelled|expired|not_dispatchable)"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_settled", err)
		}
	})
	return executionQueueSettled
}

// ExecutionQueueDepth tracks how many inputs are waiting. It is a gauge
// because the queue is a level, not a flow.
func ExecutionQueueDepth() metric.Int64UpDownCounter {
	executionQueueDepthInit.Do(func() {
		var err error
		executionQueueDepth, err = Meter().Int64UpDownCounter(
			"hotplex.execution.queue_depth",
			metric.WithDescription("Inputs currently waiting in the durable queue"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_depth", err)
		}
	})
	return executionQueueDepth
}

// ExecutionQueueWaitMs measures how long an input waited before dispatch. This
// is the number that tells an operator whether the queue is helping.
func ExecutionQueueWaitMs() metric.Float64Histogram {
	executionQueueWaitMsInit.Do(func() {
		var err error
		executionQueueWaitMs, err = Meter().Float64Histogram(
			"hotplex.execution.queue_wait_ms",
			metric.WithDescription("Milliseconds a queued input waited before dispatch"),
			metric.WithUnit("ms"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.queue_wait_ms", err)
		}
	})
	return executionQueueWaitMs
}

// RuntimeQueueOperatorActions counts operator decisions applied to the durable
// input queue. Labels: action (cancel|clear), result (ok|error). Low-cardinality
// only — no execution IDs, no actors, no input content.
func RuntimeQueueOperatorActions() metric.Int64Counter {
	runtimeQueueOperatorActionsInit.Do(func() {
		var err error
		runtimeQueueOperatorActions, err = Meter().Int64Counter(
			"hotplex.runtime.queue_actions",
			metric.WithDescription("Operator input-queue decisions applied. Labels: action (cancel|clear), result (ok|error)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.queue_actions", err)
		}
	})
	return runtimeQueueOperatorActions
}

func ExecutionDeliveryOutcome() metric.Int64Counter {
	executionDeliveryOutcomeInit.Do(func() {
		var err error
		executionDeliveryOutcome, err = Meter().Int64Counter(
			"hotplex.execution.delivery_outcome",
			metric.WithDescription("Delivery outcomes by status (delivered/unknown/failed). Labels: delivery_status"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.delivery_outcome", err)
		}
	})
	return executionDeliveryOutcome
}

func ExecutionRuntimeOutcome() metric.Int64Counter {
	executionRuntimeOutcomeInit.Do(func() {
		var err error
		executionRuntimeOutcome, err = Meter().Int64Counter(
			"hotplex.execution.runtime_outcome",
			metric.WithDescription("Runtime outcomes by status (completed/failed/unknown). Labels: runtime_status"),
		)
		if err != nil {
			warnInstrument("hotplex.execution.runtime_outcome", err)
		}
	})
	return executionRuntimeOutcome
}

func ExecutionDeliveryLatency() metric.Float64Histogram {
	executionDeliveryLatencyInit.Do(func() {
		var err error
		executionDeliveryLatency, err = Meter().Float64Histogram(
			"hotplex.execution.delivery_latency",
			metric.WithDescription("Time from accept to delivery outcome in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10),
		)
		if err != nil {
			warnInstrument("hotplex.execution.delivery_latency", err)
		}
	})
	return executionDeliveryLatency
}

func ExecutionRuntimeDuration() metric.Float64Histogram {
	executionRuntimeDurationInit.Do(func() {
		var err error
		executionRuntimeDuration, err = Meter().Float64Histogram(
			"hotplex.execution.runtime_duration",
			metric.WithDescription("Worker turn duration in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 30, 60, 120, 300),
		)
		if err != nil {
			warnInstrument("hotplex.execution.runtime_duration", err)
		}
	})
	return executionRuntimeDuration
}

// TurnTTFT records gateway input receipt to first user-visible worker output.
// Labels are bounded to worker_type and first_output (reasoning/text).
func TurnTTFT() metric.Float64Histogram {
	turnTTFTInit.Do(func() {
		var err error
		turnTTFT, err = Meter().Float64Histogram(
			"hotplex.turn.ttft",
			metric.WithDescription("Gateway receipt to first user-visible worker output in seconds. Labels: worker_type, first_output"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60),
		)
		if err != nil {
			warnInstrument("hotplex.turn.ttft", err)
		}
	})
	return turnTTFT
}

// TurnFirstTextLatency records gateway input receipt to the first text delta.
// It remains distinct from TTFT when a reasoning event is visible first.
func TurnFirstTextLatency() metric.Float64Histogram {
	turnFirstTextLatencyInit.Do(func() {
		var err error
		turnFirstTextLatency, err = Meter().Float64Histogram(
			"hotplex.turn.first_text_latency",
			metric.WithDescription("Gateway receipt to first text delta in seconds. Labels: worker_type"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60),
		)
		if err != nil {
			warnInstrument("hotplex.turn.first_text_latency", err)
		}
	})
	return turnFirstTextLatency
}

// TurnStageDuration records bounded admission, dispatch, and first-output stages.
func TurnStageDuration() metric.Float64Histogram {
	turnStageDurationInit.Do(func() {
		var err error
		turnStageDuration, err = Meter().Float64Histogram(
			"hotplex.turn.stage_duration",
			metric.WithDescription("Turn stage duration in seconds. Labels: worker_type, stage"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60),
		)
		if err != nil {
			warnInstrument("hotplex.turn.stage_duration", err)
		}
	})
	return turnStageDuration
}

// TurnWithoutOutput counts terminal turns that did not emit user-visible output.
func TurnWithoutOutput() metric.Int64Counter {
	turnWithoutOutputInit.Do(func() {
		var err error
		turnWithoutOutput, err = Meter().Int64Counter(
			"hotplex.turn.without_output",
			metric.WithDescription("Completed worker turns without visible output. Labels: worker_type, terminal_status"),
		)
		if err != nil {
			warnInstrument("hotplex.turn.without_output", err)
		}
	})
	return turnWithoutOutput
}

func LeaseRenewFailure() metric.Int64Counter {
	leaseRenewFailureInit.Do(func() {
		var err error
		leaseRenewFailure, err = Meter().Int64Counter(
			"hotplex.lease.renew_failure",
			metric.WithDescription("Lease renewal failures"),
		)
		if err != nil {
			warnInstrument("hotplex.lease.renew_failure", err)
		}
	})
	return leaseRenewFailure
}

func LeaseExpiredRecovery() metric.Int64Counter {
	leaseExpiredRecoveryInit.Do(func() {
		var err error
		leaseExpiredRecovery, err = Meter().Int64Counter(
			"hotplex.lease.expired_recovery",
			metric.WithDescription("Expired lease recoveries (runtime set to unknown, fence set)"),
		)
		if err != nil {
			warnInstrument("hotplex.lease.expired_recovery", err)
		}
	})
	return leaseExpiredRecovery
}

func RepairAttempts() metric.Int64Counter {
	repairAttemptsInit.Do(func() {
		var err error
		repairAttempts, err = Meter().Int64Counter(
			"hotplex.repair.attempts",
			metric.WithDescription("Terminal state repair attempts"),
		)
		if err != nil {
			warnInstrument("hotplex.repair.attempts", err)
		}
	})
	return repairAttempts
}

func RepairSuccess() metric.Int64Counter {
	repairSuccessInit.Do(func() {
		var err error
		repairSuccess, err = Meter().Int64Counter(
			"hotplex.repair.success",
			metric.WithDescription("Terminal state repair successes"),
		)
		if err != nil {
			warnInstrument("hotplex.repair.success", err)
		}
	})
	return repairSuccess
}

func RepairTimeout() metric.Int64Counter {
	repairTimeoutInit.Do(func() {
		var err error
		repairTimeout, err = Meter().Int64Counter(
			"hotplex.repair.timeout",
			metric.WithDescription("Terminal state repair timeouts (exceeded MaxLifetime)"),
		)
		if err != nil {
			warnInstrument("hotplex.repair.timeout", err)
		}
	})
	return repairTimeout
}

func RepairDropped() metric.Int64Counter {
	repairDroppedInit.Do(func() {
		var err error
		repairDropped, err = Meter().Int64Counter(
			"hotplex.repair.dropped",
			metric.WithDescription("Terminal state repair drops (queue full, lease recovery fallback)"),
		)
		if err != nil {
			warnInstrument("hotplex.repair.dropped", err)
		}
	})
	return repairDropped
}

// ─── Runtime Fence Instruments (#877) ───────────────────────────────

// RuntimeFenceActions counts operator fence decisions applied via the Admin
// API. Labels: decision (resolve|abandon), result (ok|conflict|error).
// Low-cardinality only — no execution IDs or actors (#877 spec §11).
func RuntimeFenceActions() metric.Int64Counter {
	runtimeFenceActionsInit.Do(func() {
		var err error
		runtimeFenceActions, err = Meter().Int64Counter(
			"hotplex.runtime.fence_actions",
			metric.WithDescription("Operator fence decisions applied. Labels: decision (resolve|abandon), result (ok|conflict|error)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.fence_actions", err)
		}
	})
	return runtimeFenceActions
}

// RuntimeFenceConflicts counts fence-version conflicts (409). A rising value
// means operators (or gateway restarts between inspect and action) raced on
// the same fence; investigate before retrying.
func RuntimeFenceConflicts() metric.Int64Counter {
	runtimeFenceConflictsInit.Do(func() {
		var err error
		runtimeFenceConflicts, err = Meter().Int64Counter(
			"hotplex.runtime.fence_conflicts",
			metric.WithDescription("Fence-version conflicts on operator decisions (conditional update matched 0 rows)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.fence_conflicts", err)
		}
	})
	return runtimeFenceConflicts
}

// ─── Runtime Effect Instruments ──────────────────────────────────────

// RuntimeEffectActions counts operator decisions applied to an uncertain
// delivery effect. Labels: decision (abandon|mark_delivered|requeue),
// result (ok|conflict|error). Low-cardinality only — no effect IDs, no
// actors, no message content.
func RuntimeEffectActions() metric.Int64Counter {
	runtimeEffectActionsInit.Do(func() {
		var err error
		runtimeEffectActions, err = Meter().Int64Counter(
			"hotplex.runtime.effect_actions",
			metric.WithDescription("Operator effect decisions applied. Labels: decision (abandon|mark_delivered|requeue), result (ok|conflict|error)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.effect_actions", err)
		}
	})
	return runtimeEffectActions
}

// RuntimeEffectConflicts counts effect-status conflicts (409). A rising value
// means operators decided against a stale view, or a late receipt arrived
// while one was deciding. Neither is a bug; a spike means the console is
// showing operators stale state.
func RuntimeEffectConflicts() metric.Int64Counter {
	runtimeEffectConflictsInit.Do(func() {
		var err error
		runtimeEffectConflicts, err = Meter().Int64Counter(
			"hotplex.runtime.effect_conflicts",
			metric.WithDescription("Effect-status conflicts on operator decisions (conditional update matched 0 rows)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.effect_conflicts", err)
		}
	})
	return runtimeEffectConflicts
}

// ─── Runtime Plan Instruments (#946) ────────────────────────────────

// RuntimePlanResolutions counts EffectiveRuntimePlan resolutions in shadow
// mode. Labels: result (ok|blocked). The plan hash is NEVER a label
// (high cardinality — #946 spec §6.4).
func RuntimePlanResolutions() metric.Int64Counter {
	runtimePlanResolutionsInit.Do(func() {
		var err error
		runtimePlanResolutions, err = Meter().Int64Counter(
			"hotplex.runtime.plan_resolutions",
			metric.WithDescription("Effective runtime plan resolutions. Labels: result (ok|blocked)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.plan_resolutions", err)
		}
	})
	return runtimePlanResolutions
}

// RuntimePlanBlocked counts fail-closed plan block reasons (#946 spec §6.2).
// Labels: code (unknown_worker_type|invalid_permission_mode|
// invalid_sandbox_mode|facts_missing_or_conflicting|capability_unverifiable|
// secret_shaped_value). Low-cardinality only.
func RuntimePlanBlocked() metric.Int64Counter {
	runtimePlanBlockedInit.Do(func() {
		var err error
		runtimePlanBlocked, err = Meter().Int64Counter(
			"hotplex.runtime.plan_blocked",
			metric.WithDescription("Blocked effective runtime plans by fail-closed reason code"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.plan_blocked", err)
		}
	})
	return runtimePlanBlocked
}

// RuntimePlanObserved counts observed-bootstrap states served by the plan
// read path (#946 spec §6.5). Labels: state
// (planned|unknown|declared|partial|enforced). Low-cardinality only.
func RuntimePlanObserved() metric.Int64Counter {
	runtimePlanObservedInit.Do(func() {
		var err error
		runtimePlanObserved, err = Meter().Int64Counter(
			"hotplex.runtime.plan_observed",
			metric.WithDescription("Observed runtime plan bootstrap states served. Labels: state (planned|unknown|declared|partial|enforced)"),
		)
		if err != nil {
			warnInstrument("hotplex.runtime.plan_observed", err)
		}
	})
	return runtimePlanObserved
}
