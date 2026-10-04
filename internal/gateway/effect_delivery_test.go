package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/messaging"
)

// fakeEffectStore records the sequence of ledger operations so a test can
// assert not just the outcome but the ORDER that makes it safe.
type fakeEffectStore struct {
	planned   []*effect.Plan
	claims    []effect.ClaimRequest
	completes []effect.Completion
	retries   []effect.RetryRequest

	expired     []*effect.Effect
	recoverable []*effect.Effect
	dueRetries  []*effect.Effect
	payload     *effect.Payload

	planErr      error
	claimErr     error
	completeErr  error
	existing     *effect.Effect
	existingUsed bool
}

func (f *fakeEffectStore) PlanOnce(
	_ context.Context, plan effect.Plan, _ time.Time,
) (*effect.Effect, bool, error) {
	if f.planErr != nil {
		return nil, false, f.planErr
	}
	f.planned = append(f.planned, &plan)
	if f.existingUsed {
		return f.existing, false, nil
	}
	return &effect.Effect{
		EffectID:        "eff-1",
		OccurrenceID:    plan.OccurrenceID,
		DeliveryOrdinal: plan.DeliveryOrdinal,
		TargetRevision:  plan.TargetRevision,
		Status:          effect.StatusPlanned,
	}, true, nil
}

func (f *fakeEffectStore) ClaimSend(
	_ context.Context, req effect.ClaimRequest,
) (*effect.Claim, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	f.claims = append(f.claims, req)
	return &effect.Claim{
		Effect: &effect.Effect{
			EffectID:        req.EffectID,
			Attempt:         req.ExpectedAttempt,
			LeaseVersion:    1,
			Status:          effect.StatusStarted,
			OwnerInstanceID: req.OwnerInstanceID,
		},
		Attempt:      req.ExpectedAttempt,
		LeaseVersion: 1,
		LeaseToken:   "ltk-test",
	}, nil
}

func (f *fakeEffectStore) CompleteSend(_ context.Context, c effect.Completion) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	f.completes = append(f.completes, c)
	return nil
}

func (f *fakeEffectStore) GetByKey(
	context.Context, string, int64, string,
) (*effect.Effect, error) {
	return nil, effect.ErrEffectNotFound
}

func (f *fakeEffectStore) GetByID(context.Context, string) (*effect.Effect, error) {
	return nil, effect.ErrEffectNotFound
}

func (f *fakeEffectStore) GetPayload(context.Context, string) (*effect.Payload, error) {
	if f.payload != nil {
		return f.payload, nil
	}
	return nil, effect.ErrPayloadNotFound
}

func (f *fakeEffectStore) GetPayloadForExecution(
	context.Context, string, string,
) (*effect.Payload, error) {
	return nil, effect.ErrPayloadNotFound
}

func (f *fakeEffectStore) ClaimRetry(
	_ context.Context, req effect.RetryRequest,
) (*effect.Claim, error) {
	f.retries = append(f.retries, req)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return &effect.Claim{
		Effect: &effect.Effect{
			EffectID:     req.EffectID,
			Attempt:      req.ExpectedAttempt + 1,
			LeaseVersion: 2,
			Status:       effect.StatusStarted,
		},
		Attempt:      req.ExpectedAttempt + 1,
		LeaseVersion: 2,
		LeaseToken:   "ltk-retry",
	}, nil
}

func (f *fakeEffectStore) ExpireLeases(
	context.Context, time.Time, int,
) ([]*effect.Effect, error) {
	return f.expired, nil
}

func (f *fakeEffectStore) ListRecoverable(context.Context, int) ([]*effect.Effect, error) {
	return f.recoverable, nil
}

func (f *fakeEffectStore) ListAttempts(
	context.Context, string,
) ([]*effect.Attempt, error) {
	return nil, nil
}

func (f *fakeEffectStore) ListDueRetries(
	context.Context, time.Time, int64, int,
) ([]*effect.Effect, error) {
	return f.dueRetries, nil
}

// ListForOperator and ApplyOperatorAction are the operator-console half of the
// ledger interface. The delivery path under test never calls them, so the fake
// answers plainly instead of inventing state.
func (f *fakeEffectStore) ListForOperator(
	context.Context, effect.OperatorListFilter,
) ([]*effect.Effect, error) {
	return nil, nil
}

func (f *fakeEffectStore) ApplyOperatorAction(
	context.Context, effect.OperatorActionRequest,
) (*effect.Effect, error) {
	return nil, effect.ErrEffectNotFound
}

