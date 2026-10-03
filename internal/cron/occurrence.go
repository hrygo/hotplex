package cron

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/sqlutil"
)

// TriggerKind identifies what caused a cron firing.
type TriggerKind string

const (
	TriggerScheduled TriggerKind = "scheduled"
	TriggerManual    TriggerKind = "manual"
	TriggerWebhook   TriggerKind = "webhook"
)

// OccurrenceStatus is the lifecycle of one durable cron firing.
type OccurrenceStatus string

const (
	// OccurrenceAccepted means the trigger was durably claimed but no Worker
	// has been dispatched yet.
	OccurrenceAccepted OccurrenceStatus = "accepted"
	// OccurrenceStarted means an execution was dispatched.
	OccurrenceStarted OccurrenceStatus = "started"
	// OccurrenceCompleted means the run finished with a known outcome.
	OccurrenceCompleted OccurrenceStatus = "completed"
	// OccurrenceFailed means the run failed with a known outcome.
	OccurrenceFailed OccurrenceStatus = "failed"
	// OccurrenceUnknown means the outcome cannot be determined. It must never
	// be treated as safe to re-run: the Agent may already have had an effect.
	OccurrenceUnknown OccurrenceStatus = "unknown"
)

// ErrTriggerIdentityIncomplete is returned when a trigger cannot produce a
// stable business key. An incomplete identity must never be papered over with a
// timestamp, because that would silently defeat deduplication.
var ErrTriggerIdentityIncomplete = errors.New("cron occurrence: trigger identity incomplete")

// ErrOccurrenceNotFound is returned when an occurrence is not in the store.
var ErrOccurrenceNotFound = errors.New("cron occurrence: not found")

// TriggerIdentity is the stable business identity of one firing.
type TriggerIdentity struct {
	Kind TriggerKind
	// JobID is the owning cron job.
	JobID string
	// ScheduleRev fingerprints the schedule at firing time so an edited
	// schedule cannot collide with a pre-edit occurrence.
	ScheduleRev string
	// ScheduledAtMs is the scheduled UTC instant in Unix ms.
	ScheduledAtMs int64
	// Nonce identifies one manual trigger request.
	Nonce string
	// SourceID is the verified webhook source/event ID.
	SourceID string
}

// Key derives the stable business key for a firing.
//
// Each trigger kind requires exactly the source of truth that identifies it:
// a scheduled firing needs its instant, a manual firing needs the request
// nonce, and a webhook firing needs a verified source/event ID. A webhook
// without a verified ID cannot be deduplicated end to end, so it is rejected
// rather than silently given a weaker key.
func (t TriggerIdentity) Key() (string, error) {
	if t.JobID == "" {
		return "", fmt.Errorf("%w: missing job id", ErrTriggerIdentityIncomplete)
	}
	switch t.Kind {
	case TriggerScheduled:
		if t.ScheduledAtMs <= 0 {
			return "", fmt.Errorf("%w: scheduled firing needs a scheduled instant",
				ErrTriggerIdentityIncomplete)
		}
		return "sched|" + t.JobID + "|" + t.ScheduleRev + "|" +
			strconv.FormatInt(t.ScheduledAtMs, 10), nil
	case TriggerManual:
		if t.Nonce == "" {
			return "", fmt.Errorf("%w: manual firing needs a request nonce",
				ErrTriggerIdentityIncomplete)
		}
		return "manual|" + t.JobID + "|" + t.Nonce, nil
	case TriggerWebhook:
		if t.SourceID == "" {
			return "", fmt.Errorf("%w: webhook firing needs a verified source id",
				ErrTriggerIdentityIncomplete)
		}
		return "webhook|" + t.JobID + "|" + t.SourceID, nil
	default:
		return "", fmt.Errorf("%w: unknown trigger kind %q",
			ErrTriggerIdentityIncomplete, t.Kind)
	}
}

