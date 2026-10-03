package slack

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/messaging"
)

// receiptAPI records what the adapter asked Slack to send and replays a
// scripted provider answer.
type receiptAPI struct {
	SlackAPI
	channel  string
	options  []slack.MsgOption
	called   bool
	replyCh  string
	replyTS  string
	replyErr error
}

func (c *receiptAPI) PostMessageContext(
	_ context.Context, channelID string, options ...slack.MsgOption,
) (string, string, error) {
	c.called = true
	c.channel = channelID
	c.options = options
	return c.replyCh, c.replyTS, c.replyErr
}

func newReceiptAdapter(api SlackAPI) *Adapter {
	return &Adapter{client: api}
}

// TestSendWithReceipt_AcceptedCarriesAProviderRef pins the difference between
// "Slack created a message" and "someone read it": the receipt records the
// message reference and nothing stronger.
func TestSendWithReceipt_AcceptedCarriesAProviderRef(t *testing.T) {
	t.Parallel()

	api := &receiptAPI{replyCh: "C123", replyTS: "1717171717.000100"}
	a := newReceiptAdapter(api)

	res, err := a.SendWithReceipt(context.Background(), "done", map[string]string{"channel_id": "C123"})
	require.NoError(t, err)
	require.Equal(t, messaging.SendAccepted, res.Outcome)
	require.True(t, res.Accepted())
	require.Equal(t, "C123:1717171717.000100", res.ProviderRef)
	require.Equal(t, messaging.RejectionClass(""), res.RejectionClass)
}

func TestSendWithReceipt_HonoursThreadTS(t *testing.T) {
	t.Parallel()

	api := &receiptAPI{replyCh: "C123", replyTS: "1.2"}
	a := newReceiptAdapter(api)

	_, err := a.SendWithReceipt(context.Background(), "done", map[string]string{
		"channel_id": "C123",
		"thread_ts":  "1717000000.000200",
	})
	require.NoError(t, err)

	// The thread target travels in the request, not in the prompt text, so the
	// message lands where the operator configured instead of at channel root.
	_, values, err := slack.UnsafeApplyMsgOptions("token", api.channel, "https://slack.test/api/chat.postMessage",
		api.options...)
	require.NoError(t, err)
	require.Equal(t, "1717000000.000200", values.Get("thread_ts"))
}

// TestSendWithReceipt_MissingChannelNeverReachesProvider covers the other half
// of the contract: a Go error means nothing was sent, so the effect stays
// retryable.
func TestSendWithReceipt_MissingChannelNeverReachesProvider(t *testing.T) {
	t.Parallel()

	api := &receiptAPI{}
	a := newReceiptAdapter(api)

	_, err := a.SendWithReceipt(context.Background(), "done", map[string]string{})
	require.Error(t, err)
	require.False(t, api.called, "a missing target must not produce a provider call")
}

// TestSendWithReceipt_OutcomeClassification is the safety core: only failures
// that prove no message was created may be retried automatically.
func TestSendWithReceipt_OutcomeClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		providerErr   error
		wantOutcome   messaging.SendOutcome
		wantRejection messaging.RejectionClass
		wantRetry     bool
	}{
		{
			name:          "rate limited is an explicit refusal",
			providerErr:   &slack.RateLimitedError{RetryAfter: 30 * time.Second},
			wantOutcome:   messaging.SendRejected,
			wantRejection: messaging.RejectionSafeRetry,
			wantRetry:     true,
		},
		{
			name:          "channel not found is permanent",
			providerErr:   slack.SlackErrorResponse{Err: "channel_not_found"},
			wantOutcome:   messaging.SendRejected,
			wantRejection: messaging.RejectionPermanent,
		},
		{
			name:          "not in channel is permanent",
			providerErr:   slack.SlackErrorResponse{Err: "not_in_channel"},
			wantOutcome:   messaging.SendRejected,
			wantRejection: messaging.RejectionPermanent,
		},
		{
			name:        "server error leaves the commit unproven",
			providerErr: slack.StatusCodeError{Code: 503, Status: "Service Unavailable"},
			wantOutcome: messaging.SendUnknown,
		},
		{
			name:          "bad request is a refusal",
			providerErr:   slack.StatusCodeError{Code: 400, Status: "Bad Request"},
			wantOutcome:   messaging.SendRejected,
			wantRejection: messaging.RejectionPermanent,
		},
		{
			name:        "dropped connection leaves the commit unproven",
			providerErr: &net.OpError{Op: "dial", Err: net.ErrClosed},
			wantOutcome: messaging.SendUnknown,
		},
		{
			name:        "expired deadline leaves the commit unproven",
			providerErr: context.DeadlineExceeded,
			wantOutcome: messaging.SendUnknown,
		},
		{
			name:        "unrecognised failure is treated as the dangerous case",
			providerErr: errNotAProviderFailure{},
			wantOutcome: messaging.SendUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &receiptAPI{replyErr: tc.providerErr}
			a := newReceiptAdapter(api)

			res, err := a.SendWithReceipt(context.Background(), "done", map[string]string{"channel_id": "C1"})
			require.NoError(t, err, "a provider round-trip is not a Go error")
			require.Equal(t, tc.wantOutcome, res.Outcome)
			require.Equal(t, tc.wantRejection, res.RejectionClass)
			require.Equal(t, tc.wantRetry, res.RetryAfter > 0)
			require.Empty(t, res.ProviderRef, "an unaccepted send has no message reference")

			if tc.wantOutcome == messaging.SendUnknown {
				require.NotEqual(t, messaging.RejectionSafeRetry, res.RejectionClass,
					"unknown must never license an automatic resend")
			}
		})
	}
}

// TestDeliveryCapabilities_ClaimsOnlyWhatSlackProves guards against the
// capability set drifting into a promise the adapter cannot keep.
func TestDeliveryCapabilities_ClaimsOnlyWhatSlackProves(t *testing.T) {
	t.Parallel()

	caps := (&Adapter{}).DeliveryCapabilities()
	require.True(t, caps.Receipt, "chat.postMessage returns the created message reference")
	require.False(t, caps.Idempotency,
		"client_msg_id is not a durable deduplication guarantee")
	require.False(t, caps.Lookup,
		"this adapter exposes no message lookup, so unknown cannot be reconciled")
	require.Zero(t, caps.IdempotencyWindow)
}

func TestBoundedAPIError(t *testing.T) {
	t.Parallel()

	require.Equal(t, "channel_not_found", boundedAPIError("  channel_not_found  "))
	require.Equal(t, "slack refused the request", boundedAPIError(""))

	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	bounded := boundedAPIError(string(long))
	require.Len(t, bounded, 64, "provider text must not reach logs unbounded")
}

type errNotAProviderFailure struct{}

func (errNotAProviderFailure) Error() string { return "something else entirely" }