// recordingSender captures the sends that actually reached the provider.
type recordingSender struct {
	result  messaging.SendResult
	err     error
	texts   []string
	caps    messaging.ProviderCapabilities
	refused bool
}

func (s *recordingSender) SendWithReceipt(
	_ context.Context, text string, _ map[string]string,
) (messaging.SendResult, error) {
	if s.refused {
		return messaging.SendResult{}, errors.New("slack: missing channel_id in platform_key")
	}
	s.texts = append(s.texts, text)
	return s.result, s.err
}

func (s *recordingSender) DeliveryCapabilities() messaging.ProviderCapabilities {
	return s.caps
}

func testRequest() cron.EffectDeliveryRequest {
	return cron.EffectDeliveryRequest{
		JobID:        "job-1",
		JobName:      "daily report",
		OccurrenceID: "occ-1",
		SessionID:    "sess-1",
		ExecutionID:  "exec-1",
		Platform:     "slack",
		PlatformKey:  map[string]string{"channel_id": "C123"},
	}
}

func newTestDeliverer(
	store effect.Store, sender *recordingSender, content string, extractErr error,
) *EffectDeliverer {
	return NewEffectDeliverer(EffectDelivererConfig{
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Effects: store,
		Extract: func(context.Context, string, string) (string, error) {
			return content, extractErr
		},
		SenderFor: func(string) (messaging.ControlledSender, bool) {
			if sender == nil {
				return nil, false
			}
			return sender, true
		},
		OwnerInstanceID: "instance-a",
	})
}

func acceptedSender() *recordingSender {
	return &recordingSender{result: messaging.SendResult{
		Outcome:     messaging.SendAccepted,
		ProviderRef: "C123:1717171717.000100",
	}}
}

func TestDeliverEffect_RecordsIntentBeforeSending(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	sender := acceptedSender()
	d := newTestDeliverer(store, sender, "the final answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))

	require.Len(t, store.planned, 1)
	require.Equal(t, "the final answer", store.planned[0].Content)
	require.Equal(t, "occ-1", store.planned[0].OccurrenceID)
	require.Equal(t, "exec-1", store.planned[0].ExecutionID)
	require.Equal(t, "slack", store.planned[0].TargetKind)
	require.Equal(t, "C123", store.planned[0].TargetRef)
	require.Equal(t, []string{"the final answer"}, sender.texts)

	require.Len(t, store.claims, 1)
	require.Equal(t, "instance-a", store.claims[0].OwnerInstanceID)
	require.Equal(t, int64(0), store.claims[0].ExpectedAttempt)

	require.Len(t, store.completes, 1)
	require.Equal(t, effect.AttemptAccepted, store.completes[0].Outcome)
	require.Equal(t, "C123:1717171717.000100", store.completes[0].ProviderRef)
}

// TestDeliverEffect_PlanFailureStopsBeforeSending is the ordering guarantee
// that matters most: if the intent cannot be committed, nothing is sent.
func TestDeliverEffect_PlanFailureStopsBeforeSending(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{planErr: effect.ErrPayloadTooLarge}
	sender := acceptedSender()
	d := newTestDeliverer(store, sender, "the final answer", nil)

	err := d.DeliverEffect(context.Background(), testRequest())
	require.ErrorIs(t, err, effect.ErrPayloadTooLarge)
	require.Empty(t, sender.texts, "nothing may be sent when the intent is not durable")
	require.Empty(t, store.claims)
}

func TestDeliverEffect_NeverSendsWithoutAnAnswerOrATarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		mutateReq func(*cron.EffectDeliveryRequest)
		wantIs    error
	}{
		{
			name:    "empty final output",
			content: "   ",
			wantIs:  ErrNoFinalOutput,
		},
		{
			name:    "no channel configured",
			content: "answer",
			mutateReq: func(r *cron.EffectDeliveryRequest) {
				r.PlatformKey = map[string]string{}
			},
			wantIs: effect.ErrMissingTarget,
		},
		{
			name:    "self originated job",
			content: "answer",
			mutateReq: func(r *cron.EffectDeliveryRequest) {
				r.Platform = "cron"
			},
			wantIs: effect.ErrMissingTarget,
		},
		{
			name:    "missing execution",
			content: "answer",
			mutateReq: func(r *cron.EffectDeliveryRequest) {
				r.ExecutionID = ""
			},
			wantIs: effect.ErrMissingIdentity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeEffectStore{}
			sender := acceptedSender()
			d := newTestDeliverer(store, sender, tc.content, nil)
			req := testRequest()
			if tc.mutateReq != nil {
				tc.mutateReq(&req)
			}

			err := d.DeliverEffect(context.Background(), req)
			require.ErrorIs(t, err, tc.wantIs)
			require.Empty(t, sender.texts)
		})
	}
}

