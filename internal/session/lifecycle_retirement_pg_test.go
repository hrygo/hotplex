//go:build pg

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/pkg/events"
)

func openLifecyclePGStores(t *testing.T) (*pgStore, *pgStore, *execution.SQLStore, *execution.SQLStore) {
	t.Helper()

	dsn := os.Getenv("HOTPLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HOTPLEX_TEST_PG_DSN not set; skipping PG lifecycle concurrency test")
	}

	cfg := &config.DBConfig{
		Driver:   "postgres",
		Postgres: config.PostgresConfig{ConnStr: dsn},
	}
	dbA, err := dbutil.Open(dbutil.DialectPostgres, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbA.Close() })

	dbB, err := dbutil.Open(dbutil.DialectPostgres, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbB.Close() })

	ctx := context.Background()
	_, err = dbA.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE`)
	require.NoError(t, err)
	_, err = dbA.DB.ExecContext(ctx, `CREATE SCHEMA public`)
	require.NoError(t, err)

	storeAValue, err := NewPGStore(ctx, dbA)
	require.NoError(t, err)
	storeBValue, err := NewPGStore(ctx, dbB)
	require.NoError(t, err)
	storeA, ok := storeAValue.(*pgStore)
	require.True(t, ok)
	storeB, ok := storeBValue.(*pgStore)
	require.True(t, ok)

	executionA, err := execution.NewSQLStore(ctx, dbA.DB, dbutil.DialectPostgres, nil, nil)
	require.NoError(t, err)
	executionB, err := execution.NewSQLStore(ctx, dbB.DB, dbutil.DialectPostgres, nil, nil)
	require.NoError(t, err)

	return storeA, storeB, executionA, executionB
}

func TestPGStore_RetirementSerializesWithInputAcceptanceAcrossInstances(t *testing.T) {
	storeA, storeB, executionA, executionB := openLifecyclePGStores(t)
	ctx := context.Background()

	for i := range 16 {
		id := fmt.Sprintf("session-lifecycle-race-%02d", i)
		now := time.Now().UTC()
		require.NoError(t, storeA.Upsert(ctx, expiredLifecycleSession(id, now)))

		start := make(chan struct{})
		type acceptResult struct {
			record *execution.Record
			err    error
		}
		accepted := make(chan acceptResult, 1)
		retired := make(chan *SessionInfo, 1)
		retireErrs := make(chan error, 1)

		acceptStore := executionA
		retireStore := storeB
		if i%2 == 1 {
			acceptStore = executionB
			retireStore = storeA
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			record, _, err := acceptStore.Accept(ctx, execution.AcceptRequest{
				SessionID: id, ClientMessageID: "message-" + id, PayloadHash: "payload-" + id,
				OwnerInstanceID: "gateway-A", WorkerRunID: "run-" + id,
			})
			accepted <- acceptResult{record: record, err: err}
		}()
		go func() {
			defer wg.Done()
			<-start
			info, err := retireStore.RetireExpiredLifecycleSession(ctx, id, now)
			retired <- info
			retireErrs <- err
		}()

		close(start)
		wg.Wait()
		accept := <-accepted
		retiredInfo := <-retired
		require.NoError(t, <-retireErrs)

		if accept.err == nil {
			require.NotNil(t, accept.record)
			require.Nil(t, retiredInfo, "accepted input must block retirement")
			stored, err := storeA.Get(ctx, id)
			require.NoError(t, err)
			require.NotEqual(t, events.StateDeleted, stored.State)
			continue
		}

		require.ErrorIs(t, accept.err, execution.ErrSessionExpired,
			"retirement winning the session row lock must reject new input")
		require.NotNil(t, retiredInfo, "rejected input must have a committed retirement")
		stored, err := storeA.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, events.StateDeleted, stored.State)
	}
}

func TestPGStore_DeleteFencesConcurrentLateUpsertAcrossInstances(t *testing.T) {
	storeA, storeB, _, _ := openLifecyclePGStores(t)
	ctx := context.Background()
	id := "session-lifecycle-delete-upsert-race"
	now := time.Now().UTC()
	info := expiredLifecycleSession(id, now)
	require.NoError(t, storeA.Upsert(ctx, info))

	lateWriter := *info
	lateWriter.State = events.StateRunning
	lateWriter.Title = "stale writer title"
	lateWriter.WorkDir = "/stale/writer"
	lateWriter.Context = map[string]any{"late": "write"}

	start := make(chan struct{})
	deleteErrs := make(chan error, 1)
	upsertErrs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		deleteErrs <- storeA.MarkDeletedWithCleanup(ctx, info)
	}()
	go func() {
		defer wg.Done()
		<-start
		upsertErrs <- storeB.Upsert(ctx, &lateWriter)
	}()

	close(start)
	wg.Wait()
	require.NoError(t, <-deleteErrs)
	upsertErr := <-upsertErrs
	if upsertErr != nil {
		require.True(t, errors.Is(upsertErr, ErrSessionCleanupPending),
			"late writer should be fenced by the cleanup tombstone")
	}

	stored, err := storeA.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, events.StateDeleted, stored.State)
	require.NotNil(t, stored.DeletedAt)
	require.Empty(t, stored.Title)
	require.Empty(t, stored.WorkDir)
	require.Nil(t, stored.Context)
	require.Empty(t, stored.WorkerSessionID)

	require.ErrorIs(t, storeB.Upsert(ctx, &lateWriter), ErrSessionCleanupPending,
		"a stale writer after deletion must not recreate session data")
	pending, err := storeA.HasPendingCleanup(ctx, id)
	require.NoError(t, err)
	require.True(t, pending, "local deletion and remote cleanup must remain durably linked")
}
