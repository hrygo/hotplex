package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/lifecycle"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestManager_RecordInputAcceptedAfterMigrationRenewsSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		keepCached            bool
		sharedMigrationPolicy bool
	}{
		{name: "hot cached manager with shared revision", keepCached: true, sharedMigrationPolicy: true},
		{name: "cold manager with shared revision", keepCached: false, sharedMigrationPolicy: true},
		{name: "hot cached manager with legacy migration revision", keepCached: true},
		{name: "cold manager with legacy migration revision", keepCached: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store, cfg := helperDB(t)
			cfg.Session.GCScanInterval = time.Hour
			cfg.Lifecycle.Policy = config.LifecyclePolicyLegacy
			cfgStore := config.NewConfigStore(cfg, nil)

			legacyManager, err := NewManager(ctx, nil, cfg, cfgStore, store)
			require.NoError(t, err)
			_, err = legacyManager.Create(ctx, "migrated-session", "review-user", worker.TypeClaudeCode, nil, "", "review")
			require.NoError(t, err)
			legacyManager.gcStop()
			<-legacyManager.gcDone

			now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
			oldInput := now.Add(-179 * 24 * time.Hour)
			_, err = store.db.ExecContext(ctx,
				`UPDATE sessions
				SET state = ?, created_at = ?, updated_at = ?, last_input_at = ?
				WHERE id = ?`,
				string(events.StateIdle), oldInput, oldInput, oldInput, "migrated-session")
			require.NoError(t, err)

			v2Cfg := *cfg
			v2Cfg.Lifecycle.Policy = config.LifecyclePolicyV2
			v2Cfg.Lifecycle.Conversation.ArchiveAfter = 7 * 24 * time.Hour
			v2Cfg.Lifecycle.Conversation.RetentionAfterLastInput = 180 * 24 * time.Hour
			v2Cfg.Lifecycle.Content.Retention = 24 * time.Hour

			migrationPolicy := lifecycle.RetentionPolicy{
				ArchiveAfter:           7 * 24 * time.Hour,
				ConversationRetention:  180 * 24 * time.Hour,
				ContentRetention:       24 * time.Hour,
				LegacyContentRetention: 30 * 24 * time.Hour,
				BatchSize:              100,
			}
			if tt.sharedMigrationPolicy {
				migrationPolicy.PolicyRevision = config.LifecyclePolicyRevision(v2Cfg.Lifecycle)
			}
			migration := lifecycle.NewMigrationService(
				store.DB(),
				dbutil.DialectSQLite,
				func() lifecycle.RetentionPolicy { return migrationPolicy },
				func() time.Time { return now },
			)
			preview, err := migration.Preview(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, preview.Sessions.Count)
			applied, err := migration.Apply(ctx, preview.PlanID, preview.PlanID)
			require.NoError(t, err)
			require.EqualValues(t, 1, applied.Sessions)

			var persistedRevision string
			require.NoError(t, store.DB().QueryRowContext(
				ctx,
				`SELECT lifecycle_policy_revision FROM sessions WHERE id = ?`,
				"migrated-session",
			).Scan(&persistedRevision))
			if tt.sharedMigrationPolicy {
				require.Equal(t, config.LifecyclePolicyRevision(v2Cfg.Lifecycle), persistedRevision)
			} else {
				require.NotEmpty(t, persistedRevision)
				require.True(t, strings.HasPrefix(persistedRevision, "v2-"),
					"the migration without a shared revision must use the legacy migration revision format")
				require.NotEqual(t, config.LifecyclePolicyRevision(v2Cfg.Lifecycle), persistedRevision,
					"an older migration revision must remain acceptable to input renewal")
			}

			manager := legacyManager
			if tt.keepCached {
				cfgStore.Swap(&v2Cfg)
			} else {
				v2ConfigStore := config.NewConfigStore(&v2Cfg, nil)
				manager, err = NewManager(ctx, nil, &v2Cfg, v2ConfigStore, store)
				require.NoError(t, err)
				manager.gcStop()
				<-manager.gcDone
			}

			eventStore := eventstore.NewSQLiteStoreWithRetention(
				store.DB(),
				store.writeMu,
				eventstore.RetentionPolicy{Content: 24 * time.Hour},
			)
			acceptedAt := now
			err = eventStore.Append(ctx, &eventstore.StoredEvent{
				SessionID: "migrated-session",
				Seq:       1,
				Type:      string(events.Input),
				Data:      []byte(`{"content":"new input"}`),
				Direction: "inbound",
				Source:    eventstore.SourceNormal,
				CreatedAt: acceptedAt.UnixMilli(),
			})
			require.NoError(t, err)

			require.NoError(t, manager.RecordInputAccepted(ctx, "migrated-session", acceptedAt))

			after, err := store.Get(ctx, "migrated-session")
			require.NoError(t, err)
			require.NotNil(t, after.LastInputAt)
			require.NotNil(t, after.ArchiveAt)
			require.NotNil(t, after.ConversationExpiresAt)
			require.NotNil(t, after.HistoryExpiresAt)
			require.NotNil(t, after.LastContentExpiresAt)
			require.True(t, after.LastInputAt.Equal(acceptedAt))
			require.True(t, after.ArchiveAt.Equal(acceptedAt.Add(7*24*time.Hour)))
			require.True(t, after.ConversationExpiresAt.Equal(acceptedAt.Add(180*24*time.Hour)))
			require.True(t, after.HistoryExpiresAt.Equal(acceptedAt.Add(180*24*time.Hour)))
			require.True(t, after.LastContentExpiresAt.Equal(acceptedAt.Add(24*time.Hour)))

			retired, err := store.RetireExpiredLifecycleSession(ctx, "migrated-session", acceptedAt.Add(2*24*time.Hour))
			require.NoError(t, err)
			require.Nil(t, retired, "a new input must keep the migrated session from retiring two days later")
		})
	}
}
