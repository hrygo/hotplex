package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func TestMigrationPolicyIncreaseAfterPartialBatchEventuallyExtendsAllContent(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	lastInput := now.Add(-60 * 24 * time.Hour)
	contentCreatedAt := lastInput.UnixMilli()
	_, err := db.Exec(`
		INSERT INTO sessions (id, lifecycle_policy, last_input_at, state)
		VALUES ('policy-change', 'legacy', ?, 'terminated')`,
		lastInput)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO events (id, session_id, created_at, expires_at, data)
		VALUES
			(1, 'policy-change', ?, 0, 'first event'),
			(2, 'policy-change', ?, 0, 'second event')`,
		contentCreatedAt, contentCreatedAt+1)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO turns (id, session_id, created_at, expires_at, content)
		VALUES
			(1, 'policy-change', ?, 0, 'first turn'),
			(2, 'policy-change', ?, 0, 'second turn')`,
		contentCreatedAt, contentCreatedAt+1)
	require.NoError(t, err)

	legacyContentRetention := 30 * 24 * time.Hour
	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       30 * 24 * time.Hour,
			LegacyContentRetention: legacyContentRetention,
			BatchSize:              1,
		}
	}, func() time.Time { return now })

	firstPreview, err := service.Preview(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, firstPreview.Events.Count)
	require.EqualValues(t, 2, firstPreview.Turns.Count)
	firstBatch, err := service.Apply(context.Background(), firstPreview.PlanID, firstPreview.PlanID)
	require.NoError(t, err)
	require.Zero(t, firstBatch.Sessions, "the session must wait for the remaining content batch")
	require.EqualValues(t, 1, firstBatch.Events)
	require.EqualValues(t, 1, firstBatch.Turns)

	var firstEventDeadline, firstTurnDeadline int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM events WHERE id = 1`).Scan(&firstEventDeadline))
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM turns WHERE id = 1`).Scan(&firstTurnDeadline))
	require.Equal(t, contentCreatedAt+int64((30*24*time.Hour)/time.Millisecond), firstEventDeadline)
	require.Equal(t, firstEventDeadline, firstTurnDeadline)

	legacyContentRetention = 180 * 24 * time.Hour
	for attempt := 0; attempt < 5; attempt++ {
		preview, previewErr := service.Preview(context.Background())
		require.NoError(t, previewErr)
		_, applyErr := service.Apply(context.Background(), preview.PlanID, preview.PlanID)
		require.NoError(t, applyErr)

		var policy string
		require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'policy-change'`).Scan(&policy))
		if policy == "v2" {
			break
		}
	}

	var policy string
	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'policy-change'`).Scan(&policy))
	require.Equal(t, "v2", policy, "the session must eventually migrate after the retention policy increases")

	minimumExpiry := contentCreatedAt + int64((180*24*time.Hour)/time.Millisecond)
	for _, table := range []string{"events", "turns"} {
		rows, queryErr := db.Query(`SELECT id, created_at, expires_at FROM ` + table + ` WHERE session_id = 'policy-change' ORDER BY id`)
		require.NoError(t, queryErr)
		seen := 0
		for rows.Next() {
			var id, createdAt, expiresAt int64
			require.NoError(t, rows.Scan(&id, &createdAt, &expiresAt))
			require.GreaterOrEqual(t, expiresAt, minimumExpiry, "%s row %d must reach the new legacy retention deadline", table, id)
			seen++
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Equal(t, 2, seen)
	}

	var finalFirstEventDeadline, finalFirstTurnDeadline int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM events WHERE id = 1`).Scan(&finalFirstEventDeadline))
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM turns WHERE id = 1`).Scan(&finalFirstTurnDeadline))
	require.GreaterOrEqual(t, finalFirstEventDeadline, firstEventDeadline, "migration must not shorten an existing event deadline")
	require.GreaterOrEqual(t, finalFirstTurnDeadline, firstTurnDeadline, "migration must not shorten an existing turn deadline")
}
