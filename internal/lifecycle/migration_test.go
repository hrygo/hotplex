package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

func TestMigrationPreviewApplyExtendsOnlyEligibleLegacyRecords(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	lastInput := now.Add(-60 * 24 * time.Hour)
	contentCreated := now.Add(-60 * 24 * time.Hour).UnixMilli()
	_, err := db.Exec(`
		INSERT INTO sessions
			(id, lifecycle_policy, lifecycle_policy_revision, last_input_at, state, title)
		VALUES
			('eligible', 'legacy', '', ?, 'terminated', 'old chat'),
			('ambiguous', 'legacy', '', NULL, 'terminated', 'unknown clock'),
			('deleted', 'legacy', '', ?, 'deleted', 'deleted chat')`,
		lastInput, lastInput)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO events (id, session_id, created_at, expires_at, data)
		VALUES
			(1, 'eligible', ?, 0, 'secret event'),
			(2, 'ambiguous', ?, 0, 'ambiguous body'),
			(3, 'missing-session', ?, 0, 'orphan body')`,
		contentCreated, contentCreated, contentCreated)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO turns (id, session_id, created_at, expires_at, content, tools_json)
		VALUES
			(1, 'eligible', ?, 0, 'secret turn', NULL),
			(2, 'ambiguous', ?, 0, 'ambiguous turn', NULL)`,
		contentCreated, contentCreated)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO execution_inputs (id, session_id, status, runtime_status, fence_reason)
		VALUES ('unknown-1', 'eligible', 'unknown', 'unknown', '')`)
	require.NoError(t, err)

	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       180 * 24 * time.Hour,
			LegacyContentRetention: 30 * 24 * time.Hour,
			BatchSize:              100,
		}
	}, func() time.Time { return now })

	preview, err := service.Preview(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.Sessions.Count)
	require.EqualValues(t, 1, preview.Events.Count)
	require.EqualValues(t, 1, preview.Turns.Count)
	require.EqualValues(t, 1, preview.Blocked.LegacySessionsMissingInputClock)
	require.EqualValues(t, 1, preview.Blocked.OrphanedEvents)
	require.EqualValues(t, 2, preview.Blocked.ContentOnSessionsMissingInputClock)
	require.EqualValues(t, 1, preview.Blocked.UnknownExecutionRecords)

	previewJSON, err := json.Marshal(preview)
	require.NoError(t, err)
	require.NotContains(t, string(previewJSON), "secret event")
	require.NotContains(t, string(previewJSON), "secret turn")
	require.NotContains(t, string(previewJSON), "eligible")

	_, err = service.Apply(context.Background(), preview.PlanID, "wrong-plan")
	require.ErrorIs(t, err, ErrConfirmationRequired)

	result, err := service.Apply(context.Background(), preview.PlanID, preview.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Sessions)
	require.EqualValues(t, 1, result.Events)
	require.EqualValues(t, 1, result.Turns)

	var policy, revision string
	var migratedInput, archiveAt, conversationExpiresAt, historyExpiresAt time.Time
	err = db.QueryRow(`
		SELECT lifecycle_policy, lifecycle_policy_revision, last_input_at, archive_at,
		       conversation_expires_at, history_expires_at
		FROM sessions WHERE id = 'eligible'`).
		Scan(&policy, &revision, &migratedInput, &archiveAt, &conversationExpiresAt, &historyExpiresAt)
	require.NoError(t, err)
	require.Equal(t, "v2", policy)
	require.NotEmpty(t, revision)
	require.True(t, migratedInput.Equal(lastInput), "migration must not invent new user activity")
	require.True(t, archiveAt.Equal(lastInput.Add(7*24*time.Hour)))
	require.True(t, conversationExpiresAt.Equal(lastInput.Add(180*24*time.Hour)))
	require.True(t, historyExpiresAt.Equal(conversationExpiresAt))

	var eventExpiry, turnExpiry int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM events WHERE id = 1`).Scan(&eventExpiry))
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM turns WHERE id = 1`).Scan(&turnExpiry))
	require.Equal(t, contentCreated+int64((180*24*time.Hour)/time.Millisecond), eventExpiry)
	require.Equal(t, eventExpiry, turnExpiry)

	var ambiguousPolicy string
	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'ambiguous'`).Scan(&ambiguousPolicy))
	require.Equal(t, "legacy", ambiguousPolicy)
	var orphanExpiry int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM events WHERE id = 3`).Scan(&orphanExpiry))
	require.Zero(t, orphanExpiry, "orphaned content must not be revived")
}

func TestMigrationApplyRejectsStalePreviewWithoutPartialChanges(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	lastInput := now.Add(-60 * 24 * time.Hour)
	_, err := db.Exec(`
		INSERT INTO sessions (id, lifecycle_policy, last_input_at, state)
		VALUES ('eligible', 'legacy', ?, 'terminated')`, lastInput)
	require.NoError(t, err)

	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       180 * 24 * time.Hour,
			LegacyContentRetention: 30 * 24 * time.Hour,
			BatchSize:              100,
		}
	}, func() time.Time { return now })

	preview, err := service.Preview(context.Background())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sessions SET last_input_at = ? WHERE id = 'eligible'`, lastInput.Add(time.Hour))
	require.NoError(t, err)

	_, err = service.Apply(context.Background(), preview.PlanID, preview.PlanID)
	require.ErrorIs(t, err, ErrStalePreview)

	var policy string
	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'eligible'`).Scan(&policy))
	require.Equal(t, "legacy", policy)
}

func TestMigrationApplyRejectsPolicyChangeBeforeCommit(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	lastInput := now.Add(-60 * 24 * time.Hour)
	_, err := db.Exec(`INSERT INTO sessions (id, lifecycle_policy, last_input_at, state) VALUES ('eligible', 'legacy', ?, 'terminated')`, lastInput)
	require.NoError(t, err)

	basePolicy := RetentionPolicy{
		ArchiveAfter:           7 * 24 * time.Hour,
		ConversationRetention:  180 * 24 * time.Hour,
		ContentRetention:       180 * 24 * time.Hour,
		LegacyContentRetention: 30 * 24 * time.Hour,
		BatchSize:              100,
	}
	policyCalls := 0
	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		policyCalls++
		if policyCalls >= 3 {
			changed := basePolicy
			changed.ContentRetention += 24 * time.Hour
			return changed
		}
		return basePolicy
	}, func() time.Time { return now })

	preview, err := service.Preview(context.Background())
	require.NoError(t, err)
	_, err = service.Apply(context.Background(), preview.PlanID, preview.PlanID)
	require.ErrorIs(t, err, ErrStalePreview)

	var policy string
	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'eligible'`).Scan(&policy))
	require.Equal(t, "legacy", policy, "the transaction must roll back when the policy changes before commit")
}

func newMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqlutil.DriverName, filepath.Join(t.TempDir(), "lifecycle-migration.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			lifecycle_policy TEXT NOT NULL DEFAULT 'legacy',
			lifecycle_policy_revision TEXT NOT NULL DEFAULT '',
			last_input_at DATETIME,
			archive_at DATETIME,
			conversation_expires_at DATETIME,
			last_content_expires_at DATETIME,
			history_expires_at DATETIME,
			deleted_at DATETIME,
			state TEXT NOT NULL,
			title TEXT,
			work_dir TEXT,
			context_json TEXT,
			platform_key_json TEXT
		);
		CREATE TABLE events (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL DEFAULT 0,
			data TEXT NOT NULL
		);
		CREATE TABLE turns (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL DEFAULT 0,
			content TEXT NOT NULL DEFAULT '',
			tools_json TEXT
		);
		CREATE TABLE execution_inputs (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			status TEXT NOT NULL,
			runtime_status TEXT NOT NULL DEFAULT 'completed',
			fence_reason TEXT NOT NULL DEFAULT ''
		);`)
	require.NoError(t, err)
	return db
}

func TestMigrationPreviewUsesBatchBoundAndExpires(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		lastInput := now.Add(-time.Duration(60+i) * 24 * time.Hour)
		_, err := db.Exec(`INSERT INTO sessions (id, lifecycle_policy, last_input_at, state) VALUES (?, 'legacy', ?, 'terminated')`, strings.Repeat("s", i+1), lastInput)
		require.NoError(t, err)
	}
	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       180 * 24 * time.Hour,
			LegacyContentRetention: 30 * 24 * time.Hour,
			BatchSize:              1,
		}
	}, func() time.Time { return now })

	preview, err := service.Preview(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 3, preview.Sessions.Count)
	require.EqualValues(t, 1, preview.BatchSize)
	require.True(t, preview.ExpiresAt.Equal(now.Add(migrationPreviewTTL)))
}

func TestMigrationPreviewCapsConfiguredBatchSize(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       180 * 24 * time.Hour,
			LegacyContentRetention: 30 * 24 * time.Hour,
			BatchSize:              maxMigrationBatchSize + 1,
		}
	}, func() time.Time { return now })

	preview, err := service.Preview(context.Background())
	require.NoError(t, err)
	require.Equal(t, maxMigrationBatchSize, preview.BatchSize)
}

func TestMigrationWaitsForAllContentBeforeSwitchingSessionPolicy(t *testing.T) {
	t.Parallel()

	db := newMigrationTestDB(t)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	lastInput := now.Add(-60 * 24 * time.Hour)
	createdAt := lastInput.UnixMilli()
	_, err := db.Exec(`INSERT INTO sessions (id, lifecycle_policy, last_input_at, state) VALUES ('large', 'legacy', ?, 'terminated')`, lastInput)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO events (id, session_id, created_at, expires_at, data)
		VALUES (1, 'large', ?, 0, 'first'), (2, 'large', ?, 0, 'second')`,
		createdAt, createdAt+1)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO turns (id, session_id, created_at, expires_at, content)
		VALUES (1, 'large', ?, 0, 'first'), (2, 'large', ?, 0, 'second')`,
		createdAt, createdAt+1)
	require.NoError(t, err)

	service := NewMigrationService(db, dbutil.DialectSQLite, func() RetentionPolicy {
		return RetentionPolicy{
			ArchiveAfter:           7 * 24 * time.Hour,
			ConversationRetention:  180 * 24 * time.Hour,
			ContentRetention:       180 * 24 * time.Hour,
			LegacyContentRetention: 30 * 24 * time.Hour,
			BatchSize:              1,
		}
	}, func() time.Time { return now })

	firstPreview, err := service.Preview(context.Background())
	require.NoError(t, err)
	firstBatch, err := service.Apply(context.Background(), firstPreview.PlanID, firstPreview.PlanID)
	require.NoError(t, err)
	require.Zero(t, firstBatch.Sessions, "session policy must wait for the remaining content batch")
	require.EqualValues(t, 1, firstBatch.Events)
	require.EqualValues(t, 1, firstBatch.Turns)

	var policy string
	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'large'`).Scan(&policy))
	require.Equal(t, "legacy", policy)

	secondPreview, err := service.Preview(context.Background())
	require.NoError(t, err)
	secondBatch, err := service.Apply(context.Background(), secondPreview.PlanID, secondPreview.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 1, secondBatch.Sessions)
	require.EqualValues(t, 1, secondBatch.Events)
	require.EqualValues(t, 1, secondBatch.Turns)

	require.NoError(t, db.QueryRow(`SELECT lifecycle_policy FROM sessions WHERE id = 'large'`).Scan(&policy))
	require.Equal(t, "v2", policy)
	var missingEventDeadlines, missingTurnDeadlines int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = 'large' AND expires_at = 0`).Scan(&missingEventDeadlines))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM turns WHERE session_id = 'large' AND expires_at = 0`).Scan(&missingTurnDeadlines))
	require.Zero(t, missingEventDeadlines)
	require.Zero(t, missingTurnDeadlines)
}
