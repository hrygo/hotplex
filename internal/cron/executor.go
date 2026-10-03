package cron

import (
	"context"
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
) *Executor {
	return &Executor{
		log:         log.With("component", "cron_executor"),
		bridge:      bridge,
		sm:          sm,
		sandbox:     sandbox,
		occurrences: occurrences,
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

	if err := w.Input(ctx, prompt, nil); err != nil {
		e.markOccurrence(ctx, occ, OccurrenceFailed, "WORKER_INPUT_FAILED")
		return out, fmt.Errorf("cron executor: input prompt: %w", err)
	}

	// Signal EOF so --print mode workers exit after processing instead of
	// waiting for more streaming input that will never arrive.
	if ci, ok := w.(interface{ CloseInput() error }); ok {
		_ = ci.CloseInput()
	}

	err = e.waitForCompletion(ctx, sessionKey, timeout)

	if err != nil {
		e.log.Error("cron executor: session execution failed",
			"session_id", sessionKey, "timeout", timeout, "err", err)
		// A timeout is not proof that the Agent had no effect, so the
		// occurrence stops at unknown rather than a retryable failure.
		e.markOccurrence(ctx, occ, OccurrenceUnknown, "EXECUTION_TIMEOUT")
	} else {
		e.markOccurrence(ctx, occ, OccurrenceCompleted, "")
	}

	// Explicitly terminate the session to ensure the worker process exits immediately.
	// We use context.Background() with a short timeout to ensure termination happens
	// even if the original context is canceled.
	termCtx, cancel := context.WithTimeout(context.Background(), base.GracefulShutdownTimeout)
	defer cancel()
	if termErr := e.sm.Transition(termCtx, sessionKey, events.StateTerminated); termErr != nil {
		e.log.Warn("cron executor: failed to terminate session", "session_id", sessionKey, "err", termErr)
	}

	return out, err
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

func (e *Executor) waitForCompletion(ctx context.Context, sessionID string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Initial check to avoid waiting for the first ticker tick if the task is near-instant.
	si, err := e.sm.Get(timeoutCtx, sessionID)
	if err == nil && si.State != events.StateRunning && si.State != events.StateCreated {
		return nil
	}

	// 500ms provides a good balance between responsiveness and system overhead.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("cron executor: timeout waiting for session %s: %w", sessionID, timeoutCtx.Err())
		case <-ticker.C:
			si, err := e.sm.Get(timeoutCtx, sessionID)
			if err != nil {
				e.log.Warn("cron executor: failed to check session state", "session_id", sessionID, "err", err)
				continue
			}
			// IDLE means the worker finished this turn and is waiting.
			// TERMINATED means the worker exited.
			if si.State != events.StateRunning && si.State != events.StateCreated {
				return nil
			}
		}
	}
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
