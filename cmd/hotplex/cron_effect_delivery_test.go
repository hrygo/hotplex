package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/gateway"
)

type stubOccurrences struct {
	cron.OccurrenceStore
	occ *cron.Occurrence
	err error
}

func (s *stubOccurrences) GetByID(context.Context, string) (*cron.Occurrence, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.occ, nil
}

type stubJobs struct {
	cron.Store
	job *cron.CronJob
	err error
}

func (s *stubJobs) Get(context.Context, string) (*cron.CronJob, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.job, nil
}

func gatewayJob() *cron.CronJob {
	return &cron.CronJob{
		ID:           "job-1",
		Platform:     "slack",
		PlatformKey:  map[string]string{"channel_id": "C123"},
		DeliveryMode: cron.DeliveryModeGateway,
	}
}

func TestResolveDeliveryTarget(t *testing.T) {
	t.Parallel()

	occurrence := &cron.Occurrence{OccurrenceID: "occ-1", JobID: "job-1"}

	t.Run("authorised gateway job resolves to its live target", func(t *testing.T) {
		t.Parallel()

		platform, key, err := resolveDeliveryTarget(context.Background(),
			&stubOccurrences{occ: occurrence}, &stubJobs{job: gatewayJob()}, "occ-1")
		require.NoError(t, err)
		require.Equal(t, "slack", platform)
		require.Equal(t, "C123", key["channel_id"])
	})

	// Recovery must re-check authorisation rather than trusting what was
	// recorded when the intent was planned.
	t.Run("a job switched back to the legacy owner is refused", func(t *testing.T) {
		t.Parallel()

		job := gatewayJob()
		job.DeliveryMode = cron.DeliveryModeLegacyCLI
		_, _, err := resolveDeliveryTarget(context.Background(),
			&stubOccurrences{occ: occurrence}, &stubJobs{job: job}, "occ-1")
		require.ErrorIs(t, err, gateway.ErrTargetUnauthorized)
	})

	t.Run("a silent job is refused", func(t *testing.T) {
		t.Parallel()

		job := gatewayJob()
		job.Silent = true
		_, _, err := resolveDeliveryTarget(context.Background(),
			&stubOccurrences{occ: occurrence}, &stubJobs{job: job}, "occ-1")
		require.ErrorIs(t, err, gateway.ErrTargetUnauthorized)
	})

	t.Run("a deleted job is refused", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolveDeliveryTarget(context.Background(),
			&stubOccurrences{occ: occurrence},
			&stubJobs{err: cron.ErrJobNotFound}, "occ-1")
		require.ErrorIs(t, err, gateway.ErrTargetUnauthorized)
	})

	t.Run("an unknown occurrence is refused", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolveDeliveryTarget(context.Background(),
			&stubOccurrences{err: cron.ErrOccurrenceNotFound},
			&stubJobs{job: gatewayJob()}, "occ-missing")
		require.ErrorIs(t, err, gateway.ErrTargetUnauthorized)
	})

	t.Run("missing stores refuse rather than assume", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolveDeliveryTarget(context.Background(), nil, nil, "occ-1")
		require.ErrorIs(t, err, gateway.ErrTargetUnauthorized)
	})
}

func TestAdapterLookup_SkipsAdaptersWithoutReceipts(t *testing.T) {
	t.Parallel()

	lookup := &adapterLookup{}
	_, ok := lookup.senderFor("slack")
	require.False(t, ok, "no adapters yet means no sender")

	lookup.set()
	_, ok = lookup.senderFor("slack")
	require.False(t, ok)
}