// TestDeliverEffect_UnprovableOutcomeIsRecordedAsUnknown is what keeps a lost
// response from becoming a duplicate message.
func TestDeliverEffect_UnprovableOutcomeIsRecordedAsUnknown(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	sender := &recordingSender{result: messaging.SendResult{
		Outcome: messaging.SendUnknown,
		Reason:  "send deadline elapsed; the commit is unproven",
	}}
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Len(t, store.completes, 1)
	require.Equal(t, effect.AttemptUnknown, store.completes[0].Outcome)
	require.Empty(t, store.completes[0].ProviderRef)
}

func TestDeliverEffect_RecordsPermanentRejection(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	sender := &recordingSender{result: messaging.SendResult{
		Outcome:        messaging.SendRejected,
		RejectionClass: messaging.RejectionPermanent,
		Reason:         "channel_not_found",
	}}
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Len(t, store.completes, 1)
	require.Equal(t, effect.AttemptRejected, store.completes[0].Outcome)
	require.Equal(t, string(messaging.RejectionPermanent), store.completes[0].ErrorCode)
}

// TestDeliverEffect_SafeRejectionSchedulesARetry keeps "later" distinct from
// "never": the effect stays owed behind a backoff instead of being failed.
func TestDeliverEffect_SafeRejectionSchedulesARetry(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	sender := &recordingSender{result: messaging.SendResult{
		Outcome:        messaging.SendRejected,
		RejectionClass: messaging.RejectionSafeRetry,
		RetryAfter:     90 * time.Second,
		Reason:         "slack rate limited the request",
	}}
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Len(t, store.completes, 1)
	require.Equal(t, effect.AttemptRejected, store.completes[0].Outcome)
	require.Equal(t, string(messaging.RejectionSafeRetry), store.completes[0].RejectionClass)
	require.Equal(t, 90*time.Second, store.completes[0].RetryAfter,
		"the provider's own hint wins over our shorter backoff")
}

// TestDeliverEffect_UnsentRequestIsNotUnknown pins the other half of the
// contract: a request that never left the process cannot have committed, so
// it must not be filed as an ambiguous outcome.
func TestDeliverEffect_UnsentRequestIsNotUnknown(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	sender := &recordingSender{refused: true}
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Len(t, store.completes, 1)
	require.Equal(t, effect.AttemptNotSent, store.completes[0].Outcome)
	require.Equal(t, "not_sent", store.completes[0].ErrorCode)
}

// TestDeliverEffect_ConvergesOnAnAlreadyDecidedEffect covers recovery: a
// restart after a successful send must not send the message again.
func TestDeliverEffect_ConvergesOnAnAlreadyDecidedEffect(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		existingUsed: true,
		existing: &effect.Effect{
			EffectID:     "eff-1",
			OccurrenceID: "occ-1",
			Status:       effect.StatusDelivered,
		},
	}
	sender := acceptedSender()
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Empty(t, sender.texts, "a decided effect must never be sent again")
	require.Empty(t, store.claims)
}

// TestDeliverEffect_LostClaimSendsNothing keeps two instances from both
// sending: the loser of the claim must stand down.
func TestDeliverEffect_LostClaimSendsNothing(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{claimErr: effect.ErrLeaseLost}
	sender := acceptedSender()
	d := newTestDeliverer(store, sender, "answer", nil)

	require.NoError(t, d.DeliverEffect(context.Background(), testRequest()))
	require.Empty(t, sender.texts)
	require.Empty(t, store.completes)
}

func TestDeliverEffect_NoReceiptCapableSenderIsAnExplicitRefusal(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{}
	d := newTestDeliverer(store, nil, "answer", nil)

	err := d.DeliverEffect(context.Background(), testRequest())
	require.ErrorIs(t, err, ErrNoControlledSender)
	require.Empty(t, store.planned, "nothing may be planned without an owner that can report")
}