// ScheduleRevision fingerprints a schedule so that occurrences from different
// schedule definitions never share a key.
func ScheduleRevision(s CronSchedule) string {
	canonical := strings.Join([]string{
		string(s.Kind), s.At, strconv.FormatInt(s.EveryMs, 10), s.Expr, s.TZ,
	}, "|")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// GenerateOccurrenceID returns a fresh surrogate occurrence ID. The surrogate
// is never the idempotency boundary — that is TriggerKey plus Generation.
func GenerateOccurrenceID() string {
	return "occ_" + uuid.New().String()
}

// ScheduledTriggerFor builds the identity of a timer-driven firing. The
// scheduled instant is the job's persisted next_run_at_ms, so re-collecting the
// same due job derives the same occurrence instead of a second run.
func ScheduledTriggerFor(job *CronJob) TriggerIdentity {
	return TriggerIdentity{
		Kind:          TriggerScheduled,
		JobID:         job.ID,
		ScheduleRev:   ScheduleRevision(job.Schedule),
		ScheduledAtMs: job.State.NextRunAtMs,
	}
}

// RequestedTriggerFor builds the identity of an operator- or webhook-driven
// firing.
//
// A webhook only deduplicates when the caller supplies a verified event ID.
// Without one there is no stable source to key on, so the firing is recorded as
// a manual (per-request) trigger rather than claiming webhook deduplication
// that cannot be honored.
func RequestedTriggerFor(job *CronJob, nonce, verifiedEventID string) TriggerIdentity {
	if job.PlatformKey["trigger"] == "webhook" && verifiedEventID != "" {
		return TriggerIdentity{
			Kind:     TriggerWebhook,
			JobID:    job.ID,
			SourceID: verifiedEventID,
		}
	}
	return TriggerIdentity{
		Kind:  TriggerManual,
		JobID: job.ID,
		Nonce: nonce,
	}
}

// Occurrence is the durable record of one cron firing.
//
// Content-free: it stores identity and lifecycle only. The prompt body, worker
// output and provider payloads are never written here.
type Occurrence struct {
	OccurrenceID string
	// TriggerKey plus Generation form the idempotency boundary. A retry or an
	// operator rerun uses an explicit new generation; it must never mutate the
	// key, because that would let a second run masquerade as the first.
	TriggerKey  string
	Generation  int64
	JobID       string
	TriggerKind TriggerKind
	ScheduleRev string
	// ScheduledAtMs is 0 for non-scheduled triggers.
	ScheduledAtMs int64
	Nonce         string
	SourceID      string
	SessionID     string
	ExecutionID   string
	// DeliveryMode is the owner recorded when this occurrence was claimed.
	// It is a fact about THIS firing: changing the job's mode later cannot
	// retroactively hand an in-flight run to a different owner.
	DeliveryMode DeliveryMode
	Status       OccurrenceStatus
	ErrorCode    string
	CreatedAtMs  int64
	UpdatedAtMs  int64
	StartedAtMs  *int64
	FinishedAtMs *int64
}

// NewOccurrence builds an accepted occurrence from a trigger identity and the
// delivery owner resolved for this firing.
//
// The mode is resolved by the caller and recorded here, so the occurrence
// carries the owner this run actually started under rather than whatever the
// job says later.
func NewOccurrence(identity TriggerIdentity, mode DeliveryMode, now time.Time) (*Occurrence, error) {
	key, err := identity.Key()
	if err != nil {
		return nil, err
	}
	return &Occurrence{
		OccurrenceID:  GenerateOccurrenceID(),
		TriggerKey:    key,
		Generation:    0,
		JobID:         identity.JobID,
		TriggerKind:   identity.Kind,
		ScheduleRev:   identity.ScheduleRev,
		ScheduledAtMs: identity.ScheduledAtMs,
		Nonce:         identity.Nonce,
		SourceID:      identity.SourceID,
		DeliveryMode:  ResolveDeliveryMode(mode),
		Status:        OccurrenceAccepted,
		CreatedAtMs:   now.UnixMilli(),
		UpdatedAtMs:   now.UnixMilli(),
	}, nil
}

// OccurrenceStore persists cron occurrences.
type OccurrenceStore interface {
	// Claim durably records the occurrence. It returns the stored occurrence
	// and created=false when an occurrence with the same trigger key and
	// generation already exists, so a repeated trigger never starts a second
	// run. Claim must be atomic under concurrency.
	Claim(ctx context.Context, occ *Occurrence) (stored *Occurrence, created bool, err error)
	// Get returns the occurrence with the given trigger key and generation.
	Get(ctx context.Context, triggerKey string, generation int64) (*Occurrence, error)
	// GetByID returns the occurrence by its surrogate ID.
	GetByID(ctx context.Context, occurrenceID string) (*Occurrence, error)
	// BindSession records the session an occurrence was dispatched into, so a
	// duplicate trigger can return the original run instead of a new one.
	BindSession(ctx context.Context, occurrenceID, sessionID string) error
	// UpdateStatus records a lifecycle transition.
	UpdateStatus(ctx context.Context, occurrenceID string, status OccurrenceStatus, errCode string, at time.Time) error
	// ListByJob returns occurrences for a job, newest first.
	ListByJob(ctx context.Context, jobID string, limit int) ([]*Occurrence, error)
}

const occurrenceColumns = `occurrence_id, trigger_key, generation, job_id, trigger_kind,
		schedule_rev, scheduled_at_ms, nonce, source_id, session_id, execution_id,
		delivery_mode, status, error_code, created_at, updated_at, started_at, finished_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOccurrence(row rowScanner) (*Occurrence, error) {
	var (
		occ       Occurrence
		startedAt sql.NullInt64
		doneAt    sql.NullInt64
	)
	err := row.Scan(
		&occ.OccurrenceID, &occ.TriggerKey, &occ.Generation, &occ.JobID, &occ.TriggerKind,
		&occ.ScheduleRev, &occ.ScheduledAtMs, &occ.Nonce, &occ.SourceID,
		&occ.SessionID, &occ.ExecutionID, &occ.DeliveryMode, &occ.Status, &occ.ErrorCode,
		&occ.CreatedAtMs, &occ.UpdatedAtMs, &startedAt, &doneAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOccurrenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cron occurrence: scan: %w", err)
	}
	if startedAt.Valid {
		v := startedAt.Int64
		occ.StartedAtMs = &v
	}
	if doneAt.Valid {
		v := doneAt.Int64
		occ.FinishedAtMs = &v
	}
	return &occ, nil
}

// SQLiteOccurrenceStore implements OccurrenceStore on SQLite.
type SQLiteOccurrenceStore struct {
	db      *sql.DB
	log     *slog.Logger
	writeMu *sqlutil.WriteMu
}

// NewSQLiteOccurrenceStore creates a SQLite-backed occurrence store.
func NewSQLiteOccurrenceStore(db *sql.DB, log *slog.Logger, writeMu *sqlutil.WriteMu) *SQLiteOccurrenceStore {
	return &SQLiteOccurrenceStore{
		db:      db,
		log:     log.With("component", "cron_occurrence_store"),
		writeMu: writeMu,
	}
}

// Claim inserts the occurrence unless its trigger key and generation are
// already taken. The insert runs under the write mutex so two concurrent
// triggers of the same firing serialize and exactly one creates the row.
func (s *SQLiteOccurrenceStore) Claim(
	ctx context.Context, occ *Occurrence,
) (*Occurrence, bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	normalizeOccurrenceMode(occ)

	var (
		stored  *Occurrence
		created bool
	)
	err := s.writeMu.WithLock(func() error {
		res, err := s.db.ExecContext(ctx, `INSERT INTO cron_occurrences (`+occurrenceColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(trigger_key, generation) DO NOTHING`,
			occ.OccurrenceID, occ.TriggerKey, occ.Generation, occ.JobID, occ.TriggerKind,
			occ.ScheduleRev, occ.ScheduledAtMs, occ.Nonce, occ.SourceID,
			occ.SessionID, occ.ExecutionID, occ.DeliveryMode, occ.Status, occ.ErrorCode,
			occ.CreatedAtMs, occ.UpdatedAtMs, occ.StartedAtMs, occ.FinishedAtMs,
		)
		if err != nil {
			return fmt.Errorf("cron occurrence: claim: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("cron occurrence: claim rows: %w", err)
		}
		created = n > 0
		stored, err = scanOccurrence(s.db.QueryRowContext(ctx,
			`SELECT `+occurrenceColumns+` FROM cron_occurrences
			 WHERE trigger_key = ? AND generation = ?`,
			occ.TriggerKey, occ.Generation))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// normalizeOccurrenceMode resolves the delivery owner at the persistence
// boundary.
//
// The column is constrained, and a zero-valued Occurrence built by hand would
// otherwise fail the write with a bare constraint error. Normalizing here
// means the stored fact is always one the rest of the pipeline can read,
// whoever constructed the value.
func normalizeOccurrenceMode(occ *Occurrence) {
	if occ != nil {
		occ.DeliveryMode = ResolveDeliveryMode(occ.DeliveryMode)
	}
}

func (s *SQLiteOccurrenceStore) Get(
	ctx context.Context, triggerKey string, generation int64,
) (*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return scanOccurrence(s.db.QueryRowContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences
		 WHERE trigger_key = ? AND generation = ?`,
		triggerKey, generation))
}

func (s *SQLiteOccurrenceStore) GetByID(
	ctx context.Context, occurrenceID string,
) (*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return scanOccurrence(s.db.QueryRowContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences WHERE occurrence_id = ?`,
		occurrenceID))
}

func (s *SQLiteOccurrenceStore) BindSession(
	ctx context.Context, occurrenceID, sessionID string,
) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE cron_occurrences SET session_id = ?, updated_at = updated_at
		 WHERE occurrence_id = ?`,
		sessionID, occurrenceID)
	if err != nil {
		return fmt.Errorf("cron occurrence: bind session: %w", err)
	}
	return nil
}

func (s *SQLiteOccurrenceStore) UpdateStatus(
	ctx context.Context,
	occurrenceID string,
	status OccurrenceStatus,
	errCode string,
	at time.Time,
) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	// started_at is stamped on the first transition into started, and
	// finished_at on any terminal transition, so both survive re-reporting.
	_, err := s.db.ExecContext(ctx, `UPDATE cron_occurrences SET
		status = ?,
		error_code = ?,
		updated_at = ?,
		started_at = CASE WHEN ? = 'started' AND started_at IS NULL THEN ? ELSE started_at END,
		finished_at = CASE WHEN ? IN ('completed', 'failed', 'unknown') THEN ? ELSE finished_at END
		WHERE occurrence_id = ?`,
		status, errCode, at.UnixMilli(), status, at.UnixMilli(),
		status, at.UnixMilli(), occurrenceID)
	if err != nil {
		return fmt.Errorf("cron occurrence: update status: %w", err)
	}
	return nil
}

func (s *SQLiteOccurrenceStore) ListByJob(
	ctx context.Context, jobID string, limit int,
) ([]*Occurrence, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+occurrenceColumns+` FROM cron_occurrences
		 WHERE job_id = ? ORDER BY created_at DESC, occurrence_id DESC LIMIT ?`,
		jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("cron occurrence: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Occurrence
	for rows.Next() {
		occ, err := scanOccurrence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, occ)
	}
	return out, rows.Err()
}
