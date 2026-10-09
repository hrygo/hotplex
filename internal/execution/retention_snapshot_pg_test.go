//go:build pg

package execution

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/effect"
)

// This test avoids t.Parallel because openPGStore resets the shared dedicated schema;
// PostgreSQL integration tests run with -p1.
func TestPGRetentionSnapshotSurvivesPolicyShortening(t *testing.T) {
	if os.Getenv("HOTPLEX_TEST_PG_DSN") == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG retention snapshot test")
	}
	store, db := openPGStore(t)
	ctx := context.Background()
	gcNow := time.Now().UTC().Truncate(time.Millisecond)
	oldFacts := 90 * 24 * time.Hour
	newFacts := 24 * time.Hour

	settleAccepted := func(record *Record, ownerID, runID string) {
		t.Helper()
		require.NoError(t, store.MarkRunning(ctx, record.ExecutionID, ownerID, runID))
		require.NoError(t, store.SetDelivery(ctx, record.ExecutionID, ownerID, StatusDelivered, ""))
		require.NoError(t, store.FinishRuntime(ctx, record.ExecutionID, runID, RuntimeCompleted, ""))
		_, err := db.ExecContext(ctx, `
			UPDATE execution_inputs
			SET finished_at = $1, runtime_error_code = 'retained-detail'
			WHERE execution_id = $2`, gcNow.Add(-2*24*time.Hour).UnixMilli(), record.ExecutionID)
		require.NoError(t, err)
	}
	accept := func(messageID, ownerID, runID string) (AcceptRequest, *Record) {
		t.Helper()
		request := AcceptRequest{
			SessionID:       "session-pg",
			ClientMessageID: messageID,
			PayloadHash:     messageID + "-hash",
			OwnerInstanceID: ownerID,
			WorkerRunID:     runID,
		}
		record, duplicate, err := store.Accept(ctx, request)
		require.NoError(t, err)
		require.False(t, duplicate)
		settleAccepted(record, ownerID, runID)
		return request, record
	}
	enqueue := func(messageID, ownerID, runID string) (QueuedRequest, *Record) {
		t.Helper()
		request := QueuedRequest{
			SessionID:       "session-pg",
			ClientMessageID: messageID,
			PayloadHash:     messageID + "-hash",
			Payload:         QueuedPayload{Content: "queued payload"},
			OwnerInstanceID: ownerID,
		}
		record, _, duplicate, err := store.AcceptQueued(ctx, request, DefaultQueueLimits())
		require.NoError(t, err)
		require.False(t, duplicate)
		return request, record
	}
	claimAndSettle := func(record *Record, ownerID, runID string) {
		t.Helper()
		claimed, _, err := store.ClaimQueued(ctx, ClaimQueuedRequest{
			SessionID:           record.SessionID,
			OwnerInstanceID:     ownerID,
			WorkerRunID:         runID,
			ExpectedExecutionID: record.ExecutionID,
		})
		require.NoError(t, err)
		require.Equal(t, record.ExecutionID, claimed.ExecutionID)
		settleAccepted(claimed, ownerID, runID)
	}

	store.SetRetentionPolicy(oldFacts, "old-policy")
	oldAcceptRequest, oldAccept := accept("pg-old-accept", "owner-old-accept", "run-old-accept")
	oldQueuedRequest, oldQueued := enqueue("pg-old-queued", "owner-old-queued", "run-old-queued")

	store.SetRetentionPolicy(newFacts, "new-policy")
	duplicate, wasDuplicate, err := store.Accept(ctx, oldAcceptRequest)
	require.NoError(t, err)
	require.True(t, wasDuplicate)
	require.Equal(t, oldAccept.ExecutionID, duplicate.ExecutionID)
	duplicateQueued, queueEntry, wasDuplicate, err := store.AcceptQueued(ctx, oldQueuedRequest, DefaultQueueLimits())
	require.NoError(t, err)
	require.True(t, wasDuplicate)
	require.Equal(t, oldQueued.ExecutionID, duplicateQueued.ExecutionID)
	require.NotNil(t, queueEntry)
	assertExecutionRetentionSnapshot(t, db, oldAccept.ExecutionID, oldFacts.Milliseconds(), "old-policy")
	assertExecutionRetentionSnapshot(t, db, oldQueued.ExecutionID, oldFacts.Milliseconds(), "old-policy")

	claimAndSettle(oldQueued, "owner-old-queued", "run-old-queued")
	_, newAccept := accept("pg-new-accept", "owner-new-accept", "run-new-accept")
	_, newQueued := enqueue("pg-new-queued", "owner-new-queued", "run-new-queued")
	claimAndSettle(newQueued, "owner-new-queued", "run-new-queued")

	store.SetRetentionPolicy(0, "legacy-policy")
	_, legacy := accept("pg-legacy", "owner-legacy", "run-legacy")
	_, err = db.ExecContext(ctx, `
		UPDATE execution_inputs
		SET finished_at = $1
		WHERE execution_id = $2`, gcNow.Add(-10*24*time.Hour).UnixMilli(), legacy.ExecutionID)
	require.NoError(t, err)

	assertExecutionRetentionSnapshot(t, db, newAccept.ExecutionID, newFacts.Milliseconds(), "new-policy")
	assertExecutionRetentionSnapshot(t, db, newQueued.ExecutionID, newFacts.Milliseconds(), "new-policy")
	assertExecutionRetentionSnapshot(t, db, legacy.ExecutionID, 0, "legacy-policy")

	compacted, err := store.CompactSettledFacts(ctx, gcNow, 20)
	require.NoError(t, err)
	require.EqualValues(t, 2, compacted, "only the two one-day records should be compacted")
	assertExecutionRuntimeDetail(t, db, oldAccept.ExecutionID, "retained-detail")
	assertExecutionRuntimeDetail(t, db, oldQueued.ExecutionID, "retained-detail")
	assertExecutionRuntimeDetail(t, db, newAccept.ExecutionID, "")
	assertExecutionRuntimeDetail(t, db, newQueued.ExecutionID, "")
	assertExecutionRuntimeDetail(t, db, legacy.ExecutionID, "retained-detail")

	// A duplicate accepted after compaction remains an idempotent read and must
	// not recreate facts or rewrite the original snapshot.
	duplicate, wasDuplicate, err = store.Accept(ctx, oldAcceptRequest)
	require.NoError(t, err)
	require.True(t, wasDuplicate)
	require.Equal(t, oldAccept.ExecutionID, duplicate.ExecutionID)
	duplicateQueued, _, wasDuplicate, err = store.AcceptQueued(ctx, oldQueuedRequest, DefaultQueueLimits())
	require.NoError(t, err)
	require.True(t, wasDuplicate)
	require.Equal(t, oldQueued.ExecutionID, duplicateQueued.ExecutionID)
	assertExecutionRetentionSnapshot(t, db, oldAccept.ExecutionID, oldFacts.Milliseconds(), "old-policy")
	assertExecutionRetentionSnapshot(t, db, oldQueued.ExecutionID, oldFacts.Milliseconds(), "old-policy")

	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	require.NotEmpty(t, dsn, "openPGStore should skip when the dedicated test DSN is absent")
	effectDB, err := dbutil.Open(dbutil.DialectPostgres, &config.DBConfig{
		Driver:   "postgres",
		Postgres: config.PostgresConfig{ConnStr: dsn},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = effectDB.Close() })
	effectStore := effect.NewPGStore(effectDB, slog.Default())

	settleEffect := func(occurrenceID, executionID, content, revision string, payloadWindow, factsWindow time.Duration, settledAt time.Time) *effect.Effect {
		t.Helper()
		effectStore.SetRetentionPolicy(payloadWindow, factsWindow, revision)
		plan := effect.Plan{
			OccurrenceID:    occurrenceID,
			DeliveryOrdinal: 1,
			TargetRevision:  "target-v1",
			SessionID:       "session-pg",
			ExecutionID:     executionID,
			WorkerRunID:     "run-" + occurrenceID,
			Content:         content,
			TargetKind:      "slack",
			TargetRef:       "channel-test",
		}
		planAt := settledAt.Add(-time.Hour)
		planned, created, err := effectStore.PlanOnce(ctx, plan, planAt)
		require.NoError(t, err)
		require.True(t, created)
		claim, err := effectStore.ClaimSend(ctx, effect.ClaimRequest{
			EffectID:        planned.EffectID,
			OwnerInstanceID: "effect-owner",
			LeaseUntil:      planAt.Add(time.Hour),
			ExpectedAttempt: 0,
			Now:             planAt,
		})
		require.NoError(t, err)
		require.NoError(t, effectStore.CompleteSend(ctx, effect.Completion{
			EffectID:     planned.EffectID,
			Attempt:      claim.Attempt,
			LeaseToken:   claim.LeaseToken,
			LeaseVersion: claim.LeaseVersion,
			Outcome:      effect.AttemptAccepted,
			ProviderRef:  "receipt-" + occurrenceID,
			Now:          settledAt,
		}))
		return planned
	}

	oldPayloadWindow := 7 * 24 * time.Hour
	oldEffect := settleEffect(
		"pg-old-effect", oldAccept.ExecutionID, "old effect payload", "old-policy",
		oldPayloadWindow, oldFacts, gcNow.Add(-2*24*time.Hour),
	)
	oldEffectPlan := effect.Plan{
		OccurrenceID:    "pg-old-effect",
		DeliveryOrdinal: 1,
		TargetRevision:  "target-v1",
		SessionID:       "session-pg",
		ExecutionID:     oldAccept.ExecutionID,
		WorkerRunID:     "run-pg-old-effect",
		Content:         "old effect payload",
		TargetKind:      "slack",
		TargetRef:       "channel-test",
	}
	effectStore.SetRetentionPolicy(time.Hour, time.Hour, "shorter-policy")
	retriedEffect, created, err := effectStore.PlanOnce(ctx, oldEffectPlan, gcNow)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, oldEffect.EffectID, retriedEffect.EffectID)
	require.Equal(t, oldEffect.PayloadID, retriedEffect.PayloadID)

	newEffect := settleEffect(
		"pg-new-effect", newAccept.ExecutionID, "new effect payload", "new-policy",
		24*time.Hour, 24*time.Hour, gcNow.Add(-2*24*time.Hour),
	)
	legacyEffect := settleEffect(
		"pg-legacy-effect", legacy.ExecutionID, "legacy effect payload", "legacy-policy",
		0, 0, gcNow.Add(-10*24*time.Hour),
	)
	assertEffectRetentionSnapshot(t, db, oldEffect.EffectID, oldPayloadWindow.Milliseconds(), oldFacts.Milliseconds(), "old-policy")
	assertEffectRetentionSnapshot(t, db, newEffect.EffectID, (24 * time.Hour).Milliseconds(), (24 * time.Hour).Milliseconds(), "new-policy")
	assertEffectRetentionSnapshot(t, db, legacyEffect.EffectID, 0, 0, "legacy-policy")

	expiredPayloads, err := effectStore.DeleteExpiredPayloads(ctx, gcNow, 20)
	require.NoError(t, err)
	require.EqualValues(t, 1, expiredPayloads, "only the one-day payload should be removed")
	expiredAttempts, err := effectStore.DeleteSettledAttempts(ctx, gcNow, 20)
	require.NoError(t, err)
	require.EqualValues(t, 1, expiredAttempts, "only the one-day attempt should be removed")

	oldPayload, err := effectStore.GetPayload(ctx, oldEffect.PayloadID)
	require.NoError(t, err)
	require.Equal(t, "old effect payload", oldPayload.Content)
	_, err = effectStore.GetPayload(ctx, newEffect.PayloadID)
	require.ErrorIs(t, err, effect.ErrPayloadNotFound)
	legacyPayload, err := effectStore.GetPayload(ctx, legacyEffect.PayloadID)
	require.NoError(t, err)
	require.Equal(t, "legacy effect payload", legacyPayload.Content)

	oldAttempts, err := effectStore.ListAttempts(ctx, oldEffect.EffectID)
	require.NoError(t, err)
	require.Len(t, oldAttempts, 1)
	newAttempts, err := effectStore.ListAttempts(ctx, newEffect.EffectID)
	require.NoError(t, err)
	require.Empty(t, newAttempts)
	legacyAttempts, err := effectStore.ListAttempts(ctx, legacyEffect.EffectID)
	require.NoError(t, err)
	require.Len(t, legacyAttempts, 1)
	assertEffectRetentionSnapshot(t, db, oldEffect.EffectID, oldPayloadWindow.Milliseconds(), oldFacts.Milliseconds(), "old-policy")
}

