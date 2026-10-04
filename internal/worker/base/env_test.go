package base

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
)

// setEnvForTest sets a process-wide variable for one test and restores the
// previous value afterwards.
//
// These tests used to clean up with defer os.Unsetenv, which is not a restore:
// it DESTROYS PATH and HOME for the whole test binary. Any test running later
// then sees a gutted environment and fails for reasons that have nothing to do
// with its own assertions — the leak this helper removes.
func setEnvForTest(t *testing.T, key, value string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	require.NoError(t, os.Setenv(key, value))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestBuildEnv_BlocklistFiltersBlockedVars(t *testing.T) {
	setEnvForTest(t, "HOME", "/home/test")
	setEnvForTest(t, "PATH", "/usr/bin")
	setEnvForTest(t, "SECRET_KEY", "should-be-filtered")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	blocklist := []string{"SECRET_KEY"}
	env := BuildEnv(session, blocklist, "test-worker")

	// Blocked vars should NOT be present.
	for _, e := range env {
		require.NotEqual(t, "SECRET_KEY=should-be-filtered", e, "SECRET_KEY should be blocked")
	}

	// Non-blocked vars should pass through.
	foundHOME := false
	foundPATH := false
	for _, e := range env {
		if e == "HOME=/home/test" {
			foundHOME = true
		}
		if e == "PATH=/usr/bin" {
			foundPATH = true
		}
	}
	require.True(t, foundHOME, "HOME should pass through")
	require.True(t, foundPATH, "PATH should pass through")

	// HOTPLEX vars are always added.
	require.Contains(t, env, "HOTPLEX_SESSION_ID=test-session")
	require.Contains(t, env, "HOTPLEX_WORKER_TYPE=test-worker")
}

func TestBuildEnv_SessionVarsOverride(t *testing.T) {
	setEnvForTest(t, "MY_VAR", "original-value")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env: map[string]string{
			"MY_VAR": "session-value",
		},
	}

	blocklist := []string{}
	env := BuildEnv(session, blocklist, "test-worker")

	// session.Env should override os.Environ value.
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "MY_VAR=") {
			require.Equal(t, "MY_VAR=session-value", e, "session.Env should override os.Environ")
			found = true
		}
	}
	require.True(t, found, "MY_VAR should be present")
}

func TestBuildEnv_HotPlexVars(t *testing.T) {
	session := worker.SessionInfo{
		SessionID:  "session-123",
		UserID:     "user-456",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	env := BuildEnv(session, nil, "claude-code")

	require.Contains(t, env, "HOTPLEX_SESSION_ID=session-123")
	require.Contains(t, env, "HOTPLEX_WORKER_TYPE=claude-code")
}

func TestBuildEnv_StripNestedAgent(t *testing.T) {
	setEnvForTest(t, "CLAUDECODE", "nested-agent-config")
	setEnvForTest(t, "HOME", "/home/test")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	// CLAUDECODE is in the blocklist AND stripped by StripNestedAgent.
	blocklist := []string{"CLAUDECODE"}
	env := BuildEnv(session, blocklist, "test-worker")

	for _, e := range env {
		require.NotEqual(t, "CLAUDECODE=nested-agent-config", e, "CLAUDECODE should be stripped")
		require.NotEqual(t, "CLAUDECODE=", e, "CLAUDECODE should be stripped")
	}
}

func TestBuildEnv_EmptyBlocklistPassesAll(t *testing.T) {
	setEnvForTest(t, "HOME", "/home/test")
	setEnvForTest(t, "SOME_CUSTOM_VAR", "custom-value")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	env := BuildEnv(session, nil, "test-worker")

	// Empty blocklist = all os.Environ vars pass through.
	foundHOME := false
	foundCustom := false
	for _, e := range env {
		if e == "HOME=/home/test" {
			foundHOME = true
		}
		if e == "SOME_CUSTOM_VAR=custom-value" {
			foundCustom = true
		}
	}
	require.True(t, foundHOME, "HOME should pass through with empty blocklist")
	require.True(t, foundCustom, "SOME_CUSTOM_VAR should pass through with empty blocklist")
}

func TestBuildEnv_SessionOnlyVars(t *testing.T) {
	setEnvForTest(t, "HOME", "/home/test")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env: map[string]string{
			"CUSTOM_VAR": "custom-value",
			"ANOTHER":    "another-value",
		},
	}

	env := BuildEnv(session, nil, "test-worker")

	require.Contains(t, env, "CUSTOM_VAR=custom-value")
	require.Contains(t, env, "ANOTHER=another-value")
}

