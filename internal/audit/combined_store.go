package audit

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/dbutil"
)

// NewCombinedStore returns a query-facing Store that reads the legacy and
// lifecycle-v2 chains together. New writes and maintenance operations go to
// lifecycle; callers that verify or prune the legacy chain must use its
// dedicated Store directly.
func NewCombinedStore(db *sql.DB, dialect dbutil.Dialect, legacy, lifecycle Store) (Store, error) {
	if db == nil || legacy == nil || lifecycle == nil {
		return nil, fmt.Errorf("audit: combined store requires database and both chain stores")
	}
	if dialect != legacy.Dialect() || dialect != lifecycle.Dialect() {
		return nil, fmt.Errorf("audit: combined store dialect mismatch")
	}
	return &combinedStore{
		db:        db,
		dialect:   dialect,
		legacy:    legacy,
		lifecycle: lifecycle,
	}, nil
}

type combinedStore struct {
	db        *sql.DB
	dialect   dbutil.Dialect
	legacy    Store
	lifecycle Store
}

func (s *combinedStore) BeginTx(ctx context.Context) (Tx, error) {
	return s.lifecycle.BeginTx(ctx)
}

func (s *combinedStore) Query(ctx context.Context, q Query) ([]UserActivity, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	where, args := buildActivityWhere(q)
	statement := fmt.Sprintf(
		"SELECT id, ts, user_id, user_id_type, platform, session_id, action, "+
			"resource_type, resource_id, outcome, detail_json, event_ref, "+
			"ip, user_agent, prev_hash, self_hash, ? AS chain_epoch, 0 AS expires_at FROM %s%s "+
			"UNION ALL "+
			"SELECT id, ts, user_id, user_id_type, platform, session_id, action, "+
			"resource_type, resource_id, outcome, detail_json, event_ref, "+
			"ip, user_agent, prev_hash, self_hash, ? AS chain_epoch, expires_at FROM %s%s "+
			"ORDER BY ts DESC, chain_epoch ASC, id DESC LIMIT ? OFFSET ?",
		legacyChainProfile.activityTable, where,
		lifecycleChainProfile.activityTable, where,
	)
	queryArgs := make([]any, 0, len(args)*2+4)
	queryArgs = append(queryArgs, legacyChainProfile.epoch)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, lifecycleChainProfile.epoch)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, limit, offset)

	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(statement), queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("audit: combined query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []UserActivity
	for rows.Next() {
		var ua UserActivity
		var sessionID, resourceType, resourceID, eventRef, ip, userAgent sql.NullString
		if err := rows.Scan(
			&ua.ID, &ua.Ts, &ua.UserID, &ua.UserIDType, &ua.Platform,
			&sessionID, &ua.Action, &resourceType, &resourceID,
			&ua.Outcome, &ua.DetailJSON, &eventRef, &ip, &userAgent,
			&ua.PrevHash, &ua.SelfHash, &ua.ChainEpoch, &ua.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("audit: combined scan: %w", err)
		}
		ua.SessionID = sessionID.String
		ua.ResourceType = resourceType.String
		ua.ResourceID = resourceID.String
		ua.EventRef = eventRef.String
		ua.IP = ip.String
		ua.UserAgent = userAgent.String
		results = append(results, ua)
	}
	return results, rows.Err()
}

func (s *combinedStore) Stats(ctx context.Context, q Query) (ActivityStats, error) {
	legacyStats, err := s.legacy.Stats(ctx, q)
	if err != nil {
		return ActivityStats{}, err
	}
	lifecycleStats, err := s.lifecycle.Stats(ctx, q)
	if err != nil {
		return ActivityStats{}, err
	}
	out := ActivityStats{
		Total:      legacyStats.Total + lifecycleStats.Total,
		ByOutcome:  make(map[string]int64, len(legacyStats.ByOutcome)+len(lifecycleStats.ByOutcome)),
		ByPlatform: make(map[string]int64, len(legacyStats.ByPlatform)+len(lifecycleStats.ByPlatform)),
	}
	for key, value := range legacyStats.ByOutcome {
		out.ByOutcome[key] += value
	}
	for key, value := range lifecycleStats.ByOutcome {
		out.ByOutcome[key] += value
	}
	for key, value := range legacyStats.ByPlatform {
		out.ByPlatform[key] += value
	}
	for key, value := range lifecycleStats.ByPlatform {
		out.ByPlatform[key] += value
	}
	return out, nil
}

// QueryAsc streams one chain only and is intended for the lifecycle-v2
// verifier when this wrapper is used outside Gateway wiring.
func (s *combinedStore) QueryAsc(ctx context.Context, fromID int64, limit int) ([]UserActivity, error) {
	return s.lifecycle.QueryAsc(ctx, fromID, limit)
}

func (s *combinedStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.lifecycle.DeleteBefore(ctx, cutoff)
}

func (s *combinedStore) SaveCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	return s.lifecycle.SaveCheckpoint(ctx, checkpoint)
}

func (s *combinedStore) LatestCheckpoint(ctx context.Context) (*Checkpoint, error) {
	return s.lifecycle.LatestCheckpoint(ctx)
}

func (s *combinedStore) ListIdentityLinks(ctx context.Context, principalUserID string) ([]IdentityLink, error) {
	return s.legacy.ListIdentityLinks(ctx, principalUserID)
}

func (s *combinedStore) UpsertIdentityLink(ctx context.Context, link IdentityLink) error {
	return s.legacy.UpsertIdentityLink(ctx, link)
}

func (s *combinedStore) DeleteIdentityLink(ctx context.Context, id string) error {
	return s.legacy.DeleteIdentityLink(ctx, id)
}

func (s *combinedStore) Close() error { return s.db.Close() }

func (s *combinedStore) Dialect() dbutil.Dialect { return s.dialect }

func (s *combinedStore) ChainEpoch() string { return lifecycleChainProfile.epoch }

func (s *combinedStore) UsesPersistedExpiry() bool { return true }

var _ Store = (*combinedStore)(nil)
