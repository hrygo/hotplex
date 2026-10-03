package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/effect"
	"github.com/hrygo/hotplex/internal/messaging"
)

func slackTarget() (string, map[string]string) {
	return "slack", map[string]string{"channel_id": "C123"}
}

func newRecoveryDeliverer(
	store *fakeEffectStore,
	sender *recordingSender,
	platform string,
	platformKey map[string]string,
	resolveErr error,
) *EffectDeliverer {
	return NewEffectDeliverer(EffectDelivererConfig{
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Effects: store,
		SenderFor: func(string) (messaging.ControlledSender, bool) {
			if sender == nil {
				return nil, false
			}
			return sender, true
		},
		ResolveTarget: func(context.Context, string) (string, map[string]string, error) {
			if resolveErr != nil {
				return "", nil, resolveErr
			}
			return platform, platformKey, nil
		},
		OwnerInstanceID: "instance-a",
	})
}

func recoverableEffect(id string) *effect.Effect {
	return &effect.Effect{
		EffectID:       id,
		OccurrenceID:   "occ-1",
		TargetKind:     "slack",
		TargetRef:      "C123",
		TargetRevision: TargetRevision("slack", map[string]string{"channel_id": "C123"}),
		PayloadID:      "pay-1",
		Status:         effect.StatusPlanned,
	}
}

func withPayload(content string) *effect.Payload {
	return &effect.Payload{
		PayloadID:  "pay-1",
		Content:    content,
		ContentSHA: effect.ContentHash(content),
	}
}

// TestRecoverOnce_SendsWorkLeftBehindByARestart is the whole point of the
// pass: a committed intent nobody sent is finished, not abandoned.
func TestRecoverOnce_SendsWorkLeftBehindByARestart(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		recoverable: []*effect.Effect{recoverableEffect("eff-1")},
		payload:     withPayload("the final answer"),
	}
	sender := acceptedSender()
	platform, key := slackTarget()
	d := newRecoveryDeliverer(store, sender, platform, key, nil)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Sent)
	require.Equal(t, []string{"the final answer"}, sender.texts)
	require.Len(t, store.claims, 1)
}

// TestRecoverOnce_FencesBeforeSending keeps a lapsed lease out of the send
// path in the same pass that would otherwise pick it up as work.
func TestRecoverOnce_FencesBeforeSending(t *testing.T) {
	t.Parallel()

	expired := recoverableEffect("eff-expired")
	expired.Status = effect.StatusUnknown
	store := &fakeEffectStore{
		expired: []*effect.Effect{expired},
		payload: withPayload("the final answer"),
	}
	sender := acceptedSender()
	platform, key := slackTarget()
	d := newRecoveryDeliverer(store, sender, platform, key, nil)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.ExpiredLeases)
	require.Zero(t, report.Sent, "a fenced effect must never be sent in the same pass")
	require.Empty(t, sender.texts)
}

// TestRecoverOnce_RetriesOnlyAfterASafeRejection covers the retry leg: the
// attempt advances on the same effect, never into a second one.
func TestRecoverOnce_RetriesOnlyAfterASafeRejection(t *testing.T) {
	t.Parallel()

	due := recoverableEffect("eff-1")
	due.Status = effect.StatusStarted
	due.Attempt = 1
	store := &fakeEffectStore{
		dueRetries: []*effect.Effect{due},
		payload:    withPayload("the final answer"),
	}
	sender := acceptedSender()
	platform, key := slackTarget()
	d := newRecoveryDeliverer(store, sender, platform, key, nil)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Sent)
	require.Len(t, store.retries, 1)
	require.Equal(t, int64(1), store.retries[0].ExpectedAttempt)
	require.Equal(t, int64(3), store.retries[0].MaxAttempts)
	require.Empty(t, store.claims, "a retry is not a fresh claim")
}

// TestRecoverOnce_RefusesWhenTheTargetMoved is the stale-authorization guard:
// a promise made about one target must not be delivered to another.
func TestRecoverOnce_RefusesWhenTheTargetMoved(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		recoverable: []*effect.Effect{recoverableEffect("eff-1")},
		payload:     withPayload("the final answer"),
	}
	sender := acceptedSender()
	d := newRecoveryDeliverer(store, sender, "slack",
		map[string]string{"channel_id": "C999"}, nil)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Zero(t, report.Sent)
	require.Equal(t, 1, report.Deferred)
	require.Empty(t, sender.texts, "a changed target must never be delivered to")
	require.Empty(t, store.claims)
}

func TestRecoverOnce_RefusesWhenDeliveryIsNoLongerAuthorized(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		recoverable: []*effect.Effect{recoverableEffect("eff-1")},
		payload:     withPayload("the final answer"),
	}
	sender := acceptedSender()
	d := newRecoveryDeliverer(store, sender, "slack", nil, ErrTargetUnauthorized)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Deferred)
	require.Empty(t, sender.texts)
}

// TestRecoverOnce_ReportsMissingContentRatherThanSubstituting: a vanished
// payload is a gap in the record, not a reason to regenerate the answer.
func TestRecoverOnce_ReportsMissingContentRatherThanSubstituting(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		recoverable: []*effect.Effect{recoverableEffect("eff-1")},
	}
	sender := acceptedSender()
	platform, key := slackTarget()
	d := newRecoveryDeliverer(store, sender, platform, key, nil)

	report, err := d.RecoverOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Deferred)
	require.Empty(t, sender.texts)
	require.Empty(t, store.claims)
}

func TestRecoverOnce_IgnoresDecidedEffects(t *testing.T) {
	t.Parallel()

	for _, status := range []effect.Status{
		effect.StatusDelivered,
		effect.StatusFailed,
		effect.StatusUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			decided := recoverableEffect("eff-1")
			decided.Status = status
			store := &fakeEffectStore{
				recoverable: []*effect.Effect{decided},
				payload:     withPayload("the final answer"),
			}
			sender := acceptedSender()
			platform, key := slackTarget()
			d := newRecoveryDeliverer(store, sender, platform, key, nil)

			report, err := d.RecoverOnce(context.Background(), 10)
			require.NoError(t, err)
			require.Zero(t, report.Sent, "%s is decided and must not be re-sent", status)
			require.Equal(t, 1, report.Skipped)
		})
	}
}

func TestRecoverOnce_StopsWhenCancelled(t *testing.T) {
	t.Parallel()

	store := &fakeEffectStore{
		recoverable: []*effect.Effect{recoverableEffect("eff-1")},
		payload:     withPayload("the final answer"),
	}
	sender := acceptedSender()
	platform, key := slackTarget()
	d := newRecoveryDeliverer(store, sender, platform, key, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.RecoverOnce(ctx, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, sender.texts,
		"a shutting-down process must not dispatch new sends")
}
