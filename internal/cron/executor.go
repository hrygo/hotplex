package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/events"
)

// BridgeStarter is the narrow interface the executor needs from the gateway Bridge.
type BridgeStarter interface {
	StartSession(ctx context.Context, p worker.SessionStartParams) error
}

// SystemInputRequest is an internal, trusted input that did not arrive over a
// client connection (a cron firing, a system event).
type SystemInputRequest struct {
	SessionID string
	// OccurrenceID makes the input identity stable per firing, so a retry
	// resolves to the same durable execution instead of starting a second run.
	OccurrenceID string
	Content      string
}

// SystemInputResult reports the durable execution that owns the input.
type SystemInputResult struct {
	ExecutionID string
	// Duplicate is true when the input was already accepted, so no Worker
	// dispatch happened for this call.
	Duplicate bool
}

// SystemInputDispatcher accepts an internal system-originated input through the
// same durable accept, owner-lease and terminal-correlation path as a client
// input. Cron must go through this rather than calling Worker.Input directly,
// which would bypass the execution ledger entirely.
type SystemInputDispatcher interface {
	DispatchSystemInput(ctx context.Context, req SystemInputRequest) (SystemInputResult, error)
	// WaitForExecution blocks until the given execution reaches a terminal
	// runtime state. It correlates on the execution rather than on the
	// session's global IDLE state, because a session can look idle while the
	// run this caller owns is still executing.
	WaitForExecution(ctx context.Context, sessionID, executionID string, timeout time.Duration) error
}

// ErrSystemInputDispatcherUnavailable is returned when no dispatcher is wired.
// Cron must never fall back to calling Worker.Input directly: that would
// bypass the execution ledger and leave the run unattributable.
var ErrSystemInputDispatcherUnavailable = errors.New("cron executor: system input dispatcher unavailable")

// SessionStateChecker polls session state for completion detection.
type SessionStateChecker interface {
	Get(ctx context.Context, id string) (*session.SessionInfo, error)
	GetWorker(id string) worker.Worker
	Transition(ctx context.Context, id string, to events.SessionState) error
}

// Executor runs a single cron job by starting a worker session and delivering the prompt.
type Executor struct {
	log         *slog.Logger
	bridge      BridgeStarter
	sm          SessionStateChecker
	sandbox     string
	occurrences OccurrenceStore
	// dispatcher routes the prompt through the gateway's durable input path.
	// It is required: dispatching straight to Worker.Input would bypass the
	// execution ledger and leave the run unattributable.
	dispatcher SystemInputDispatcher
	// now is injectable so occurrence timestamps and schedule keys are
	// deterministic under test.
	now func() time.Time
}

// NewExecutor creates a new cron executor.
func NewExecutor(
	log *slog.Logger,
	bridge BridgeStarter,
	sm SessionStateChecker,
	sandbox string,
	occurrences OccurrenceStore,
	dispatcher SystemInputDispatcher,
) *Executor {
	return &Executor{
		log:         log.With("component", "cron_executor"),
		bridge:      bridge,
		sm:          sm,
		sandbox:     sandbox,
		occurrences: occurrences,
		dispatcher:  dispatcher,
		now:         time.Now,
	}
}

// ExecuteResult describes how one firing was resolved against the durable
// occurrence ledger.
type ExecuteResult struct {
	// SessionID is the session the run used, empty on a rejected duplicate.
	SessionID string
	// OccurrenceID is the durable occurrence for this firing.
	OccurrenceID string
	// Duplicate is true when the trigger had already been claimed, so no new
	// Agent run was started.
	Duplicate bool
}

