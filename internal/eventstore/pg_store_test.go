package eventstore

import (
	"context"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/dbutil"
)

func newEventPGMock(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)

	db := &dbutil.DB{DB: mockDB}

	pg := dbutil.DialectPostgres
	sqlMap := map[string]string{
		"insert":                  pg.Rebind("INSERT INTO events (session_id, seq, type, data, direction, source, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"),
		"query_latest":            pg.Rebind("SELECT id, session_id, seq, type, data, direction, source, created_at, expires_at FROM events WHERE session_id = ? AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?)) ORDER BY id DESC LIMIT ?"),
		"delete_by_session":       pg.Rebind(queries["delete_by_session"]),
		"turns.delete_by_session": pg.Rebind(queries["turns.delete_by_session"]),
	}

	store := &pgStore{
		db:      db,
		dialect: pg,
		sql:     sqlMap,
		log:     slog.Default(),
	}

	return store, mock, func() {
		require.NoError(t, mock.ExpectationsWereMet())
		mockDB.Close()
	}
}

func eventColumns() []string {
	return []string{"id", "session_id", "seq", "type", "data", "direction", "source", "created_at", "expires_at"}
}

func expectPGSessionWriteBarrier(mock sqlmock.Sqlmock, sessionID string, state string, found bool, hasPurgeJob bool) {
	stateQuery := dbutil.DialectPostgres.Rebind(`SELECT state FROM sessions WHERE id = ? FOR UPDATE`)
	stateRows := sqlmock.NewRows([]string{"state"})
	if found {
		stateRows.AddRow(state)
	}
	mock.ExpectQuery(regexp.QuoteMeta(stateQuery)).WithArgs(sessionID).WillReturnRows(stateRows)
	if found {
		return
	}
	purgeQuery := dbutil.DialectPostgres.Rebind(`SELECT EXISTS(
		SELECT 1 FROM session_purge_jobs WHERE session_id = ?
	)`)
	mock.ExpectQuery(regexp.QuoteMeta(purgeQuery)).
		WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(hasPurgeJob))
}

