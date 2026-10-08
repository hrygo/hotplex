package audit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComputeSelfHash_GenesisHasStableValue(t *testing.T) {
	t.Parallel()
	ua := &UserActivity{
		Ts: 1700000000000, UserID: "u1", UserIDType: UserIDTypePlatform,
		Platform: "feishu", Action: ActionAuthLogin, Outcome: OutcomeSuccess,
		DetailJSON: `{"k":"v"}`,
	}
	// Genesis: prev_hash = ""
	h, err := ComputeSelfHash("", ua)
	require.NoError(t, err)
	require.Len(t, h, 64) // sha256 hex
	require.Equal(t, "dc037a6227117cd7c8e1656d3d7112f46f6d4ea5e86dba9ef82197d01eeaffd3", h)
	h2, _ := ComputeSelfHash("", ua)
	require.Equal(t, h, h2, "hash must be deterministic")
}

func TestComputeSelfHashIncludesOnlyLifecycleV2Expiry(t *testing.T) {
	t.Parallel()

	legacy := &UserActivity{Ts: 1, UserID: "u", Action: ActionAuthLogin, Outcome: OutcomeSuccess}
	legacyHash, err := ComputeSelfHash("", legacy)
	require.NoError(t, err)
	legacy.ExpiresAt = 1234
	legacyHashWithExpiry, err := ComputeSelfHash("", legacy)
	require.NoError(t, err)
	require.Equal(t, legacyHash, legacyHashWithExpiry, "legacy hash payload must remain byte-compatible")

	lifecycle := &UserActivity{
		Ts:         1,
		UserID:     "u",
		Action:     ActionAuthLogin,
		Outcome:    OutcomeSuccess,
		ChainEpoch: lifecycleChainProfile.epoch,
		ExpiresAt:  1234,
	}
	lifecycleHash, err := ComputeSelfHash("", lifecycle)
	require.NoError(t, err)
	lifecycle.ExpiresAt++
	lifecycleHashWithChangedExpiry, err := ComputeSelfHash("", lifecycle)
	require.NoError(t, err)
	require.NotEqual(t, lifecycleHash, lifecycleHashWithChangedExpiry, "lifecycle expiry must be covered by the hash chain")
}

func TestComputeSelfHash_ChangesWithPrev(t *testing.T) {
	t.Parallel()
	ua := &UserActivity{Ts: 1700000000000, UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess}
	h1, _ := ComputeSelfHash("", ua)
	h2, _ := ComputeSelfHash(h1, ua)
	require.NotEqual(t, h1, h2, "different prev_hash must produce different self_hash")
}

func TestComputeSelfHash_DifferentFieldsDifferentHashes(t *testing.T) {
	t.Parallel()
	ua1 := &UserActivity{Ts: 1, UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess}
	ua2 := &UserActivity{Ts: 2, UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess}
	h1, _ := ComputeSelfHash("", ua1)
	h2, _ := ComputeSelfHash("", ua2)
	require.NotEqual(t, h1, h2, "different ts must produce different hashes")
}

func TestComputeSelfHash_NilErrors(t *testing.T) {
	t.Parallel()
	_, err := ComputeSelfHash("", nil)
	require.Error(t, err)
}

func TestVerifyChain_Valid(t *testing.T) {
	t.Parallel()
	var rows []UserActivity
	prev := ""
	for i := 0; i < 5; i++ {
		ua := UserActivity{
			ID: int64(i + 1), Ts: int64(1700000000000 + i*1000),
			UserID: "u1", UserIDType: UserIDTypePlatform, Platform: "feishu",
			Action: ActionAuthLogin, Outcome: OutcomeSuccess, DetailJSON: `{}`,
			PrevHash: prev,
		}
		h, _ := ComputeSelfHash(prev, &ua)
		ua.SelfHash = h
		rows = append(rows, ua)
		prev = h
	}
	breaks := VerifyChain(rows, "")
	require.Empty(t, breaks, "valid chain should not break")
}

func TestVerifyChain_TamperedRowDetected(t *testing.T) {
	t.Parallel()
	var rows []UserActivity
	prev := ""
	for i := 0; i < 3; i++ {
		ua := UserActivity{
			ID: int64(i + 1), Ts: int64(1700000000000 + i*1000),
			UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess, DetailJSON: `{}`,
			PrevHash: prev,
		}
		h, _ := ComputeSelfHash(prev, &ua)
		ua.SelfHash = h
		rows = append(rows, ua)
		prev = h
	}
	// Tamper: change UserID on row 2
	rows[1].UserID = "attacker"
	breaks := VerifyChain(rows, "")
	require.Len(t, breaks, 1, "one tampered row yields one break")
	require.Equal(t, int64(2), breaks[0].ID)
	require.Contains(t, breaks[0].Reason, "self_hash_mismatch")
}

func TestVerifyChain_GenesisPrevMustBeEmpty(t *testing.T) {
	t.Parallel()
	ua := UserActivity{
		ID: 1, Ts: 1700000000000, UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess,
		DetailJSON: `{}`, PrevHash: "should_be_empty",
	}
	// A real row's self_hash is computed from its own prev_hash; only the
	// chain linkage is broken here, so exactly one break is reported.
	h, _ := ComputeSelfHash("should_be_empty", &ua)
	ua.SelfHash = h
	rows := []UserActivity{ua}
	breaks := VerifyChain(rows, "")
	require.Len(t, breaks, 1)
	require.Equal(t, int64(1), breaks[0].ID)
	require.Contains(t, breaks[0].Reason, "prev_hash_mismatch")
}

func TestVerifyChain_AcceptsCheckpointOverride(t *testing.T) {
	t.Parallel()
	ua := UserActivity{
		ID: 100, Ts: 1700000000000, UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess,
		DetailJSON: `{}`, PrevHash: "abc123checkpoint",
	}
	h, _ := ComputeSelfHash("abc123checkpoint", &ua)
	ua.SelfHash = h
	rows := []UserActivity{ua}
	breaks := VerifyChain(rows, "abc123checkpoint")
	require.Empty(t, breaks, "checkpoint override must anchor the chain")
}

func TestVerifyChain_ReportsEveryBreak(t *testing.T) {
	t.Parallel()
	var rows []UserActivity
	prev := ""
	for i := 0; i < 6; i++ {
		ua := UserActivity{
			ID: int64(i + 1), Ts: int64(1700000000000 + i*1000),
			UserID: "u1", Action: ActionAuthLogin, Outcome: OutcomeSuccess, DetailJSON: `{}`,
			PrevHash: prev,
		}
		h, _ := ComputeSelfHash(prev, &ua)
		ua.SelfHash = h
		rows = append(rows, ua)
		prev = h
	}
	// Physically remove rows 3 and 5, mimicking two independent interior
	// deletions: rows 4 and 6 now reference self_hashes that no longer exist.
	rows = append(rows[:2], rows[3:]...)
	rows = append(rows[:3], rows[4:]...)

	breaks := VerifyChain(rows, "")
	require.Len(t, breaks, 2, "both orphaned rows must be reported")
	require.Equal(t, int64(4), breaks[0].ID)
	require.Equal(t, int64(6), breaks[1].ID)
	require.NotEqual(t, breaks[0].Expected, breaks[0].Actual)
	require.NotEqual(t, breaks[1].Expected, breaks[1].Actual)
}
