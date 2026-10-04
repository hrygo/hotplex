package slack

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/hrygo/hotplex/internal/messaging"

	"github.com/slack-go/slack"
)

// Compile-time verification that the Slack adapter exposes the receipt path.
var _ messaging.ControlledSender = (*Adapter)(nil)

// SendWithReceipt posts one message and reports what Slack actually confirmed.
//
// The classification is the whole point of this method, so it is worth stating
// the rule it follows: an error is only "rejected" when Slack's response
// proves no message was created. A timeout, a dropped connection or a 5xx all
// leave the commit unproven and become unknown — never a safe retry.
func (a *Adapter) SendWithReceipt(
	ctx context.Context, text string, platformKey map[string]string,
) (messaging.SendResult, error) {
	channelID := platformKey["channel_id"]
	if channelID == "" {
		// Never reached the provider: nothing was attempted.
		return messaging.SendResult{}, errors.New("slack: missing channel_id in platform_key")
	}

	opts := []slack.MsgOption{slack.MsgOptionText(messaging.SanitizeText(text), false)}
	// thread_ts is honoured here rather than in the prompt: a cron reply must
	// land in the thread the operator configured, not at channel root.
	if threadTS := platformKey["thread_ts"]; threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	channel, timestamp, err := a.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return classifySlackSendError(ctx, err), nil
	}

	return messaging.SendResult{
		Outcome: messaging.SendAccepted,
		// An opaque reference, not a replay recipe. Nothing downstream may
		// treat it as an idempotency key.
		ProviderRef: channel + ":" + timestamp,
		Reason:      "slack accepted the message",
	}, nil
}

// DeliveryCapabilities reports what this adapter can promise today.
//
// Receipt is true because chat.postMessage returns the channel and timestamp
// of the created message. Idempotency and lookup are deliberately false:
// chat.postMessage carries a client_msg_id, but the platform does not promise
// durable deduplication for it, and this adapter exposes no message lookup, so
// an unknown outcome cannot be reconciled. Claiming otherwise would license
// blind resends.
func (a *Adapter) DeliveryCapabilities() messaging.ProviderCapabilities {
	return messaging.ProviderCapabilities{
		Idempotency: false,
		Lookup:      false,
		Receipt:     true,
	}
}

// classifySlackSendError turns a Slack API failure into a typed outcome.
//
// The returned error is always nil: the request DID reach Slack, so the caller
// must reason about the outcome rather than retry on a Go error.
func classifySlackSendError(ctx context.Context, err error) messaging.SendResult {
	// A rate limit is an explicit refusal to create anything, with the
	// provider's own backoff. That is the one rejection that is safe to retry.
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return messaging.SendResult{
			Outcome:        messaging.SendRejected,
			RejectionClass: messaging.RejectionSafeRetry,
			RetryAfter:     rateLimited.RetryAfter,
			Reason:         "slack rate limited the request",
		}
	}

	// The local context expiring proves nothing about the provider: the
	// request may already have been accepted before the deadline hit.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return messaging.SendResult{
			Outcome: messaging.SendUnknown,
			Reason:  "send deadline elapsed; the commit is unproven",
		}
	}

	var statusErr slack.StatusCodeError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.Code >= 500:
			// A 5xx does not prove non-commit. Slack may have created the
			// message and failed to answer.
			return messaging.SendResult{
				Outcome: messaging.SendUnknown,
				Reason:  "slack server error; the commit is unproven",
			}
		case statusErr.Code == http.StatusTooManyRequests:
			return messaging.SendResult{
				Outcome:        messaging.SendRejected,
				RejectionClass: messaging.RejectionSafeRetry,
				Reason:         "slack rate limited the request",
			}
		default:
			return messaging.SendResult{
				Outcome:        messaging.SendRejected,
				RejectionClass: messaging.RejectionPermanent,
				Reason:         "slack refused the request",
			}
		}
	}

	var apiErr slack.SlackErrorResponse
	if errors.As(err, &apiErr) {
		// Named Slack API errors (channel_not_found, not_in_channel,
		// invalid_auth, ...) are refusals: the message was not created.
		return messaging.SendResult{
			Outcome:        messaging.SendRejected,
			RejectionClass: messaging.RejectionPermanent,
			Reason:         boundedAPIError(apiErr.Err),
		}
	}

	// Transport-level failures (connection reset, DNS, TLS) leave the commit
	// unproven in both directions.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return messaging.SendResult{
			Outcome: messaging.SendUnknown,
			Reason:  "network failure; the commit is unproven",
		}
	}

	// Anything unrecognised is treated as the dangerous case, not the safe one.
	return messaging.SendResult{
		Outcome: messaging.SendUnknown,
		Reason:  "unrecognised slack failure; the commit is unproven",
	}
}

// boundedAPIError keeps the provider's own short error code and drops
// everything else, so no response body, channel content or token can reach a
// log line or an operator surface.
func boundedAPIError(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "slack refused the request"
	}
	const maxLen = 64
	if len(code) > maxLen {
		return code[:maxLen]
	}
	return code
}
