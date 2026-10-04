package cron

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/hrygo/hotplex/internal/security"
)

// RequiredPlatformKey maps each platform to the PlatformKey field required
// for CLI-based result delivery. Used by ValidateJob, HasCLIDelivery, and
// buildDeliverySuffix to avoid duplicating platform→key mappings.
var RequiredPlatformKey = map[string]string{
	"feishu": "chat_id",
	"slack":  "channel_id",
}

var threatPatterns = []string{
	"ignore previous instructions",
	"ignore all above",
	"ignore all instructions",
	"forget your instructions",
	"disregard your training",
	"system prompt override",
	"new instructions:",
	"override previous",
	"jailbreak",
	"you are now",
}

// filterControl removes control characters and Unicode formatting codepoints from s.
// When keepNewlineTab is true, \n and \t are preserved (used for prompts).
// When false, all control chars including \n and \t are removed (used for job names).
func filterControl(s string, keepNewlineTab bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			if !keepNewlineTab || (r != '\n' && r != '\t') {
				continue
			}
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ValidateJobPrompt scans for obvious prompt injection patterns.
func ValidateJobPrompt(prompt string) error {
	if prompt == "" {
		return errors.New("cron: prompt must not be empty")
	}
	if len(prompt) > 4096 {
		return fmt.Errorf("cron: prompt exceeds 4KB limit (%d bytes)", len(prompt))
	}
	// NFKD decomposes compatibility characters (e.g., fullwidth Latin → ASCII)
	// so that confusable Unicode cannot bypass substring-based threat detection.
	cleaned := strings.ToLower(norm.NFKD.String(filterControl(prompt, true)))
	for _, pat := range threatPatterns {
		if strings.Contains(cleaned, pat) {
			return fmt.Errorf("cron: potential prompt injection detected")
		}
	}
	return nil
}

// SanitizeJobName strips control characters and newlines from a job name
// to prevent injection when the name is embedded in worker prompts.
func SanitizeJobName(name string) string {
	return filterControl(name, false)
}

// SanitizePrompt strips invisible characters and normalizes Unicode from a prompt
// and returns the cleaned version suitable for storage. Called after ValidateJobPrompt
// passes to ensure the persisted text matches what was validated.
func SanitizePrompt(prompt string) string {
	return norm.NFKD.String(filterControl(prompt, true))
}

// formatJobPrompt builds the standard cron prompt with job metadata and timestamp.
func formatJobPrompt(job *CronJob, scheduledAt time.Time) string {
	return fmt.Sprintf("[cron:%s %s] %s\n%s",
		job.ID, SanitizeJobName(job.Name),
		job.Payload.Message, scheduledAt.Format(time.RFC3339))
}

// buildWebhookPrefix returns a prefix injected when a cron job is triggered
// via webhook (PlatformKey contains trigger=webhook and pr_number).
// This ensures the LLM targets the specific PR without enumerating all open PRs.
func buildWebhookPrefix(job *CronJob) string {
	if job.PlatformKey == nil {
		return ""
	}
	prNum := job.PlatformKey["pr_number"]
	if job.PlatformKey["trigger"] != "webhook" || prNum == "" {
		return ""
	}
	// Defense-in-depth: reject non-numeric pr_number to prevent injection
	// via PlatformKey (e.g. SQLite deserialization or UpdateJob API path).
	for _, r := range prNum {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return fmt.Sprintf("⚠️ WEBHOOK 触发：仅审查 PR #%s。不要枚举其他 open PR。"+
		" 环境变量 TARGET_PR=%s 已设置，直接使用 $TARGET_PR 作为目标 PR。\n\n", prNum, prNum)
}

// ValidateJob performs full validation on a CronJob before creation/update.
func ValidateJob(job *CronJob) error {
	if job.Name == "" {
		return errors.New("cron: name is required")
	}
	if len(job.Name) > 128 {
		return fmt.Errorf("cron: name exceeds 128 character limit (%d chars)", len(job.Name))
	}
	// Reject names with control characters or newlines that could enable
	// prompt injection when the name is embedded in worker prompts.
	for _, r := range job.Name {
		if unicode.IsControl(r) {
			return errors.New("cron: name contains control characters")
		}
	}
	if job.OwnerID == "" {
		return errors.New("cron: owner_id is required")
	}
	// #870: an explicit unsafe workdir fails closed at plan time, not at
	// run time. Empty keeps the executor's resolveWorkDir fallback; a
	// relative or forbidden path must never become a worker's cwd.
	if job.WorkDir != "" {
		if err := security.ValidateWorkDir(job.WorkDir); err != nil {
			return fmt.Errorf("cron: invalid work_dir: %w", err)
		}
	}
	if job.BotID == "" {
		return errors.New("cron: bot_id is required")
	}
	if err := ValidateSchedule(job.Schedule); err != nil {
		return err
	}
	if err := ValidateJobPrompt(job.Payload.Message); err != nil {
		return err
	}
	// Attached session validation.
	if job.Payload.Kind == PayloadAttachedSession {
		if job.Payload.TargetSessionID == "" {
			return errors.New("cron: target_session_id is required for attached_session")
		}
		if job.Schedule.Kind == ScheduleCron {
			return errors.New("cron: attached_session does not support cron expression schedules")
		}
	}
	// Platform delivery validation: each platform requires a specific key in platform_key.
	if key, ok := RequiredPlatformKey[job.Platform]; ok {
		if job.PlatformKey[key] == "" {
			return fmt.Errorf("cron: %s platform requires %s in platform_key", job.Platform, key)
		}
	}
	// Recurring jobs must have lifecycle constraints to prevent infinite execution.
	if job.Schedule.Kind != ScheduleAt {
		if job.MaxRuns <= 0 {
			return errors.New("cron: max_runs is required for recurring jobs (every/cron)")
		}
		if job.ExpiresAt == "" {
			return errors.New("cron: expires_at is required for recurring jobs (every/cron)")
		}
		if _, err := time.Parse(time.RFC3339, job.ExpiresAt); err != nil {
			return fmt.Errorf("cron: invalid expires_at: %w", err)
		}
	}
	return nil
}
