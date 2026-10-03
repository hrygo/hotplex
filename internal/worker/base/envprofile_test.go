package base

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/worker"
)

// setHostEnv sets a process-wide variable for the duration of the test and
// restores it afterwards.
//
// It uses os.Setenv rather than t.Setenv because t.Setenv forbids
// t.Parallel(), and every test here uses a distinct sentinel key, so
// concurrent execution is safe. The one shared key (PATH) is never set — it
// already exists in the environment.
func setHostEnv(t *testing.T, key, value string) {
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

func envValue(env []string, key string) (string, bool) {
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && dedupeEnvKey(k) == dedupeEnvKey(key) {
			return v, true
		}
	}
	return "", false
}

// TestStrictEnv_ExcludesUnknownHostKeys is the core E1 guarantee: a host
// variable nobody allowlisted does not reach a strict worker.
func TestStrictEnv_ExcludesUnknownHostKeys(t *testing.T) {
	t.Parallel()
	// A sentinel no allowlist could plausibly contain.
	const sentinel = "HOTPLEX_TEST_ENV_SENTINEL_XYZ"
	setHostEnv(t, sentinel, "leaked")

	compat := BuildEnv(worker.SessionInfo{}, nil, "test")
	v, ok := envValue(compat, sentinel)
	require.True(t, ok, "compat inherits the host environment; that is the default")
	require.Equal(t, "leaked", v)

	strict := BuildEnv(worker.SessionInfo{EnvProfile: worker.EnvProfileStrict}, nil, "test")
	_, ok = envValue(strict, sentinel)
	require.False(t, ok, "strict must not inherit an unlisted host variable")
}

func TestStrictEnv_KeepsTheSystemAllowlist(t *testing.T) {
	t.Parallel()
	strict := BuildEnv(worker.SessionInfo{EnvProfile: worker.EnvProfileStrict}, nil, "test")

	v, ok := envValue(strict, "PATH")
	if runtime.GOOS == "windows" {
		require.False(t, ok, "windows uses Path, not PATH")
	} else {
		require.True(t, ok, "a strict worker still needs PATH to find its binary")
		require.Equal(t, os.Getenv("PATH"), v)
	}
}

func TestStrictEnv_AdmitsOnlyExplicitlyAllowedExtraKeys(t *testing.T) {
	t.Parallel()
	const allowed = "HOTPLEX_TEST_ENV_ALLOWED_XYZ"
	const denied = "HOTPLEX_TEST_ENV_DENIED_XYZ"
	setHostEnv(t, allowed, "yes")
	setHostEnv(t, denied, "no")

	strict := BuildEnv(worker.SessionInfo{
		EnvProfile:   worker.EnvProfileStrict,
		EnvAllowKeys: []string{allowed},
	}, nil, "test")

	_, ok := envValue(strict, allowed)
	require.True(t, ok, "an operator-allowlisted key must pass through")
	_, ok = envValue(strict, denied)
	require.False(t, ok)
}

// TestBlocklistStillAppliesInStrict guards against "strict" being read as a
// reason to re-admit something the operator explicitly blocked.
func TestBlocklistStillAppliesInStrict(t *testing.T) {
	t.Parallel()
	const blocked = "HOTPLEX_TEST_ENV_BLOCKED_XYZ"
	setHostEnv(t, blocked, "value")

	strict := BuildEnv(worker.SessionInfo{
		EnvProfile:   worker.EnvProfileStrict,
		EnvAllowKeys: []string{blocked},
	}, []string{blocked}, "test")

	_, ok := envValue(strict, blocked)
	require.False(t, ok, "an explicit block outranks an explicit allow")
}

// TestForceDisabledItemCannotBeRestoredByOverride is a regression test for a
// real ordering bug: the nested-agent scrub used to run BEFORE the config
// merge, so worker.environment could put CLAUDECODE straight back on the very
// next phase. A force-disabled item must be disabled after every merge.
func TestForceDisabledItemCannotBeRestoredByOverride(t *testing.T) {
	t.Parallel()
	setHostEnv(t, "CLAUDECODE", "from-host")

	fromHost := BuildEnv(worker.SessionInfo{}, nil, "test")
	_, ok := envValue(fromHost, "CLAUDECODE")
	require.False(t, ok, "the host value is scrubbed")

	viaConfig := BuildEnv(worker.SessionInfo{
		ConfigEnv: []string{"CLAUDECODE=1"},
	}, nil, "test")
	_, ok = envValue(viaConfig, "CLAUDECODE")
	require.False(t, ok,
		"worker.environment must not be able to restore a force-disabled item")

	viaSession := BuildEnv(worker.SessionInfo{
		Env: map[string]string{"CLAUDECODE": "1"},
	}, nil, "test")
	_, ok = envValue(viaSession, "CLAUDECODE")
	require.False(t, ok)
}

func TestInvalidEnvKeysAreDropped(t *testing.T) {
	t.Parallel()
	built := BuildEnv(worker.SessionInfo{
		Env: map[string]string{
			"":        "empty key",
			"BAD=KEY": "embedded equals",
		},
		ConfigEnv: []string{"NOEQUALS", "=leading", "GOOD=value"},
	}, nil, "test")

	_, ok := envValue(built, "BAD=KEY")
	require.False(t, ok)
	_, ok = envValue(built, "NOEQUALS")
	require.False(t, ok)
	_, ok = envValue(built, "GOOD")
	require.True(t, ok, "a well-formed entry still lands")
}

func TestNormalizeEnvProfile_UnknownValueIsCompat(t *testing.T) {
	t.Parallel()
	require.Equal(t, worker.EnvProfileStrict, NormalizeEnvProfile(worker.EnvProfileStrict))
	require.Equal(t, worker.EnvProfileCompat, NormalizeEnvProfile("STRICT"))
	require.Equal(t, worker.EnvProfileCompat, NormalizeEnvProfile(""))
	require.Equal(t, worker.EnvProfileCompat, NormalizeEnvProfile("typo"))
	require.Equal(t, worker.EnvProfileCompat, NormalizeEnvProfile("no-such-profile"),
		"an unknown profile must never be read as strict")
}

func TestDedupeEnvKey_FollowsOSSemantics(t *testing.T) {
	t.Parallel()
	// POSIX env names are case-sensitive; folding them would merge two
	// distinct variables. Windows names are not.
	got := dedupeEnvKey("Path")
	if runtime.GOOS == "windows" {
		require.Equal(t, "PATH", got)
	} else {
		require.Equal(t, "Path", got)
	}
}

func TestBuildProcessEnv_AppliesAScopedProfile(t *testing.T) {
	t.Parallel()
	const sentinel = "HOTPLEX_TEST_PROCESS_SENTINEL_XYZ"
	setHostEnv(t, sentinel, "leaked")

	strict := BuildProcessEnv(worker.EnvProfileStrict, nil, nil, "shared")
	_, ok := envValue(strict, sentinel)
	require.False(t, ok,
		"a shared process under strict must not inherit an unlisted host key")

	compat := BuildProcessEnv("", nil, nil, "shared")
	_, ok = envValue(compat, sentinel)
	require.True(t, ok, "an absent profile keeps today's behaviour")
}