func TestPGEventStore_AppendEvent(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()

	event := &StoredEvent{
		SessionID: "sess-1",
		Seq:       1,
		Type:      "message.delta",
		Data:      []byte(`{"content":"hello"}`),
		Direction: "out",
		Source:    "normal",
		CreatedAt: 1000000,
	}

	q := dbutil.DialectPostgres.Rebind(
		"INSERT INTO events (session_id, seq, type, data, direction, source, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")

	mock.ExpectBegin()
	expectPGSessionWriteBarrier(mock, event.SessionID, "", false, false)
	mock.ExpectExec(regexp.QuoteMeta(q)).
		WithArgs(event.SessionID, event.Seq, event.Type, event.Data, event.Direction, event.Source, event.CreatedAt, int64(0)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := store.Append(context.Background(), event)
	require.NoError(t, err)
}

func TestPGEventStore_AppendAssignsExpiryAndAdvancesSessionDeadline(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()
	store.retention = RetentionPolicy{Content: 24 * time.Hour, Legacy: 30 * 24 * time.Hour}
	store.sql["lifecycle.get_content_policy"] = dbutil.DialectPostgres.Rebind(queries["lifecycle.get_content_policy"])
	store.sql["lifecycle.update_session_content_deadline"] =
		dbutil.DialectPostgres.Rebind(queries["lifecycle.update_session_content_deadline"])

	event := &StoredEvent{
		SessionID: "sess-1",
		Seq:       1,
		Type:      "message",
		Data:      []byte(`{"content":"hello"}`),
		Direction: "out",
		Source:    "normal",
		CreatedAt: 1_000_000,
	}
	expiresAt := event.CreatedAt + (24 * time.Hour).Milliseconds()
	insertQuery := dbutil.DialectPostgres.Rebind(queries["insert"])
	updateQuery := dbutil.DialectPostgres.Rebind(queries["lifecycle.update_session_content_deadline"])
	policyQuery := dbutil.DialectPostgres.Rebind(queries["lifecycle.get_content_policy"])
	deadline := time.UnixMilli(expiresAt).UTC()

	mock.ExpectBegin()
	expectPGSessionWriteBarrier(mock, event.SessionID, "running", true, false)
	mock.ExpectQuery(regexp.QuoteMeta(policyQuery)).
		WithArgs(event.SessionID).
		WillReturnRows(sqlmock.NewRows([]string{"lifecycle_policy"}).AddRow("v2"))
	mock.ExpectExec(regexp.QuoteMeta(insertQuery)).
		WithArgs(event.SessionID, event.Seq, event.Type, event.Data, event.Direction, event.Source, event.CreatedAt, expiresAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(updateQuery)).
		WithArgs(deadline, deadline, deadline, deadline, event.SessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, store.Append(context.Background(), event))
}

func TestPGEventStore_RejectsPurgeTombstone(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()
	event := &StoredEvent{
		SessionID: "sess-purged", Seq: 1, Type: "message",
		Data: []byte(`{}`), Direction: "out", Source: SourceNormal, CreatedAt: 1000,
	}

	mock.ExpectBegin()
	expectPGSessionWriteBarrier(mock, event.SessionID, "", false, true)
	mock.ExpectRollback()

	require.ErrorIs(t, store.Append(context.Background(), event), ErrSessionDeleted)
}

func TestPGEventStore_DeleteConversationAtomic(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()
	sessionID := "sess-purge"
	deleteEvents := regexp.QuoteMeta(store.sql["delete_by_session"])
	deleteTurns := regexp.QuoteMeta(store.sql["turns.delete_by_session"])

	mock.ExpectBegin()
	mock.ExpectExec(deleteEvents).WithArgs(sessionID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(deleteTurns).WithArgs(sessionID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, store.DeleteConversationBySession(context.Background(), sessionID))
}

func TestPGEventStore_DeleteConversationRollsBackTogether(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()
	sessionID := "sess-purge"

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(store.sql["delete_by_session"])).
		WithArgs(sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(store.sql["turns.delete_by_session"])).
		WithArgs(sessionID).
		WillReturnError(sqlmock.ErrCancelled)
	mock.ExpectRollback()

	require.Error(t, store.DeleteConversationBySession(context.Background(), sessionID))
}

func TestPGEventStore_QueryBySession_Latest(t *testing.T) {
	t.Parallel()
	store, mock, cleanup := newEventPGMock(t)
	defer cleanup()

	sessionID := "sess-1"
	fetchLimit := 201

	q := dbutil.DialectPostgres.Rebind(
		"SELECT id, session_id, seq, type, data, direction, source, created_at, expires_at FROM events WHERE session_id = ? AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?)) ORDER BY id DESC LIMIT ?")

	rows := sqlmock.NewRows(eventColumns()).
		AddRow(int64(2), sessionID, int64(2), "message.delta", []byte(`{"c":"b"}`), "out", "normal", int64(2000), int64(0)).
		AddRow(int64(1), sessionID, int64(1), "message.delta", []byte(`{"c":"a"}`), "out", "normal", int64(1000), int64(0))

	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs(sessionID, sqlmock.AnyArg(), sqlmock.AnyArg(), fetchLimit).
		WillReturnRows(rows)

	page, err := store.QueryBySession(context.Background(), sessionID, 0, CursorLatest, 200)
	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Events, 2)
	require.Equal(t, int64(1), page.Events[0].Seq)
	require.Equal(t, int64(2), page.Events[1].Seq)
	require.Equal(t, int64(1), page.OldestSeq)
	require.Equal(t, int64(2), page.NewestSeq)
	require.Equal(t, int64(1), page.OldestID)
	require.Equal(t, int64(2), page.NewestID)
	require.False(t, page.HasOlder)
}

func TestPGEventStore_Close(t *testing.T) {
	t.Parallel()
	store, _, cleanup := newEventPGMock(t)
	defer cleanup()

	err := store.Close()
	require.NoError(t, err)
}
