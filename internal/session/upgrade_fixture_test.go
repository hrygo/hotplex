package session

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// preQueueFixture is a real SQLite database migrated to version 36 — the state
// an installation is in immediately before the bounded input queue arrived. It
// is committed on purpose: "upgrade from an existing installation" was an
// unverified claim for as long as there was no pre-migration database to
// upgrade, and a fresh database only ever exercises the first-start path.
//
// The native smoke job in release.yml runs the real binary against this same
// file, so the upgrade is exercised on macOS and Windows runners too.
const preQueueFixture = "testdata/pre-queue-036.db"

// preQueueVersion is the schema version the fixture is built at. It must stay
// below the first queue migration (37), or the fixture stops being a pre-queue
// database.
const preQueueVersion = 36

var updateFixtures = flag.Bool("update-fixtures", false,
	"rewrite the committed migration fixtures under testdata/")

func openFixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqlutil.DriverName, path)
	require.NoError(t, err)
	cfg := config.Default()
	require.NoError(t, sqlutil.InitSQLiteDB(db, &cfg.DB, sqlutil.DialectSQLite, "upgrade_fixture"),
		"open sqlite fixture")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func gooseProvider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()
	migrations, err := fs.Sub(migrationFS, "sql/migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations,
		goose.WithDisableGlobalRegistry(true),
	)
	require.NoError(t, err)
	return provider
}

