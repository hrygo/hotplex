package lifecycle

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/dbutil"
)

const (
	migrationPreviewTTL   = 30 * time.Minute
	maxMigrationPlans     = 16
	maxMigrationBatchSize = 500
)

var (
	ErrConfirmationRequired = errors.New("lifecycle: migration plan confirmation required")
	ErrPreviewExpired       = errors.New("lifecycle: migration preview expired or unavailable")
	ErrStalePreview         = errors.New("lifecycle: migration preview is stale")
)

// RetentionPolicy is the v2 policy used to migrate eligible legacy records.
// LegacyContentRetention preserves the effective expiry used before per-record
// deadlines were introduced.
type RetentionPolicy struct {
	ArchiveAfter           time.Duration
	ConversationRetention  time.Duration
	ContentRetention       time.Duration
	LegacyContentRetention time.Duration
	BatchSize              int
}

// MigrationSummary describes the records that would receive a new lifecycle
// policy or an extended explicit deadline. Bytes counts stored content bytes
// for message bodies; session metadata deliberately reports zero bytes.
type MigrationSummary struct {
	Count          int64      `json:"count"`
	Bytes          int64      `json:"bytes"`
	OldestDeadline *time.Time `json:"oldest_deadline,omitempty"`
}

// MigrationBlocked contains aggregate-only facts excluded from migration.
type MigrationBlocked struct {
	LegacySessionsMissingInputClock    int64 `json:"legacy_sessions_missing_input_clock"`
	OrphanedEvents                     int64 `json:"orphaned_events"`
	OrphanedTurns                      int64 `json:"orphaned_turns"`
	ContentOnSessionsMissingInputClock int64 `json:"content_on_sessions_missing_input_clock"`
	UnknownExecutionRecords            int64 `json:"unknown_execution_records"`
	ActiveExecutionRecords             int64 `json:"active_execution_records"`
}