func TestBuildEnv_PrefixBlocklist(t *testing.T) {
	setEnvForTest(t, "HOME", "/home/test")
	setEnvForTest(t, "HOTPLEX_JWT_SECRET", "super-secret")
	setEnvForTest(t, "HOTPLEX_ADMIN_TOKEN_1", "admin-secret")
	setEnvForTest(t, "HOTPLEX_CUSTOM_VAR", "should-be-blocked")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	// HOTPLEX_ with trailing "_" is a prefix match.
	blocklist := []string{"HOTPLEX_"}
	env := BuildEnv(session, blocklist, "test-worker")

	// HOME should pass through.
	foundHOME := false
	for _, e := range env {
		if e == "HOME=/home/test" {
			foundHOME = true
		}
	}
	require.True(t, foundHOME, "HOME should pass through")

	// All HOTPLEX_ vars should be blocked.
	for _, e := range env {
		require.False(t, strings.HasPrefix(e, "HOTPLEX_JWT_SECRET="), "HOTPLEX_JWT_SECRET should be blocked")
		require.False(t, strings.HasPrefix(e, "HOTPLEX_ADMIN_TOKEN_1="), "HOTPLEX_ADMIN_TOKEN_1 should be blocked")
		require.False(t, strings.HasPrefix(e, "HOTPLEX_CUSTOM_VAR="), "HOTPLEX_CUSTOM_VAR should be blocked")
	}

	// HOTPLEX_SESSION_ID and HOTPLEX_WORKER_TYPE are added after filtering.
	require.Contains(t, env, "HOTPLEX_SESSION_ID=test-session")
	require.Contains(t, env, "HOTPLEX_WORKER_TYPE=test-worker")
}

func TestBuildEnv_ConfigEnvOverrides(t *testing.T) {
	setEnvForTest(t, "MY_VAR", "original")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
		ConfigEnv:  []string{"MY_VAR=overridden"},
	}

	env := BuildEnv(session, nil, "test-worker")

	// ConfigEnv should override os.Environ value.
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "MY_VAR=") {
			require.Equal(t, "MY_VAR=overridden", e, "ConfigEnv should override os.Environ")
			found = true
		}
	}
	require.True(t, found, "MY_VAR should be present")
}

func TestBuildEnv_WorkerSecretStripping(t *testing.T) {
	setEnvForTest(t, "HOTPLEX_WORKER_GITHUB_TOKEN", "from-dotenv")
	setEnvForTest(t, "GITHUB_TOKEN", "from-system")
	setEnvForTest(t, "HOME", "/home/test")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	env := BuildEnv(session, nil, "test-worker")

	require.Contains(t, env, "GITHUB_TOKEN=from-dotenv", "stripped HOTPLEX_WORKER_ var should be injected")
	for _, e := range env {
		require.NotEqual(t, "GITHUB_TOKEN=from-system", e, "system-level var should be blocked when HOTPLEX_WORKER_ override exists")
		require.False(t, strings.HasPrefix(e, "HOTPLEX_WORKER_GITHUB_TOKEN="), "HOTPLEX_WORKER_ prefixed version should not appear")
	}
}

func TestBuildEnv_GatewayVarsNotStripped(t *testing.T) {
	setEnvForTest(t, "HOTPLEX_JWT_SECRET", "jwt-secret")
	setEnvForTest(t, "HOTPLEX_ADMIN_TOKEN_1", "admin-token")
	setEnvForTest(t, "HOTPLEX_WORKER_GITHUB_TOKEN", "github-token")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	env := BuildEnv(session, nil, "test-worker")

	// Gateway-internal HOTPLEX_ vars should NOT be stripped or passed through.
	for _, e := range env {
		require.False(t, strings.HasPrefix(e, "JWT_SECRET="), "JWT_SECRET should not be injected")
		require.False(t, strings.HasPrefix(e, "ADMIN_TOKEN_1="), "ADMIN_TOKEN_1 should not be injected")
	}
	// Only HOTPLEX_WORKER_ vars are stripped.
	require.Contains(t, env, "GITHUB_TOKEN=github-token", "HOTPLEX_WORKER_ var should be stripped")
}

func TestBuildEnv_SystemVarPassesWithoutWorkerOverride(t *testing.T) {
	setEnvForTest(t, "MY_SECRET", "system-value")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env:        nil,
	}

	env := BuildEnv(session, nil, "test-worker")
	require.Contains(t, env, "MY_SECRET=system-value", "system var should pass when no HOTPLEX_WORKER_ override exists")
}

func TestBuildEnv_PriorityOrder(t *testing.T) {
	setEnvForTest(t, "MY_KEY", "system")
	setEnvForTest(t, "HOTPLEX_WORKER_MY_KEY", "from-stripping")

	session := worker.SessionInfo{
		SessionID:  "test-session",
		UserID:     "test-user",
		ProjectDir: "/tmp",
		Env: map[string]string{
			"MY_KEY": "from-session",
		},
		ConfigEnv: []string{"MY_KEY=from-config"},
	}

	env := BuildEnv(session, nil, "test-worker")
	require.Contains(t, env, "MY_KEY=from-config", "ConfigEnv should have highest priority")
}
