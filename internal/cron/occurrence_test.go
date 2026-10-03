package cron

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/sqlutil"
)

func newOccurrenceStore(t *testing.T) *SQLiteOccurrenceStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "occurrence.db")
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA busy_timeout=5000`)
	require.NoError(t, err)

	// Mirrors goose migration 033 (SQLite dialect).
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cron_occurrences (
			occurrence_id   TEXT PRIMARY KEY,
			trigger_key     TEXT NOT NULL,
			generation      INTEGER NOT NULL DEFAULT 0,
			job_id          TEXT NOT NULL,
			trigger_kind    TEXT NOT NULL CHECK(trigger_kind IN ('scheduled', 'manual', 'webhook')),
			schedule_rev    TEXT NOT NULL DEFAULT '',
			scheduled_at_ms INTEGER NOT NULL DEFAULT 0,
			nonce           TEXT NOT NULL DEFAULT '',
			source_id       TEXT NOT NULL DEFAULT '',
			session_id      TEXT NOT NULL DEFAULT '',
			execution_id    TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL CHECK(status IN ('accepted', 'started', 'completed', 'failed', 'unknown')),
			error_code      TEXT NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL,
			started_at      INTEGER,
			finished_at     INTEGER,
			UNIQUE(trigger_key, generation)
		)`)
	require.NoError(t, err)

	return NewSQLiteOccurrenceStore(db, slog.Default(), nil)
}

func TestTriggerIdentityKey(t *testing.T) {
	t.Parallel()

	rev := ScheduleRevision(CronSchedule{Kind: ScheduleEvery, EveryMs: 60_000})

	tests := []struct {
		name     string
		identity TriggerIdentity
		want     string
		wantErr  bool
	}{
		{
			name: "scheduled key carries job, revision and instant",
			identity: TriggerIdentity{
				Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: rev,
				ScheduledAtMs: 1_700_000_000_000,
			},
			want: "sched|job-1|" + rev + "|1700000000000",
		},
		{
			name: "manual key carries the request nonce",
			identity: TriggerIdentity{
				Kind: TriggerManual, JobID: "job-1", Nonce: "nonce-abc",
			},
			want: "manual|job-1|nonce-abc",
		},
		{
			name: "webhook key carries the verified source id",
			identity: TriggerIdentity{
				Kind: TriggerWebhook, JobID: "job-1", SourceID: "evt-99",
			},
			want: "webhook|job-1|evt-99",
		},
		{
			name: "scheduled firing without an instant is rejected",
			identity: TriggerIdentity{
				Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: rev,
			},
			wantErr: true,
		},
		{
			name: "manual firing without a nonce is rejected",
			identity: TriggerIdentity{
				Kind: TriggerManual, JobID: "job-1",
			},
			wantErr: true,
		},
		{
			name: "webhook without a verified source id cannot claim dedup",
			identity: TriggerIdentity{
				Kind: TriggerWebhook, JobID: "job-1",
			},
			wantErr: true,
		},
		{
			name:     "missing job id is rejected",
			identity: TriggerIdentity{Kind: TriggerManual, Nonce: "n"},
			wantErr:  true,
		},
		{
			name:     "unknown trigger kind is rejected",
			identity: TriggerIdentity{Kind: "telepathy", JobID: "job-1", Nonce: "n"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.identity.Key()
			if tt.wantErr {
				require.ErrorIs(t, err, ErrTriggerIdentityIncomplete)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTriggerIdentityKeyIsStableAcrossEquivalentIdentities(t *testing.T) {
	t.Parallel()

	identity := TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}
	first, err := identity.Key()
	require.NoError(t, err)
	second, err := identity.Key()
	require.NoError(t, err)
	require.Equal(t, first, second, "the same firing must always derive the same key")
}

func TestScheduleRevisionChangesWithSchedule(t *testing.T) {
	t.Parallel()

	base := CronSchedule{Kind: ScheduleCron, Expr: "0 9 * * *", TZ: "UTC"}
	same := ScheduleRevision(base)
	require.Equal(t, same, ScheduleRevision(base), "revision must be deterministic")

	edited := base
	edited.Expr = "0 10 * * *"
	require.NotEqual(t, same, ScheduleRevision(edited),
		"an edited schedule must not collide with the pre-edit revision")
}

func TestOccurrenceClaimDeduplicatesSameTrigger(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	ctx := context.Background()
	identity := TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}

	first, err := NewOccurrence(identity, time.UnixMilli(1_700_000_000_000))
	require.NoError(t, err)
	stored, created, err := store.Claim(ctx, first)
	require.NoError(t, err)
	require.True(t, created, "the first trigger creates the occurrence")
	require.Equal(t, first.OccurrenceID, stored.OccurrenceID)

	// A repeated trigger of the same firing resolves to the same occurrence
	// instead of producing a second run.
	second, err := NewOccurrence(identity, time.UnixMilli(1_700_000_000_500))
	require.NoError(t, err)
	stored2, created2, err := store.Claim(ctx, second)
	require.NoError(t, err)
	require.False(t, created2, "a duplicate trigger must not create a second occurrence")
	require.Equal(t, stored.OccurrenceID, stored2.OccurrenceID)
}

func TestOccurrenceRerunUsesExplicitGeneration(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	ctx := context.Background()
	identity := TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}

	original, err := NewOccurrence(identity, time.Now())
	require.NoError(t, err)
	_, created, err := store.Claim(ctx, original)
	require.NoError(t, err)
	require.True(t, created)

	// An operator rerun keeps the trigger key but advances the generation, so
	// it is an explicit new run rather than a mutated idempotency key.
	rerun, err := NewOccurrence(identity, time.Now())
	require.NoError(t, err)
	rerun.Generation = 1
	stored, created, err := store.Claim(ctx, rerun)
	require.NoError(t, err)
	require.True(t, created, "a new generation is a new run")
	require.Equal(t, original.TriggerKey, stored.TriggerKey)
	require.Equal(t, int64(1), stored.Generation)
	require.NotEqual(t, original.OccurrenceID, stored.OccurrenceID)
}

func TestOccurrenceClaimIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	identity := TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_000_000,
	}

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		created   int
		ids       = make(map[string]struct{}, racers)
		claimErrs []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			occ, err := NewOccurrence(identity, time.Now())
			if err != nil {
				mu.Lock()
				claimErrs = append(claimErrs, err)
				mu.Unlock()
				return
			}
			stored, wasCreated, err := store.Claim(context.Background(), occ)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				claimErrs = append(claimErrs, err)
				return
			}
			if wasCreated {
				created++
			}
			ids[stored.OccurrenceID] = struct{}{}
		}()
	}
	wg.Wait()

	require.Empty(t, claimErrs, "concurrent claims must not error")
	require.Equal(t, 1, created, "exactly one concurrent trigger may create the occurrence")
	require.Len(t, ids, 1, "all racers must observe the same occurrence")
}