func TestTargetRevision(t *testing.T) {
	t.Parallel()

	base := map[string]string{"channel_id": "C123", "thread_ts": "1.2"}
	rev := TargetRevision("slack", base)

	require.Equal(t, rev, TargetRevision("slack", base),
		"the same target must fingerprint identically")
	require.NotEqual(t, rev, TargetRevision("slack", map[string]string{
		"channel_id": "C123", "thread_ts": "9.9",
	}), "editing the target yields a new effect")
	require.NotEqual(t, rev, TargetRevision("feishu", base))

	// Map iteration order must not change the fingerprint.
	shuffled := map[string]string{}
	for k, v := range base {
		shuffled[k] = v
	}
	require.Equal(t, rev, TargetRevision("slack", shuffled))
}

func TestSelectFinalAssistantOutput(t *testing.T) {
	t.Parallel()

	failed := false
	success := true

	tests := []struct {
		name    string
		turns   []*eventstore.TurnRecord
		want    string
		wantErr error
	}{
		{
			name: "picks the newest assistant answer",
			turns: []*eventstore.TurnRecord{
				{Role: "user", Content: "run the report"},
				{Role: "assistant", Content: "older answer"},
				{Role: "user", Content: "and again"},
				{Role: "assistant", Content: "newest answer"},
			},
			want: "newest answer",
		},
		{
			name: "a trailing user turn is not an answer",
			turns: []*eventstore.TurnRecord{
				{Role: "assistant", Content: "the answer"},
				{Role: "user", Content: "follow-up question"},
			},
			want: "the answer",
		},
		{
			name: "a synthetic crash marker is not an answer",
			turns: []*eventstore.TurnRecord{
				{Role: "assistant", Content: "real answer"},
				{Role: "assistant", Content: "worker crashed", Source: eventstore.SourceCrash},
			},
			want: "real answer",
		},
		{
			name: "a timeout marker is not an answer",
			turns: []*eventstore.TurnRecord{
				{Role: "assistant", Content: "timed out", Source: eventstore.SourceTimeout},
			},
			wantErr: ErrNoFinalOutput,
		},
		{
			name: "a failed turn is not an answer",
			turns: []*eventstore.TurnRecord{
				{Role: "assistant", Content: "partial output", Success: &failed},
				{Role: "assistant", Content: "good answer", Success: &success},
			},
			want: "good answer",
		},
		{
			name: "whitespace is not an answer",
			turns: []*eventstore.TurnRecord{
				{Role: "assistant", Content: "   \n "},
			},
			wantErr: ErrNoFinalOutput,
		},
		{
			name:    "no turns at all",
			turns:   nil,
			wantErr: ErrNoFinalOutput,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := SelectFinalAssistantOutput(tc.turns)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// Fault window from the plan's failure matrix: the provider accepted the
// message, but writing the receipt back failed. The message exists; the ledger
// does not say so.
//
// This is the one window where the ledger's own write is what broke, so it is
// the one place a retry would turn an honest unknown into a duplicate delivery.
// The failure must surface to the caller, and the send must be treated as spent:
// the store is left with the effect in-flight, which expiry turns to unknown and
// never back to unsent work (internal/effect TestExpireLeases_BecomeUnknownNotSendable).
func TestDeliverEffect_LostWriteBackAfterSendSurfacesAndDoesNotResend(t *testing.T) {
	t.Parallel()

	writeBackErr := errors.New("ledger write failed")
	store := &fakeEffectStore{completeErr: writeBackErr}
	sender := &recordingSender{result: messaging.SendResult{
		Outcome:     messaging.SendAccepted,
		ProviderRef: "C123.1700000000.001",
	}}
	d := newTestDeliverer(store, sender, "answer", nil)

	err := d.DeliverEffect(context.Background(), testRequest())

	// The caller is told the outcome could not be recorded. Swallowing this
	// would let a scheduler treat the delivery as finished.
	require.Error(t, err, "a lost write-back must surface, not be swallowed")
	require.ErrorIs(t, err, writeBackErr)

	// Exactly one send. Nothing in this path may re-enter the sender.
	require.Len(t, sender.texts, 1, "the provider was called once and must stay called once")
	require.Equal(t, []string{"answer"}, sender.texts)

	// And nothing was recorded, which is precisely why it must not be read as
	// delivered: the effect is still in-flight, not accepted.
	require.Empty(t, store.completes,
		"no completion was persisted; the effect must remain in-flight for expiry to fence")
	require.Len(t, store.planned, 1, "one occurrence produces one effect, not a second one")
}
