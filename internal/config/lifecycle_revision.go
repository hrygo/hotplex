package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// LifecyclePolicyRevision identifies the effective policy consistently across
// new sessions, accepted inputs and explicitly confirmed migrations.
func LifecyclePolicyRevision(cfg LifecycleConfig) string {
	encoded, _ := json.Marshal(cfg)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