func TestOccurrenceUpdateStatusStampsLifecycle(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	ctx := context.Background()
	identity := TriggerIdentity{
		Kind: TriggerWebhook, JobID: "job-1", SourceID: "evt-1",
	}
	occ, err := NewOccurrence(identity, time.UnixMilli(1_700_000_000_000))
	require.NoError(t, err)
	_, created, err := store.Claim(ctx, occ)
	require.NoError(t, err)
	require.True(t, created)

	startedAt := time.UnixMilli(1_700_000_001_000)
	require.NoError(t, store.UpdateStatus(ctx, occ.OccurrenceID, OccurrenceStarted, "", startedAt))

	got, err := store.GetByID(ctx, occ.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, OccurrenceStarted, got.Status)
	require.NotNil(t, got.StartedAtMs)
	require.Equal(t, startedAt.UnixMilli(), *got.StartedAtMs)
	require.Nil(t, got.FinishedAtMs, "a started occurrence is not finished yet")

	// Re-reporting started must not move the original started_at.
	later := time.UnixMilli(1_700_000_009_000)
	require.NoError(t, store.UpdateStatus(ctx, occ.OccurrenceID, OccurrenceStarted, "", later))
	got, err = store.GetByID(ctx, occ.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, startedAt.UnixMilli(), *got.StartedAtMs)

	require.NoError(t, store.UpdateStatus(ctx, occ.OccurrenceID,
		OccurrenceUnknown, "EXECUTION_TIMEOUT", later))
	got, err = store.GetByID(ctx, occ.OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, OccurrenceUnknown, got.Status)
	require.Equal(t, "EXECUTION_TIMEOUT", got.ErrorCode)
	require.NotNil(t, got.FinishedAtMs, "unknown is a terminal outcome for the occurrence")
}

func TestOccurrenceGetMissingReturnsSentinel(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	_, err := store.Get(context.Background(), "sched|missing|rev|1", 0)
	require.ErrorIs(t, err, ErrOccurrenceNotFound)

	_, err = store.GetByID(context.Background(), "occ_missing")
	require.ErrorIs(t, err, ErrOccurrenceNotFound)
	require.False(t, errors.Is(err, ErrTriggerIdentityIncomplete))
}

func TestOccurrenceListByJobNewestFirst(t *testing.T) {
	t.Parallel()

	store := newOccurrenceStore(t)
	ctx := context.Background()
	for i, instant := range []int64{1_700_000_000_000, 1_700_000_060_000, 1_700_000_120_000} {
		occ, err := NewOccurrence(TriggerIdentity{
			Kind: TriggerScheduled, JobID: "job-1", ScheduleRev: "rev1",
			ScheduledAtMs: instant,
		}, time.UnixMilli(instant))
		require.NoError(t, err)
		_ = i
		_, _, err = store.Claim(ctx, occ)
		require.NoError(t, err)
	}

	// A different job must not leak into the listing.
	other, err := NewOccurrence(TriggerIdentity{
		Kind: TriggerScheduled, JobID: "job-2", ScheduleRev: "rev1",
		ScheduledAtMs: 1_700_000_999_000,
	}, time.UnixMilli(1_700_000_999_000))
	require.NoError(t, err)
	_, _, err = store.Claim(ctx, other)
	require.NoError(t, err)

	list, err := store.ListByJob(ctx, "job-1", 10)
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Equal(t, int64(1_700_000_120_000), list[0].ScheduledAtMs)
	require.Equal(t, int64(1_700_000_000_000), list[2].ScheduledAtMs)
}

func TestOccurrenceClaimRejectsIncompleteIdentity(t *testing.T) {
	t.Parallel()

	// An incomplete identity must fail before it can reach the store, so no
	// weak key is ever persisted.
	_, err := NewOccurrence(TriggerIdentity{Kind: TriggerWebhook, JobID: "job-1"},
		time.Now())
	require.ErrorIs(t, err, ErrTriggerIdentityIncomplete)
}
