package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/config"
)

type initialLifecycleState struct {
	policy   string
	revision string

	archiveAt             *time.Time
	conversationExpiresAt *time.Time
	historyExpiresAt      *time.Time
}

// shouldNotifyTerminationCleanup reports whether a terminated session should
// trigger logical-session cleanup (queued inputs and attached Cron jobs).
// Lifecycle v2 treats runtime-release reasons as separate from session cleanup.
func shouldNotifyTerminationCleanup(policy, reason string) bool {
	if policy != config.LifecyclePolicyV2 {
		return true
	}

	switch reason {
	case "idle_timeout", "max_lifetime", "zombie", "gateway_restart", "max_turns", "gc", "cron_complete":
		return false
	default:
		return true
	}
}

func updateArchiveProjection(info *SessionInfo, now time.Time) {
	if info == nil {
		return
	}
	info.Archived = info.LifecyclePolicy == config.LifecyclePolicyV2 &&
		info.ArchiveAt != nil &&
		!info.ArchiveAt.After(now)
}

func newInitialLifecycleState(now time.Time, cfg config.LifecycleConfig) (initialLifecycleState, error) {
	switch cfg.Policy {
	case config.LifecyclePolicyLegacy:
		return initialLifecycleState{policy: config.LifecyclePolicyLegacy}, nil
	case config.LifecyclePolicyV2:
	default:
		return initialLifecycleState{}, fmt.Errorf("session lifecycle: unsupported policy %q", cfg.Policy)
	}

	if cfg.Conversation.ArchiveAfter <= 0 {
		return initialLifecycleState{}, fmt.Errorf("session lifecycle: archive_after must be positive")
	}
	if cfg.Conversation.RetentionAfterLastInput <= 0 {
		return initialLifecycleState{}, fmt.Errorf("session lifecycle: retention_after_last_input must be positive")
	}
	if cfg.Conversation.ArchiveAfter > cfg.Conversation.RetentionAfterLastInput {
		return initialLifecycleState{}, fmt.Errorf("session lifecycle: archive_after must not exceed retention_after_last_input")
	}

	archiveAt := now.Add(cfg.Conversation.ArchiveAfter)
	conversationExpiresAt := now.Add(cfg.Conversation.RetentionAfterLastInput)
	return initialLifecycleState{
		policy:                config.LifecyclePolicyV2,
		revision:              lifecyclePolicyRevision(cfg),
		archiveAt:             &archiveAt,
		conversationExpiresAt: &conversationExpiresAt,
		historyExpiresAt:      &conversationExpiresAt,
	}, nil
}

func lifecyclePolicyRevision(cfg config.LifecycleConfig) string {
	encoded, _ := json.Marshal(cfg)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func latestLifecycleTime(current *time.Time, candidate time.Time) time.Time {
	if current != nil && current.After(candidate) {
		return *current
	}
	return candidate
}

func (s initialLifecycleState) apply(info *SessionInfo) {
	if info == nil {
		return
	}
	info.LifecyclePolicy = s.policy
	info.LifecyclePolicyRevision = s.revision
	info.ArchiveAt = s.archiveAt
	info.ConversationExpiresAt = s.conversationExpiresAt
	info.HistoryExpiresAt = s.historyExpiresAt
}