// Execute runs a cron job: claims its durable occurrence, starts a session,
// sends the prompt, and waits for completion.
//
// The occurrence is claimed before any Worker starts, so a crash between
// accept and dispatch cannot lose the fact that the trigger was taken, and a
// repeated trigger resolves to the existing run instead of starting a second
// Agent. Returns the result for delivery routing.
// timeout is the execution deadline (from job.TimeoutSec or scheduler default).
func (e *Executor) Execute(
	ctx context.Context,
	job *CronJob,
	trigger TriggerIdentity,
	timeout time.Duration,
) (ExecuteResult, error) {
	var out ExecuteResult

	now := e.now()
	occ, err := NewOccurrence(trigger, now)
	if err != nil {
		return out, fmt.Errorf("cron executor: build occurrence: %w", err)
	}
	out.OccurrenceID = occ.OccurrenceID

	if e.occurrences != nil {
		stored, created, err := e.occurrences.Claim(ctx, occ)
		if err != nil {
			// A claim failure must not start an Agent: without a durable
			// record we could neither deduplicate the trigger nor prove what
			// it did.
			return out, fmt.Errorf("cron executor: claim occurrence: %w", err)
		}
		occ = stored
		out.OccurrenceID = stored.OccurrenceID
		if !created {
			// Already claimed. Returning the recorded identity lets the caller
			// report the original run instead of executing a second one.
			out.Duplicate = true
			out.SessionID = stored.SessionID
			e.log.Info("cron executor: duplicate trigger suppressed",
				"job_id", job.ID, "occurrence_id", stored.OccurrenceID,
				"status", stored.Status, "session_id", stored.SessionID)
			return out, nil
		}
	}

	// The session key derives from the occurrence, not from the wall clock, so
	// a retry of the same firing lands on the same session identity.
	sessionKey := session.DeriveCronSessionKey(job.ID, occurrenceEpoch(occ))
	out.SessionID = sessionKey

	// Merge platform context so the bridge can inject environment variables (like channel_id).
	// Inject default sandbox first; per-job PlatformKey overrides below.
	platformKey := make(map[string]string)
	if e.sandbox != "" {
		platformKey[worker.SandboxPlatformKey] = e.sandbox
	}
	if job.PlatformKey != nil {
		maps.Copy(platformKey, job.PlatformKey)
	}
	platformKey["cron_job_id"] = job.ID
	title := fmt.Sprintf("cron:%s", job.Name)

	wt := worker.WorkerType(job.Payload.WorkerType)
	if wt == "" {
		wt = worker.TypeClaudeCode // Default
	}

	// Normalize a job without a messaging delivery target to the "cron"
	// platform. CronJob.SessionKey already derives under "cron"; aligning the
	// session platform keeps the session.create audit row (and per-platform
	// metrics) populated instead of empty for pure scheduled tasks. Jobs that
	// deliver to slack/feishu keep their real platform so agent-config
	// fallback still resolves correctly.
	platform := job.Platform
	if platform == "" {
		platform = "cron"
	}

	if err := e.bridge.StartSession(ctx, worker.SessionStartParams{
		ID:           sessionKey,
		UserID:       job.OwnerID,
		BotID:        job.BotID,
		BotName:      job.BotName,
		WorkerType:   wt,
		AllowedTools: job.Payload.AllowedTools,
		WorkDir:      e.resolveWorkDir(job),
		Platform:     platform,
		PlatformKey:  platformKey,
		Title:        title,
	}); err != nil {
		e.markOccurrence(ctx, occ, OccurrenceFailed, "SESSION_START_FAILED")
		return out, fmt.Errorf("start cron session: %w", err)
	}

	w := e.sm.GetWorker(sessionKey)
	if w == nil {
		e.markOccurrence(ctx, occ, OccurrenceFailed, "WORKER_NOT_FOUND")
		return out, fmt.Errorf("cron executor: worker not found after start")
	}

	// Bind the session and mark the run started before the first prompt byte
	// reaches the Worker, so an interrupted run is still attributable.
	if e.occurrences != nil {
		if err := e.occurrences.BindSession(ctx, occ.OccurrenceID, sessionKey); err != nil {
			e.log.Error("cron executor: bind occurrence session failed",
				"occurrence_id", occ.OccurrenceID, "session_id", sessionKey, "err", err)
		}
		if err := e.occurrences.UpdateStatus(ctx, occ.OccurrenceID, OccurrenceStarted, "", e.now()); err != nil {
			e.log.Error("cron executor: persist occurrence start failed",
				"occurrence_id", occ.OccurrenceID, "err", err)
		}
	}

	prompt := buildWebhookPrefix(job) + formatJobPrompt(job, e.now())
	prompt += buildDeliverySuffix(job)

	if e.dispatcher == nil {
		e.markOccurrence(ctx, occ, OccurrenceFailed, "DISPATCHER_UNAVAILABLE")
		return out, fmt.Errorf("cron executor: system input dispatcher unavailable: %w",
			ErrSystemInputDispatcherUnavailable)
	}
	dispatch, err := e.dispatcher.DispatchSystemInput(ctx, SystemInputRequest{
		SessionID:    sessionKey,
		OccurrenceID: occ.OccurrenceID,
		Content:      prompt,
	})
	if err != nil {
		e.markOccurrence(ctx, occ, OccurrenceFailed, "WORKER_INPUT_FAILED")
		return out, fmt.Errorf("cron executor: dispatch system input: %w", err)
	}
	if dispatch.Duplicate {
		// The occurrence is new but the execution ledger already owns this
		// identity, so no second dispatch happens and the run is already
		// accounted for.
		e.markOccurrence(ctx, occ, OccurrenceCompleted, "")
		e.terminateSession(sessionKey)
		return out, nil
	}

	// Signal EOF so --print mode workers exit after processing instead of
	// waiting for more streaming input that will never arrive.
	if ci, ok := w.(interface{ CloseInput() error }); ok {
		_ = ci.CloseInput()
	}

	// Wait on the execution this dispatch created, not on the session's global
	// IDLE state: a session can read idle while the run we own is still going.
	err = e.dispatcher.WaitForExecution(ctx, sessionKey, dispatch.ExecutionID, timeout)

	if err != nil {
		e.log.Error("cron executor: session execution failed",
			"session_id", sessionKey, "timeout", timeout, "err", err)
		// A timeout is not proof that the Agent had no effect, so the
		// occurrence stops at unknown rather than a retryable failure.
		e.markOccurrence(ctx, occ, OccurrenceUnknown, "EXECUTION_TIMEOUT")
	} else {
		e.markOccurrence(ctx, occ, OccurrenceCompleted, "")
	}

	e.terminateSession(sessionKey)

	return out, err
}