func assertExecutionRetentionSnapshot(
	t *testing.T, db *sql.DB, executionID string, wantWindowMS int64, wantRevision string,
) {
	t.Helper()
	var window sql.NullInt64
	var revision string
	err := db.QueryRow(
		`SELECT facts_retention_ms, retention_policy_revision FROM execution_inputs WHERE execution_id = $1`,
		executionID,
	).Scan(&window, &revision)
	require.NoError(t, err)
	if wantWindowMS == 0 {
		require.False(t, window.Valid)
	} else {
		require.True(t, window.Valid)
		require.Equal(t, wantWindowMS, window.Int64)
	}
	require.Equal(t, wantRevision, revision)
}

func assertExecutionRuntimeDetail(t *testing.T, db *sql.DB, executionID, want string) {
	t.Helper()
	var detail string
	err := db.QueryRow(`SELECT runtime_error_code FROM execution_inputs WHERE execution_id = $1`, executionID).Scan(&detail)
	require.NoError(t, err)
	require.Equal(t, want, detail)
}

func assertEffectRetentionSnapshot(
	t *testing.T, db *sql.DB, effectID string, wantPayloadMS, wantFactsMS int64, wantRevision string,
) {
	t.Helper()
	var payloadWindow, factsWindow sql.NullInt64
	var revision string
	err := db.QueryRow(
		`SELECT payload_retention_ms, facts_retention_ms, retention_policy_revision FROM effects WHERE effect_id = $1`,
		effectID,
	).Scan(&payloadWindow, &factsWindow, &revision)
	require.NoError(t, err)
	if wantPayloadMS == 0 {
		require.False(t, payloadWindow.Valid)
	} else {
		require.True(t, payloadWindow.Valid)
		require.Equal(t, wantPayloadMS, payloadWindow.Int64)
	}
	if wantFactsMS == 0 {
		require.False(t, factsWindow.Valid)
	} else {
		require.True(t, factsWindow.Valid)
		require.Equal(t, wantFactsMS, factsWindow.Int64)
	}
	require.Equal(t, wantRevision, revision)
}