// MigrationPreview contains no session identifiers, message text, payloads,
// or credentials. Plan IDs bind the subsequent apply to an in-memory snapshot.
type MigrationPreview struct {
	PlanID         string           `json:"plan_id"`
	CreatedAt      time.Time        `json:"created_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
	PolicyRevision string           `json:"policy_revision"`
	BatchSize      int              `json:"batch_size"`
	Sessions       MigrationSummary `json:"sessions"`
	Events         MigrationSummary `json:"events"`
	Turns          MigrationSummary `json:"turns"`
	Blocked        MigrationBlocked `json:"blocked"`
}

// MigrationApplyResult reports how many rows in each bounded batch were
// updated. It intentionally contains no row identifiers.
type MigrationApplyResult struct {
	Sessions int64 `json:"sessions"`
	Events   int64 `json:"events"`
	Turns    int64 `json:"turns"`
}

type migrationPolicySource func() RetentionPolicy
type migrationNow func() time.Time

// MigrationService previews and applies a short-lived, explicitly confirmed
// migration from legacy lifecycle clocks to v2 deadlines.
type MigrationService struct {
	db      *sql.DB
	dialect dbutil.Dialect
	policy  migrationPolicySource
	now     migrationNow

	mu    sync.Mutex
	plans map[string]*migrationPlan
}

type migrationPlan struct {
	preview MigrationPreview
	digest  string
	policy  RetentionPolicy
	rows    migrationSnapshot
}

type migrationSnapshot struct {
	digest  string
	events  []contentMigration
	turns   []contentMigration
	preview MigrationPreview
}

type sessionMigration struct {
	id                    string
	state                 string
	lastInput             time.Time
	lifecycleRevision     string
	oldArchive            sql.NullTime
	oldConversationExpiry sql.NullTime
	oldLastContentExpiry  sql.NullTime
	oldHistoryExpiry      sql.NullTime
	archive               time.Time
	conversationExpiry    time.Time
	lastContentExpiry     time.Time
	historyExpiry         time.Time
}

type contentMigration struct {
	id          int64
	sessionID   string
	createdAtMS int64
	oldExpiryMS int64
	newExpiryMS int64
}

type migrationQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewMigrationService constructs a migration service. Configuration is read
// for each preview and apply so a policy reload invalidates old plans.
func NewMigrationService(
	db *sql.DB,
	dialect dbutil.Dialect,
	policy func() RetentionPolicy,
	now func() time.Time,
) *MigrationService {
	if now == nil {
		now = time.Now
	}
	return &MigrationService{
		db:      db,
		dialect: dialect,
		policy:  policy,
		now:     now,
		plans:   make(map[string]*migrationPlan),
	}
}

// Preview creates an aggregate-only report and a bounded, short-lived plan.
func (s *MigrationService) Preview(ctx context.Context) (*MigrationPreview, error) {
	if s == nil || s.db == nil || s.policy == nil {
		return nil, errors.New("lifecycle: migration service is unavailable")
	}
	policy := normalizeMigrationPolicy(s.policy())
	if err := validateRetentionPolicy(policy); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: begin migration preview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	snapshot, err := s.readSnapshot(ctx, tx, policy)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lifecycle: finish migration preview: %w", err)
	}

	now := s.now().UTC()
	snapshot.preview.PlanID = uuid.NewString()
	snapshot.preview.CreatedAt = now
	snapshot.preview.ExpiresAt = now.Add(migrationPreviewTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunePlansLocked(now)
	if len(s.plans) >= maxMigrationPlans {
		s.removeOldestPlanLocked()
	}
	s.plans[snapshot.preview.PlanID] = &migrationPlan{
		preview: snapshot.preview,
		digest:  snapshot.digest,
		policy:  policy,
		rows:    snapshot,
	}
	preview := snapshot.preview
	return &preview, nil
}

// Apply requires the preview's exact plan ID to be submitted twice. It
// revalidates the entire aggregate snapshot in a serializable transaction and
// then updates at most BatchSize rows per data type.
func (s *MigrationService) Apply(
	ctx context.Context,
	planID string,
	confirmPlanID string,
) (*MigrationApplyResult, error) {
	if strings.TrimSpace(planID) == "" || planID != confirmPlanID {
		return nil, ErrConfirmationRequired
	}
	if s == nil || s.db == nil || s.policy == nil {
		return nil, errors.New("lifecycle: migration service is unavailable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.prunePlansLocked(now)
	plan, ok := s.plans[planID]
	if !ok || !now.Before(plan.preview.ExpiresAt) {
		delete(s.plans, planID)
		return nil, ErrPreviewExpired
	}

	policy := normalizeMigrationPolicy(s.policy())
	if err := validateRetentionPolicy(policy); err != nil {
		return nil, err
	}
	if policy != plan.policy {
		delete(s.plans, planID)
		return nil, ErrStalePreview
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: begin migration apply: %w", err)
	}
	rollback := func(cause error) (*MigrationApplyResult, error) {
		_ = tx.Rollback()
		return nil, cause
	}

	current, err := s.readSnapshot(ctx, tx, policy)
	if err != nil {
		return rollback(err)
	}
	if current.digest != plan.digest {
		delete(s.plans, planID)
		return rollback(ErrStalePreview)
	}

	result, err := applySnapshot(ctx, tx, s.dialect, policy, plan.rows)
	if err != nil {
		if errors.Is(err, ErrStalePreview) {
			delete(s.plans, planID)
		}
		return rollback(err)
	}
	if normalizeMigrationPolicy(s.policy()) != policy {
		delete(s.plans, planID)
		return rollback(ErrStalePreview)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lifecycle: commit migration apply: %w", err)
	}
	delete(s.plans, planID)
	return result, nil
}

func (s *MigrationService) readSnapshot(
	ctx context.Context,
	q migrationQueryer,
	policy RetentionPolicy,
) (migrationSnapshot, error) {
	snapshot := migrationSnapshot{
		preview: MigrationPreview{
			PolicyRevision: retentionPolicyRevision(policy),
			BatchSize:      policy.BatchSize,
		},
	}
	digest := sha256.New()
	writeDigest(digest, snapshot.preview.PolicyRevision)

	if err := readBlocked(ctx, q, s.dialect, &snapshot.preview.Blocked, digest); err != nil {
		return migrationSnapshot{}, err
	}
	if err := readSessionCandidates(ctx, q, s.dialect, policy, &snapshot.preview.Sessions, digest); err != nil {
		return migrationSnapshot{}, err
	}
	if err := readContentCandidates(ctx, q, s.dialect, policy, "events", `length(CAST(data AS BLOB))`, `octet_length(data)`, &snapshot.preview.Events, &snapshot.events, digest); err != nil {
		return migrationSnapshot{}, err
	}
	if err := readContentCandidates(ctx, q, s.dialect, policy, "turns",
		`length(CAST(content AS BLOB)) + length(CAST(COALESCE(tools_json, '') AS BLOB))`,
		`octet_length(content) + octet_length(COALESCE(tools_json, ''))`,
		&snapshot.preview.Turns, &snapshot.turns, digest); err != nil {
		return migrationSnapshot{}, err
	}

	snapshot.digest = hex.EncodeToString(digest.Sum(nil))
	return snapshot, nil
}

func readBlocked(
	ctx context.Context,
	q migrationQueryer,
	dialect dbutil.Dialect,
	blocked *MigrationBlocked,
	digest hash.Hash,
) error {
	counts := []struct {
		query string
		dst   *int64
		name  string
	}{
		{
			query: `SELECT COUNT(*) FROM sessions
				WHERE lifecycle_policy = 'legacy'
				  AND state <> 'deleted' AND deleted_at IS NULL AND last_input_at IS NULL`,
			dst:  &blocked.LegacySessionsMissingInputClock,
			name: "missing_input_clock",
		},
		{
			query: `SELECT COUNT(*) FROM events e LEFT JOIN sessions s ON s.id = e.session_id
				WHERE s.id IS NULL`,
			dst:  &blocked.OrphanedEvents,
			name: "orphaned_events",
		},
		{
			query: `SELECT COUNT(*) FROM turns t LEFT JOIN sessions s ON s.id = t.session_id
				WHERE s.id IS NULL`,
			dst:  &blocked.OrphanedTurns,
			name: "orphaned_turns",
		},
		{
			query: `SELECT
				(SELECT COUNT(*) FROM events e JOIN sessions s ON s.id = e.session_id
				 WHERE s.lifecycle_policy = 'legacy' AND s.state <> 'deleted'
				   AND s.deleted_at IS NULL AND s.last_input_at IS NULL)
				+
				(SELECT COUNT(*) FROM turns t JOIN sessions s ON s.id = t.session_id
				 WHERE s.lifecycle_policy = 'legacy' AND s.state <> 'deleted'
				   AND s.deleted_at IS NULL AND s.last_input_at IS NULL)`,
			dst:  &blocked.ContentOnSessionsMissingInputClock,
			name: "content_missing_clock",
		},
		{
			query: `SELECT COUNT(*) FROM execution_inputs
				WHERE status = 'unknown' OR runtime_status = 'unknown' OR fence_reason <> ''`,
			dst:  &blocked.UnknownExecutionRecords,
			name: "unknown_execution_records",
		},
		{
			query: `SELECT COUNT(*) FROM execution_inputs
				WHERE runtime_status IN ('pending', 'running') OR fence_reason <> ''`,
			dst:  &blocked.ActiveExecutionRecords,
			name: "active_execution_records",
		},
	}
	for _, item := range counts {
		if err := q.QueryRowContext(ctx, dialect.Rebind(item.query)).Scan(item.dst); err != nil {
			return fmt.Errorf("lifecycle: count %s: %w", item.name, err)
		}
		writeDigest(digest, item.name, strconv.FormatInt(*item.dst, 10))
	}
	return nil
}

func readSessionCandidates(
	ctx context.Context,
	q migrationQueryer,
	dialect dbutil.Dialect,
	policy RetentionPolicy,
	summary *MigrationSummary,
	digest hash.Hash,
) error {
	rows, err := q.QueryContext(ctx, dialect.Rebind(`
		SELECT id, state, last_input_at, lifecycle_policy_revision,
		       archive_at, conversation_expires_at, last_content_expires_at, history_expires_at
		FROM sessions
		WHERE lifecycle_policy = 'legacy'
		  AND state <> 'deleted' AND deleted_at IS NULL AND last_input_at IS NOT NULL
		ORDER BY last_input_at ASC, id ASC`))
	if err != nil {
		return fmt.Errorf("lifecycle: list legacy sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var candidate sessionMigration
		if err := rows.Scan(
			&candidate.id,
			&candidate.state,
			&candidate.lastInput,
			&candidate.lifecycleRevision,
			&candidate.oldArchive,
			&candidate.oldConversationExpiry,
			&candidate.oldLastContentExpiry,
			&candidate.oldHistoryExpiry,
		); err != nil {
			return fmt.Errorf("lifecycle: scan legacy session: %w", err)
		}
		candidate.lastInput = candidate.lastInput.UTC()
		candidate.archive = maxTime(candidate.oldArchive, candidate.lastInput.Add(policy.ArchiveAfter))
		candidate.conversationExpiry = maxTime(candidate.oldConversationExpiry, candidate.lastInput.Add(policy.ConversationRetention))
		writeDigest(digest,
			"session",
			candidate.id,
			candidate.state,
			candidate.lastInput.Format(time.RFC3339Nano),
			candidate.lifecycleRevision,
			nullTimeString(candidate.oldArchive),
			nullTimeString(candidate.oldConversationExpiry),
			nullTimeString(candidate.oldLastContentExpiry),
			nullTimeString(candidate.oldHistoryExpiry),
		)
		summary.Count++
		updateOldestDeadline(summary, candidate.conversationExpiry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lifecycle: iterate legacy sessions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("lifecycle: close legacy sessions: %w", err)
	}
	return nil
}

func readContentCandidates(
	ctx context.Context,
	q migrationQueryer,
	dialect dbutil.Dialect,
	policy RetentionPolicy,
	table string,
	sqliteBytesExpr string,
	postgresBytesExpr string,
	summary *MigrationSummary,
	selected *[]contentMigration,
	digest hash.Hash,
) error {
	byteExpr := sqliteBytesExpr
	if dialect == dbutil.DialectPostgres {
		byteExpr = postgresBytesExpr
	}
	query := `SELECT c.id, c.session_id, c.created_at, c.expires_at, ` + byteExpr + `
		FROM ` + table + ` c
		JOIN sessions s ON s.id = c.session_id
		WHERE s.lifecycle_policy = 'legacy'
		  AND s.state <> 'deleted' AND s.deleted_at IS NULL AND s.last_input_at IS NOT NULL
		ORDER BY c.id ASC`
	rows, err := q.QueryContext(ctx, dialect.Rebind(query))
	if err != nil {
		return fmt.Errorf("lifecycle: list legacy %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, createdAtMS, expiryMS, byteCount int64
		var sessionID string
		if err := rows.Scan(&id, &sessionID, &createdAtMS, &expiryMS, &byteCount); err != nil {
			return fmt.Errorf("lifecycle: scan legacy %s: %w", table, err)
		}
		newExpiryMS := migratedContentExpiry(createdAtMS, expiryMS, policy)
		needsUpdate := expiryMS == 0 || expiryMS < createdAtMS+durationMillis(policy.ContentRetention)
		writeDigest(digest,
			table,
			strconv.FormatInt(id, 10),
			sessionID,
			strconv.FormatInt(createdAtMS, 10),
			strconv.FormatInt(expiryMS, 10),
			strconv.FormatInt(byteCount, 10),
			strconv.FormatInt(newExpiryMS, 10),
		)
		if needsUpdate {
			summary.Count++
			summary.Bytes += byteCount
			updateOldestDeadline(summary, time.UnixMilli(newExpiryMS).UTC())
			if len(*selected) < policy.BatchSize {
				*selected = append(*selected, contentMigration{
					id:          id,
					sessionID:   sessionID,
					createdAtMS: createdAtMS,
					oldExpiryMS: expiryMS,
					newExpiryMS: newExpiryMS,
				})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lifecycle: iterate legacy %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("lifecycle: close legacy %s: %w", table, err)
	}

	return nil
}

func readSelectedSessionContentExpiry(
	ctx context.Context,
	q migrationQueryer,
	dialect dbutil.Dialect,
	policy RetentionPolicy,
	sessions []sessionMigration,
) error {
	if len(sessions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(sessions))
	for _, candidate := range sessions {
		ids = append(ids, candidate.id)
	}
	sort.Strings(ids)
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "(?)"
	}
	query := `WITH selected_sessions(session_id) AS (VALUES ` + strings.Join(placeholders, ",") + `),
	content AS (
		SELECT e.session_id, ` + effectiveContentExpiry("e") + ` AS expiry_ms
		FROM events e JOIN selected_sessions s ON s.session_id = e.session_id
		UNION ALL
		SELECT t.session_id, ` + effectiveContentExpiry("t") + ` AS expiry_ms
		FROM turns t JOIN selected_sessions s ON s.session_id = t.session_id
	)
	SELECT session_id, MAX(expiry_ms) FROM content GROUP BY session_id`
	// Session IDs appear once in the CTE, followed by each branch's duration
	// placeholders. Keeping the placeholder count bounded also supports older
	// SQLite builds with a 999-variable limit.
	durationArgs := durationArgs(policy)
	orderedArgs := make([]any, 0, len(ids)+len(durationArgs)*2)
	for _, id := range ids {
		orderedArgs = append(orderedArgs, id)
	}
	orderedArgs = append(orderedArgs, durationArgs...)
	orderedArgs = append(orderedArgs, durationArgs...)
	rows, err := q.QueryContext(ctx, dialect.Rebind(query), orderedArgs...)
	if err != nil {
		return fmt.Errorf("lifecycle: summarize selected session content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	latest := make(map[string]time.Time, len(sessions))
	for rows.Next() {
		var sessionID string
		var expiryMS int64
		if err := rows.Scan(&sessionID, &expiryMS); err != nil {
			return fmt.Errorf("lifecycle: scan selected session content: %w", err)
		}
		latest[sessionID] = time.UnixMilli(expiryMS).UTC()
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lifecycle: iterate selected session content: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("lifecycle: close selected session content: %w", err)
	}
	for i := range sessions {
		if expiry, ok := latest[sessions[i].id]; ok {
			sessions[i].lastContentExpiry = expiry
		}
	}
	return nil
}

func applySnapshot(
	ctx context.Context,
	tx *sql.Tx,
	dialect dbutil.Dialect,
	policy RetentionPolicy,
	snapshot migrationSnapshot,
) (*MigrationApplyResult, error) {
	result := &MigrationApplyResult{}
	revision := retentionPolicyRevision(policy)
	for _, candidate := range snapshot.events {
		updated, err := tx.ExecContext(ctx, dialect.Rebind(`
			UPDATE events SET expires_at = ?
			WHERE id = ? AND session_id = ? AND created_at = ? AND expires_at = ?`),
			candidate.newExpiryMS, candidate.id, candidate.sessionID, candidate.createdAtMS, candidate.oldExpiryMS,
		)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: migrate event deadline: %w", err)
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("lifecycle: event rows affected: %w", err)
		}
		if rows != 1 {
			return nil, ErrStalePreview
		}
		result.Events++
	}
	for _, candidate := range snapshot.turns {
		updated, err := tx.ExecContext(ctx, dialect.Rebind(`
			UPDATE turns SET expires_at = ?
			WHERE id = ? AND session_id = ? AND created_at = ? AND expires_at = ?`),
			candidate.newExpiryMS, candidate.id, candidate.sessionID, candidate.createdAtMS, candidate.oldExpiryMS,
		)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: migrate turn deadline: %w", err)
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("lifecycle: turn rows affected: %w", err)
		}
		if rows != 1 {
			return nil, ErrStalePreview
		}
		result.Turns++
	}

	// Do not switch a session to v2 until every existing content row already
	// has an explicit deadline at least as long as the effective legacy/v2
	// retention. Otherwise a bounded content batch could leave old rows subject
	// to the shorter legacy TTL after the session root has moved to v2.
	sessions, err := readReadySessionCandidates(ctx, tx, dialect, policy)
	if err != nil {
		return nil, err
	}
	if err := readSelectedSessionContentExpiry(ctx, tx, dialect, policy, sessions); err != nil {
		return nil, err
	}
	for _, candidate := range sessions {
		candidate.archive = maxTime(candidate.oldArchive, candidate.lastInput.Add(policy.ArchiveAfter))
		candidate.conversationExpiry = maxTime(candidate.oldConversationExpiry, candidate.lastInput.Add(policy.ConversationRetention))
		candidate.lastContentExpiry = maxTime(candidate.oldLastContentExpiry, candidate.lastContentExpiry)
		candidate.historyExpiry = maxTime(candidate.oldHistoryExpiry, candidate.conversationExpiry)
		candidate.historyExpiry = maxTime(
			sql.NullTime{Time: candidate.lastContentExpiry, Valid: !candidate.lastContentExpiry.IsZero()},
			candidate.historyExpiry,
		)
		updated, err := tx.ExecContext(ctx, dialect.Rebind(`
			UPDATE sessions
			SET lifecycle_policy = 'v2',
			    lifecycle_policy_revision = ?,
			    archive_at = ?,
			    conversation_expires_at = ?,
			    last_content_expires_at = ?,
			    history_expires_at = ?
			WHERE id = ? AND lifecycle_policy = 'legacy'
			  AND state = ? AND last_input_at = ? AND deleted_at IS NULL`),
			revision,
			candidate.archive,
			candidate.conversationExpiry,
			nullableTime(candidate.lastContentExpiry),
			candidate.historyExpiry,
			candidate.id,
			candidate.state,
			candidate.lastInput,
		)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: migrate session: %w", err)
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("lifecycle: session rows affected: %w", err)
		}
		if rows != 1 {
			return nil, ErrStalePreview
		}
		result.Sessions++
	}
	return result, nil
}

func readReadySessionCandidates(
	ctx context.Context,
	q migrationQueryer,
	dialect dbutil.Dialect,
	policy RetentionPolicy,
) ([]sessionMigration, error) {
	// For a content row, migratedContentExpiry preserves the later of the
	// configured legacy and v2 deadlines. A session is ready only when all
	// existing event/turn rows have reached at least that effective deadline.
	contentRetention := maxDuration(policy.ContentRetention, policy.LegacyContentRetention)
	contentRetentionMS := durationMillis(contentRetention)
	rows, err := q.QueryContext(ctx, dialect.Rebind(`
		SELECT s.id, s.state, s.last_input_at, s.lifecycle_policy_revision,
		       s.archive_at, s.conversation_expires_at, s.last_content_expires_at, s.history_expires_at
		FROM sessions s
		WHERE s.lifecycle_policy = 'legacy'
		  AND s.state <> 'deleted' AND s.deleted_at IS NULL AND s.last_input_at IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM events e
		      WHERE e.session_id = s.id AND e.expires_at < e.created_at + ?
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM turns t
		      WHERE t.session_id = s.id AND t.expires_at < t.created_at + ?
		  )
		ORDER BY s.last_input_at ASC, s.id ASC
		LIMIT ?`),
		contentRetentionMS, contentRetentionMS, policy.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: list sessions with migrated content: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := make([]sessionMigration, 0, policy.BatchSize)
	for rows.Next() {
		var candidate sessionMigration
		if err := rows.Scan(
			&candidate.id,
			&candidate.state,
			&candidate.lastInput,
			&candidate.lifecycleRevision,
			&candidate.oldArchive,
			&candidate.oldConversationExpiry,
			&candidate.oldLastContentExpiry,
			&candidate.oldHistoryExpiry,
		); err != nil {
			return nil, fmt.Errorf("lifecycle: scan session with migrated content: %w", err)
		}
		candidate.lastInput = candidate.lastInput.UTC()
		sessions = append(sessions, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lifecycle: iterate sessions with migrated content: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("lifecycle: close sessions with migrated content: %w", err)
	}
	return sessions, nil
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func effectiveContentExpiry(alias string) string {
	return `CASE
		WHEN ` + alias + `.expires_at > 0 THEN
			CASE WHEN ` + alias + `.expires_at > ` + alias + `.created_at + ? THEN ` + alias + `.expires_at
			     ELSE ` + alias + `.created_at + ? END
		WHEN ` + alias + `.created_at + ? > ` + alias + `.created_at + ? THEN ` + alias + `.created_at + ?
		ELSE ` + alias + `.created_at + ? END`
}

func durationArgs(policy RetentionPolicy) []any {
	content := durationMillis(policy.ContentRetention)
	legacy := durationMillis(policy.LegacyContentRetention)
	return []any{content, content, legacy, content, legacy, content}
}

func migratedContentExpiry(createdAtMS, oldExpiryMS int64, policy RetentionPolicy) int64 {
	target := createdAtMS + durationMillis(policy.ContentRetention)
	if oldExpiryMS > 0 {
		if oldExpiryMS > target {
			return oldExpiryMS
		}
		return target
	}
	legacy := createdAtMS + durationMillis(policy.LegacyContentRetention)
	if legacy > target {
		return legacy
	}
	return target
}

func validateRetentionPolicy(policy RetentionPolicy) error {
	switch {
	case policy.ArchiveAfter <= 0:
		return errors.New("lifecycle: archive retention must be positive")
	case policy.ConversationRetention <= 0:
		return errors.New("lifecycle: conversation retention must be positive")
	case policy.ArchiveAfter > policy.ConversationRetention:
		return errors.New("lifecycle: archive retention must not exceed conversation retention")
	case policy.ContentRetention <= 0:
		return errors.New("lifecycle: content retention must be positive")
	case policy.LegacyContentRetention <= 0:
		return errors.New("lifecycle: legacy content retention must be positive")
	case policy.BatchSize <= 0:
		return errors.New("lifecycle: migration batch size must be positive")
	case policy.BatchSize > maxMigrationBatchSize:
		return fmt.Errorf("lifecycle: migration batch size must not exceed %d", maxMigrationBatchSize)
	}
	return nil
}

func normalizeMigrationPolicy(policy RetentionPolicy) RetentionPolicy {
	if policy.BatchSize > maxMigrationBatchSize {
		policy.BatchSize = maxMigrationBatchSize
	}
	return policy
}

func retentionPolicyRevision(policy RetentionPolicy) string {
	input := fmt.Sprintf(
		"v2|archive=%d|conversation=%d|content=%d|legacy=%d|batch=%d",
		policy.ArchiveAfter,
		policy.ConversationRetention,
		policy.ContentRetention,
		policy.LegacyContentRetention,
		policy.BatchSize,
	)
	sum := sha256.Sum256([]byte(input))
	return "v2-" + hex.EncodeToString(sum[:8])
}

func durationMillis(duration time.Duration) int64 {
	return duration.Milliseconds()
}

func maxTime(old sql.NullTime, target time.Time) time.Time {
	if old.Valid && old.Time.After(target) {
		return old.Time.UTC()
	}
	return target.UTC()
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func nullTimeString(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
}

func updateOldestDeadline(summary *MigrationSummary, candidate time.Time) {
	candidate = candidate.UTC()
	if summary.OldestDeadline == nil || candidate.Before(*summary.OldestDeadline) {
		deadline := candidate
		summary.OldestDeadline = &deadline
	}
}

func writeDigest(digest hash.Hash, values ...string) {
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
}

func (s *MigrationService) prunePlansLocked(now time.Time) {
	for id, plan := range s.plans {
		if !now.Before(plan.preview.ExpiresAt) {
			delete(s.plans, id)
		}
	}
}

func (s *MigrationService) removeOldestPlanLocked() {
	var oldestID string
	var oldestTime time.Time
	for id, plan := range s.plans {
		if oldestID == "" || plan.preview.CreatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = plan.preview.CreatedAt
		}
	}
	if oldestID != "" {
		delete(s.plans, oldestID)
	}
}