// terminateSession ensures the worker process exits immediately. It uses a
// background context with a short timeout so termination still happens even
// when the original context is canceled.
func (e *Executor) terminateSession(sessionKey string) {
	termCtx, cancel := context.WithTimeout(context.Background(), base.GracefulShutdownTimeout)
	defer cancel()
	if termErr := e.sm.Transition(termCtx, sessionKey, events.StateTerminated); termErr != nil {
		e.log.Warn("cron executor: failed to terminate session", "session_id", sessionKey, "err", termErr)
	}
}

// occurrenceEpoch derives a stable per-occurrence epoch for the session key.
func occurrenceEpoch(occ *Occurrence) int64 {
	var sum uint64
	for _, c := range []byte(occ.TriggerKey) {
		sum = sum*131 + uint64(c)
	}
	return int64(sum) + occ.Generation
}

// markOccurrence records an occurrence lifecycle transition, logging rather
// than propagating failures: the run outcome is already decided, and losing
// the annotation must not turn a completed run into an error.
func (e *Executor) markOccurrence(
	ctx context.Context, occ *Occurrence, status OccurrenceStatus, errCode string,
) {
	if e.occurrences == nil || occ == nil {
		return
	}
	if err := e.occurrences.UpdateStatus(ctx, occ.OccurrenceID, status, errCode, e.now()); err != nil {
		e.log.Error("cron executor: persist occurrence status failed",
			"occurrence_id", occ.OccurrenceID, "status", status, "err", err)
	}
}

// resolveWorkDir computes the working directory for a cron job execution.
// Webhook-triggered PR reviews get a per-PR directory under the OS temp dir;
// all other executions (cron fallback, etc.) use the job's static WorkDir.
func (e *Executor) resolveWorkDir(job *CronJob) string {
	prNum := job.PlatformKey["pr_number"]
	if prNum != "" && job.PlatformKey["trigger"] == "webhook" {
		return filepath.Join(os.TempDir(),
			fmt.Sprintf("pr-review-%s/pr-%s", time.Now().Format("20060102"), prNum))
	}
	return job.WorkDir
}

// HasCLIDelivery returns true if the job has sufficient platform info
// for CLI-based result delivery.
func HasCLIDelivery(job *CronJob) bool {
	key, ok := RequiredPlatformKey[job.Platform]
	if !ok {
		return false
	}
	return job.PlatformKey[key] != ""
}

// buildDeliverySuffix appends CLI delivery instructions to the cron prompt.
func buildDeliverySuffix(job *CronJob) string {
	if job.Silent {
		return ""
	}
	if job.Platform == "" || job.Platform == "cron" {
		return ""
	}
	switch job.Platform {
	case "slack":
		return buildSlackDelivery(job)
	case "feishu":
		return buildFeishuDelivery(job)
	default:
		return ""
	}
}

func buildSlackDelivery(job *CronJob) string {
	ch := job.PlatformKey[RequiredPlatformKey["slack"]]
	if ch == "" {
		return ""
	}
	cmd := fmt.Sprintf("hotplex slack send-message --channel %s --text \"结果内容\"", ch)
	if ts := job.PlatformKey["thread_ts"]; ts != "" {
		cmd += fmt.Sprintf(" --thread-ts %s", ts)
	}
	return fmt.Sprintf(deliveryBlockFmt, SanitizeJobName(job.Name), cmd)
}

func buildFeishuDelivery(job *CronJob) string {
	chatID := job.PlatformKey[RequiredPlatformKey["feishu"]]
	if chatID == "" {
		return ""
	}
	var cmd string
	if msgID := job.PlatformKey["message_id"]; msgID != "" {
		cmd = fmt.Sprintf("lark-cli im +messages-reply --as bot --message-id %s --markdown \"结果内容\"", msgID)
	} else {
		cmd = fmt.Sprintf("lark-cli im +messages-send --as bot --chat-id %s --markdown \"结果内容\"", chatID)
	}
	return fmt.Sprintf(deliveryBlockFmt, SanitizeJobName(job.Name), cmd)
}

const deliveryBlockFmt = `

## 结果投递（必须执行）

任务「%s」执行完成后，你必须将结果通过以下命令投递给用户。将 "结果内容" 替换为执行结果的简洁摘要（支持 Markdown 格式）。

` + "```bash\n%s\n```" + `

投递完成后，直接结束对话退出。如果投递命令执行失败，在日志中记录错误后仍然退出。`
