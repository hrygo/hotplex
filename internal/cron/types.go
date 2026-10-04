package cron

import (
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
)

// Schedule kinds.
type ScheduleKind string

const (
	ScheduleAt    ScheduleKind = "at"
	ScheduleEvery ScheduleKind = "every"
	ScheduleCron  ScheduleKind = "cron"
)

// CronSchedule defines when a job fires.
type CronSchedule struct {
	Kind    ScheduleKind `json:"kind"`
	At      string       `json:"at,omitempty"`       // kind=at: ISO-8601 timestamp
	EveryMs int64        `json:"every_ms,omitempty"` // kind=every: interval in ms
	Expr    string       `json:"expr,omitempty"`     // kind=cron: cron expression
	TZ      string       `json:"tz,omitempty"`       // timezone, default Local
}

// Payload kinds.
type PayloadKind string

const (
	PayloadIsolatedSession PayloadKind = "isolated_session" // renamed from agent_turn
	PayloadSystemEvent     PayloadKind = "system_event"     // reserved
	PayloadAttachedSession PayloadKind = "attached_session" // inject existing session
)

// CronPayload defines what a job executes.
type CronPayload struct {
	Kind            PayloadKind `json:"kind"`
	Message         string      `json:"message"`
	TargetSessionID string      `json:"target_session_id,omitempty"` // attached_session only
	AllowedTools    []string    `json:"allowed_tools,omitempty"`
	WorkerType      string      `json:"worker_type,omitempty"` // e.g. "claude_code"
}

// JobStatus records the outcome of the last run.
type JobStatus string

const (
	StatusSuccess JobStatus = "success"
	StatusFailed  JobStatus = "failed"
	StatusTimeout JobStatus = "timeout"
)

// DeliveryMode decides who owns the delivery of a cron run's final answer.
//
// The two modes are mutually exclusive by design. A job must never have both
// the gateway sending its result and a CLI instruction telling the Agent to
// send it again — that is the double-delivery this mode exists to prevent.
type DeliveryMode string

const (
	// DeliveryModeLegacyCLI keeps today's behaviour: the result is appended to
	// the prompt as a CLI instruction and the Agent performs the send. It is
	// the default for any job that does not name a mode, so existing jobs
	// keep their exact current behaviour.
	DeliveryModeLegacyCLI DeliveryMode = "legacy_cli"
	// DeliveryModeGateway means the gateway owns delivery: the Agent returns
	// the answer and the gateway sends it through a recorded effect.
	DeliveryModeGateway DeliveryMode = "gateway"
)

// ResolveDeliveryMode maps a stored mode to the mode actually used.
//
// An absent or unrecognised value resolves to legacy_cli: an existing job
// must never silently acquire a delivery owner it was never validated for.
func ResolveDeliveryMode(mode DeliveryMode) DeliveryMode {
	if mode == DeliveryModeGateway {
		return DeliveryModeGateway
	}
	return DeliveryModeLegacyCLI
}

// CronJobState holds mutable runtime state.
type CronJobState struct {
	NextRunAtMs     int64     `json:"next_run_at_ms"`
	LastRunAtMs     int64     `json:"last_run_at_ms"`
	RunningAtMs     int64     `json:"running_at_ms"`
	LastStatus      JobStatus `json:"last_status,omitempty"`
	ConsecutiveErrs int       `json:"consecutive_errors"` // execution failures
	SchedErrs       int       `json:"sched_errors"`       // schedule computation failures
	RetryCount      int       `json:"retry_count,omitempty"`
	LastRunID       string    `json:"last_run_id,omitempty"`
	RunCount        int       `json:"run_count,omitempty"`
}

// SessionKey derives the deterministic session key for this cron job's execution history.
//
// Migration note (v1.22): Prior to this version, SessionKey always used
// TypeClaudeCode as the worker type regardless of Payload.WorkerType. After
// upgrade, cron jobs with non-claudecode worker_type values will derive
// different session keys, orphaning historical session data under the old
// keys. Affected sessions remain in the store but are no longer reachable
// via SessionKey(). This is a correctness fix — the old behavior silently
// misattributed codexcli/acp sessions to claudecode's session space.
func (j *CronJob) SessionKey() string {
	wt := worker.WorkerType(j.Payload.WorkerType)
	if wt == "" {
		wt = worker.TypeClaudeCode
	}
	return session.DerivePlatformSessionKey(
		j.OwnerID, wt,
		session.PlatformContext{
			Platform: "cron",
			BotID:    j.BotID,
			UserID:   j.OwnerID,
			WorkDir:  j.WorkDir,
			ChatID:   j.ID,
		},
	)
}

// Clone returns a deep copy of the job, including reference-type fields
// (PlatformKey map, AllowedTools slice), so the clone is safe for concurrent mutation.
func (j *CronJob) Clone() *CronJob {
	cp := *j
	if j.PlatformKey != nil {
		cp.PlatformKey = make(map[string]string, len(j.PlatformKey))
		for k, v := range j.PlatformKey {
			cp.PlatformKey[k] = v
		}
	}
	if j.Payload.AllowedTools != nil {
		cp.Payload.AllowedTools = make([]string, len(j.Payload.AllowedTools))
		copy(cp.Payload.AllowedTools, j.Payload.AllowedTools)
	}
	return &cp
}

// CronJob is the top-level job entity.
type CronJob struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Enabled     bool              `json:"enabled"`
	Schedule    CronSchedule      `json:"schedule"`
	Payload     CronPayload       `json:"payload"`
	WorkDir     string            `json:"work_dir,omitempty"`
	BotID       string            `json:"bot_id,omitempty"`
	BotName     string            `json:"bot_name,omitempty"`
	OwnerID     string            `json:"owner_id,omitempty"`
	Platform    string            `json:"platform,omitempty"`
	PlatformKey map[string]string `json:"platform_key,omitempty"`
	// DeliveryMode selects the delivery owner. Empty means legacy_cli.
	DeliveryMode   DeliveryMode `json:"delivery_mode,omitempty"`
	TimeoutSec     int          `json:"timeout_sec,omitempty"`
	DeleteAfterRun bool         `json:"delete_after_run,omitempty"`
	Silent         bool         `json:"silent,omitempty"`
	MaxRetries     int          `json:"max_retries,omitempty"`
	MaxRuns        int          `json:"max_runs,omitempty"`
	ExpiresAt      string       `json:"expires_at,omitempty"`
	State          CronJobState `json:"state"`
	CreatedAtMs    int64        `json:"created_at_ms"`
	UpdatedAtMs    int64        `json:"updated_at_ms"`
}
