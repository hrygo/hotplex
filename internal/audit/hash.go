package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// hashableFields returns the UserActivity fields included in self_hash computation.
// The fields are: all except ID, PrevHash, SelfHash.
type hashableFields struct {
	Ts           int64  `json:"ts"`
	UserID       string `json:"user_id"`
	UserIDType   string `json:"user_id_type"`
	Platform     string `json:"platform"`
	SessionID    string `json:"session_id"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Outcome      string `json:"outcome"`
	DetailJSON   string `json:"detail_json"`
	EventRef     string `json:"event_ref"`
	IP           string `json:"ip"`
	UserAgent    string `json:"user_agent"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

// ComputeSelfHash returns sha256(prevHash || canonical(ua fields except prev/self_hash)).
// For the genesis row, prevHash is "".
func ComputeSelfHash(prevHash string, ua *UserActivity) (string, error) {
	if ua == nil {
		return "", fmt.Errorf("audit: nil UserActivity")
	}
	expiresAt := int64(0)
	if ua.ChainEpoch == lifecycleChainProfile.epoch {
		expiresAt = ua.ExpiresAt
	}
	payload, err := json.Marshal(hashableFields{
		Ts:           ua.Ts,
		UserID:       ua.UserID,
		UserIDType:   ua.UserIDType,
		Platform:     ua.Platform,
		SessionID:    ua.SessionID,
		Action:       ua.Action,
		ResourceType: ua.ResourceType,
		ResourceID:   ua.ResourceID,
		Outcome:      ua.Outcome,
		DetailJSON:   ua.DetailJSON,
		EventRef:     ua.EventRef,
		IP:           ua.IP,
		UserAgent:    ua.UserAgent,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		return "", fmt.Errorf("audit: hash marshal: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChainBreak is one hash-chain violation found during verification.
// Expected is the prev_hash the row should carry (previous row's
// self_hash or the checkpoint anchor); Actual is what the row stores.
type ChainBreak struct {
	ID       int64
	Reason   string
	Expected string
	Actual   string
}

// VerifyChain validates a sequence of UserActivity rows in id order and
// returns EVERY break found, not just the first. The cursor always
// advances to the row's stored self_hash so later breaks are still
// detected after an earlier one (the historical single-break
// short-circuit hid later gaps — e.g. id=1269 was masked by 1253).
// The genesis row's prev_hash must equal checkpointHash ("" for a fresh
// chain, or the rebase anchor after GC).
func VerifyChain(rows []UserActivity, checkpointHash string) []ChainBreak {
	expectedPrev := checkpointHash
	var breaks []ChainBreak
	for i := range rows {
		row := rows[i]
		if row.PrevHash != expectedPrev {
			breaks = append(breaks, ChainBreak{
				ID: row.ID, Reason: "prev_hash_mismatch",
				Expected: expectedPrev, Actual: row.PrevHash,
			})
		}
		// self_hash is computed from the row's OWN prev_hash (that is how
		// it was written), so a prev mismatch never double-reports the row.
		computed, err := ComputeSelfHash(row.PrevHash, &row)
		if err != nil {
			breaks = append(breaks, ChainBreak{
				ID: row.ID, Reason: "compute_error:" + err.Error(),
				Expected: row.PrevHash, Actual: row.SelfHash,
			})
		} else if computed != row.SelfHash {
			breaks = append(breaks, ChainBreak{
				ID: row.ID, Reason: "self_hash_mismatch",
				Expected: computed, Actual: row.SelfHash,
			})
		}
		expectedPrev = row.SelfHash
	}
	return breaks
}