// TestBuildPreQueueFixture regenerates the committed database:
//
//	go test ./internal/session -run TestBuildPreQueueFixture -update-fixtures
//
// A binary blob with no generator rots the moment a column changes; this is
// why the fixture can be rebuilt rather than hand-edited.
func TestBuildPreQueueFixture(t *testing.T) {
	if !*updateFixtures {
		t.Skipf("run with -update-fixtures to rebuild %s", preQueueFixture)
	}
	ctx := context.Background()
	require.NoError(t, os.MkdirAll("testdata", 0o750))
	path := filepath.Join("testdata", filepath.Base(preQueueFixture))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err, "remove the previous fixture")
	}

	db := openFixtureDB(t, path)
	_, err := gooseProvider(t, db).UpTo(ctx, preQueueVersion)
	require.NoError(t, err, "migrate the fixture to version %d", preQueueVersion)

	// One session per runtime state, because idx_execution_one_active_per_session
	// allows at most ONE row per session that is pending/running OR fenced — a
	// single index, not one slot for each. That constraint is exactly why a
	// fenced session stops accepting new inputs, so the fixture reproduces it
	// rather than working around it.
	sessions := []struct{ id, title string }{
		{"sess-legacy", "legacy run"},
		{"sess-pending", "accepted, never dispatched"},
		{"sess-fenced", "worker response lost"},
	}
	for _, row := range sessions {
		_, err = db.ExecContext(ctx, `INSERT INTO sessions
			(id, user_id, worker_type, state, created_at, updated_at, title)
			VALUES (?, 'u-1', 'claude_code', 'idle', '2023-11-14 22:13:20',
				'2023-11-14 22:20:00', ?)`,
			row.id, row.title)
		require.NoErrorf(t, err, "seed session %s", row.id)
	}

	// An input that was accepted but never dispatched, one that completed, and
	// one that ended unknown and fenced. The last is the one a careless upgrade
	// would rewrite into a success.
	seed := []struct {
		execID, sessionID, status, runtime, fence string
		fenceVersion                              int
	}{
		{"exec-accepted", "sess-pending", "accepted", "pending", "", 0},
		{"exec-done", "sess-legacy", "delivered", "completed", "", 0},
		{"exec-fenced", "sess-fenced", "unknown", "unknown", "WORKER_RESPONSE_LOST", 1},
	}
	for _, row := range seed {
		_, err = db.ExecContext(ctx, `INSERT INTO execution_inputs
			(execution_id, session_id, client_message_id, payload_hash, status, error_code,
			 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
			 runtime_status, runtime_error_code, fence_reason, fence_version, fence_created_at)
			VALUES (?, ?, ?, ?, ?, '', 1700000000000, 1700000000000, 'inst-a', 'run-1', 0,
			 ?, '', ?, ?, 1700000000000)`,
			row.execID, row.sessionID, "msg-"+row.execID, "hash-"+row.execID,
			row.status, row.runtime, row.fence, row.fenceVersion)
		require.NoErrorf(t, err, "seed execution %s", row.execID)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO cron_occurrences
		(occurrence_id, trigger_key, generation, job_id, trigger_kind, delivery_mode,
		 status, created_at, updated_at, finished_at)
		VALUES ('occ-legacy', 'sched|job-1|rev1|1700000000000', 0, 'job-1', 'scheduled',
			'legacy_cli', 'completed', 1700000000000, 1700000060000, 1700000060000)`)
	require.NoError(t, err, "seed occurrence")

	// A delivery confirmed before the upgrade, with its receipt. If the ledger's
	// rows do not survive, "the delivery happened" becomes unfalsifiable after an
	// upgrade — which is the whole reason the ledger is durable.
	_, err = db.ExecContext(ctx, `INSERT INTO effects
		(effect_id, occurrence_id, delivery_ordinal, target_revision, attempt,
		 session_id, execution_id, target_kind, target_ref, status,
		 provider_ref, created_at, updated_at)
		VALUES ('eff-legacy', 'occ-legacy', 0, 'rev1', 1,
			'sess-legacy', 'exec-done', 'slack', 'C123', 'delivered',
			'1712345678.0001', 1700000000000, 1700000060000)`)
	require.NoError(t, err, "seed effect")

	_, err = db.ExecContext(ctx, `INSERT INTO effect_attempts
		(attempt_id, effect_id, attempt, owner_instance_id, lease_version,
		 lease_token, started_at, finished_at, outcome, provider_ref)
		VALUES ('att-legacy', 'eff-legacy', 1, 'inst-a', 1,
			'tok-1', 1700000000000, 1700000060000, 'accepted', '1712345678.0001')`)
	require.NoError(t, err, "seed effect attempt")

	_, err = db.ExecContext(ctx, `INSERT INTO user_activity
		(id, ts, user_id, user_id_type, platform, session_id, action,
		 resource_type, resource_id, outcome, detail_json, prev_hash, self_hash)
		VALUES (1, 1700000000000, 'u-1', 'platform', 'webchat', 'sess-legacy',
			'message.send', 'session', 'sess-legacy', 'success', '{}', '', 'hash-1')`)
	require.NoError(t, err, "seed audit row")
}

// TestUpgradeFromPreQueueFixture is the upgrade path: an installation that
// already has data, running the migrations that add the queue.
func TestUpgradeFromPreQueueFixture(t *testing.T) {
	ctx := context.Background()

	src, err := os.Open(preQueueFixture)
	require.NoErrorf(t, err, "fixture %s must be committed; rebuild it with -update-fixtures", preQueueFixture)
	dst := filepath.Join(t.TempDir(), "hotplex.db")
	out, err := os.Create(dst)
	require.NoError(t, err)
	_, err = src.WriteTo(out)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.NoError(t, src.Close())

	db := openFixtureDB(t, dst)

	// The upgrade itself.
	require.NoError(t, RunMigrations(ctx, db, dbutil.DialectSQLite),
		"an existing installation must upgrade cleanly")

	t.Run("pre-existing rows survive", func(t *testing.T) {
		var sessions int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sessions WHERE id = 'sess-legacy' AND title = 'legacy run'`).Scan(&sessions))
		require.Equal(t, 1, sessions, "the session must survive the upgrade")

		var policy, revision string
		var conversationExpiry, historyExpiry sql.NullTime
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT lifecycle_policy, lifecycle_policy_revision, conversation_expires_at, history_expires_at
			 FROM sessions WHERE id = 'sess-legacy'`,
		).Scan(&policy, &revision, &conversationExpiry, &historyExpiry))
		require.Equal(t, "legacy", policy, "upgrade must preserve existing rows on their legacy policy")
		require.Empty(t, revision, "upgrade must not fabricate a policy revision for old rows")
		require.False(t, conversationExpiry.Valid, "upgrade must not invent a user-input-based expiry")
		require.False(t, historyExpiry.Valid, "upgrade must not extend or shorten legacy history")

		var effects, attempts int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM effects WHERE effect_id = 'eff-legacy' AND status = 'delivered'`).Scan(&effects))
		require.Equal(t, 1, effects, "a confirmed delivery must not vanish on upgrade")
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM effect_attempts WHERE attempt_id = 'att-legacy'`).Scan(&attempts))
		require.Equal(t, 1, attempts, "the attempt history must survive")

		var audit int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_activity WHERE id = 1 AND self_hash = 'hash-1'`).Scan(&audit))
		require.Equal(t, 1, audit, "the audit chain must survive")

		var mode string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT delivery_mode FROM cron_occurrences WHERE occurrence_id = 'occ-legacy'`).Scan(&mode))
		require.Equal(t, "legacy_cli", mode,
			"an occurrence that ran before the upgrade keeps its original delivery owner")
	})

	t.Run("runtime states are not rewritten", func(t *testing.T) {
		// The dangerous upgrade is one that "normalises" an unknown or fenced run
		// into a success. The rows must come out exactly as they went in.
		rows, err := db.QueryContext(ctx,
			`SELECT execution_id, status, runtime_status, fence_reason, fence_version
			 FROM execution_inputs ORDER BY execution_id`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()

		got := map[string][3]any{}
		for rows.Next() {
			var id, status, runtime, fence string
			var version int
			require.NoError(t, rows.Scan(&id, &status, &runtime, &fence, &version))
			got[id] = [3]any{status + "/" + runtime, fence, version}
		}
		require.NoError(t, rows.Err())

		require.Equal(t, map[string][3]any{
			"exec-accepted": {"accepted/pending", "", 0},
			"exec-done":     {"delivered/completed", "", 0},
			"exec-fenced":   {"unknown/unknown", "WORKER_RESPONSE_LOST", 1},
		}, got)
	})

	t.Run("legacy executions keep no fabricated turn deadline", func(t *testing.T) {
		var startedAt, deadlineAt sql.NullInt64
		var revision string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT turn_started_at, turn_deadline_at, turn_policy_revision
			 FROM execution_inputs WHERE execution_id = 'exec-done'`,
		).Scan(&startedAt, &deadlineAt, &revision))
		require.False(t, startedAt.Valid)
		require.False(t, deadlineAt.Valid)
		require.Empty(t, revision)
	})

	t.Run("queue schema exists and admits queued", func(t *testing.T) {
		for _, table := range []string{
			"execution_queue", "execution_queue_counters",
			"execution_queue_budget", "execution_queue_payloads",
		} {
			var n int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n))
			require.Equalf(t, 1, n, "table %s must exist after the upgrade", table)
		}

		// The point of migration 037: the CHECK on runtime_status was widened to
		// admit 'queued'. If it were not, every enqueue would fail on a database
		// that already existed — a failure a fresh-database test cannot see.
		_, err := db.ExecContext(ctx, `INSERT INTO execution_inputs
			(execution_id, session_id, client_message_id, payload_hash, status, error_code,
			 created_at, updated_at, owner_instance_id, worker_run_id, lease_until,
			 runtime_status, runtime_error_code, fence_reason, fence_version)
			VALUES ('exec-queued', 'sess-legacy', 'msg-queued', 'hash-queued', 'accepted', '',
			 1700000000000, 1700000000000, '', '', 0, 'queued', '', '', 0)`)
		require.NoError(t, err, "the upgraded schema must accept a queued input")

		var checkSQL string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='table' AND name='execution_inputs'`).Scan(&checkSQL))
		require.Contains(t, checkSQL, "queued", "the rebuilt table must declare the queued state")
	})

	t.Run("the store serves the upgraded database", func(t *testing.T) {
		// Migrations can apply and still leave a schema the store cannot read, so
		// the assertion is a real query through the store against the file the
		// upgrade produced — the same path the gateway takes on startup.
		cfg := config.Default()
		cfg.DB.Driver = "sqlite"
		cfg.DB.Path = dst
		cfg.DB.SQLite.Path = dst
		store, err := NewSQLiteStore(ctx, cfg, nil)
		require.NoError(t, err, "the store must open an upgraded database")
		t.Cleanup(func() { _ = store.Close() })

		info, err := store.Get(ctx, "sess-legacy")
		require.NoError(t, err, "the pre-upgrade session must be readable after the upgrade")
		require.Equal(t, "u-1", info.UserID)
		require.Equal(t, "legacy run", info.Title)
	})
}
