package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/pkg/events"
)

// sensitiveToolNames are tools whose full input is recorded in the audit trail
// because their inputs are forensically valuable (spec §5.3: "sensitive
// behaviors store full context directly in detail_json"). Bash/Write/Edit
// command surfaces, MultiEdit bulk edits, and network-fetch tools fall here.
// Add with care — every entry means audit rows grow with full input.
var sensitiveToolNames = map[string]bool{
	"bash":        true,
	"write":       true, // codex/opencode naming
	"edit":        true,
	"multiedit":   true,
	"str_replace": true, // opencode
	"webfetch":    true,
	"websearch":   true,
	"web_fetch":   true,
	"web_search":  true,
}

// sensitiveInputPatterns match secret-like substrings inside tool inputs so
// they are masked before the input is stored in the audit trail (spec §5.9:
// "forbidden to log credentials, API key plaintext"). Matches are replaced
// with a prefix+mask token. Patterns are intentionally conservative — false
// negatives (missed secrets) are acceptable, false positives (masked non-
// secrets) are not harmful.
//
// Each pattern MUST define two capture groups:
//
//	group 1 = the literal prefix to preserve verbatim (e.g. "hpk_", "Bearer ",
//	          "password=")
//	group 2 = the secret payload to mask
//
// maskSensitiveInput relies on this two-group contract.
// maskSensitiveInput redacts secret-like substrings from a rendered tool-input
// string. Returns the sanitized string. Non-matching input is returned as-is.
// Each match is replaced with a prefix(4)+… token so the audit row shows enough
// to identify which key was used without exposing it (spec §5.9 prefix+mask).
func maskSensitiveInput(s string) string {
	return audit.MaskSensitiveText(s)
}

// sha256Hex returns the hex-encoded sha256 of s. Used for non-sensitive tool
// input fingerprinting (spec §5.3: non-sensitive stores summary + sha256).
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// emitToolCallAudit enqueues a bodyless tool.call audit event. Non-blocking and
// safe to call from the forward path; no-op when auditCollector is nil.
// Outcome is success (see note on ToolCall branch in bridge_forward.go re:
// failure scope).
// Attribution: UserID comes from fc.sessOwner (resolved earlier via
// sm.Get → OwnerID||UserID, same identity space as message.inbound's
// env.OwnerID). UserIDType is "platform" per spec §5.4 tool.call backtracking
// (session_id → sessions.user_id).
func (b *Bridge) emitToolCallAudit(fc *forwardContext, tc *events.ToolCallData) {
	c := b.auditCollector
	if c == nil || tc == nil {
		return
	}
	userID := fc.sessOwner
	if userID == "" {
		userID = audit.AnonymousUserID
	}
	detail := buildToolCallDetail(tc)
	ua := &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   audit.UserIDTypePlatform,
		Platform:     fc.sessPlatform,
		SessionID:    fc.sessionID,
		Action:       audit.ActionToolCall,
		ResourceType: "tool",
		ResourceID:   tc.ID,
		Outcome:      audit.OutcomeSuccess,
		DetailJSON:   detail,
	}
	_ = c.Enqueue(context.Background(), ua)
}

// buildToolCallDetail constructs the whitelisted, bodyless detail_json for a
// tool.call audit row. Tool names are bounded identifiers because the upstream
// worker controls this value; free-form names could otherwise carry user text.
func buildToolCallDetail(tc *events.ToolCallData) string {
	name := "unknown"
	if tc != nil {
		name = safeAuditToolName(tc.Name)
	}
	d := map[string]any{
		"name":    name,
		"success": true, // tool was invoked; failure correlation is P3
	}
	b, _ := json.Marshal(d)
	return string(b)
}

func safeAuditToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "unknown"
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return "unknown"
	}
	return name
}

// emitPermissionRequestAudit enqueues a permission.request audit event when
// an outgoing PermissionRequest is forwarded to a client.
func (b *Bridge) emitPermissionRequestAudit(fc *forwardContext, pr *events.PermissionRequestData) {
	c := b.auditCollector
	if c == nil || pr == nil {
		return
	}
	userID := fc.sessOwner
	if userID == "" {
		userID = audit.AnonymousUserID
	}

	detailMap := map[string]any{"id": pr.ID}

	detailBytes, err := json.Marshal(detailMap)
	var detailStr string
	if err == nil {
		detailStr = string(detailBytes)
	}

	ua := &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   audit.UserIDTypePlatform,
		Platform:     fc.sessPlatform,
		SessionID:    fc.sessionID,
		Action:       audit.ActionPermissionRequest,
		ResourceType: "permission",
		ResourceID:   pr.ID,
		Outcome:      audit.OutcomeSuccess,
		DetailJSON:   detailStr,
	}
	_ = c.Enqueue(context.Background(), ua)
}
