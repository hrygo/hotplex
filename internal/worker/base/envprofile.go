package base

// Environment profiles (#946 plan unit E).
//
// "compat" is today's behaviour and stays the default: the worker inherits the
// host environment minus a blocklist. That is a denylist, and a denylist can
// only block what someone already thought to enumerate.
//
// "strict" inverts it. The worker starts from a minimal system allowlist and
// receives only what it was explicitly given. Two things this file is careful
// NOT to claim:
//
//   - Passing HOME is not filesystem isolation. It is one variable; the worker
//     can still read any path the OS lets it.
//   - A narrower environment is not a sandbox. Isolation claims come from the
//     IsolationReporter, which reports what a backend actually enforced.
//
// The allowlist below is a deliberately small STARTING point for tests, not a
// curated set of everything a CLI might need. Adding a key is a decision with
// a reason attached, which is the point.

import (
	"runtime"
	"slices"
	"strings"

	"github.com/hrygo/hotplex/internal/worker"
)

// posixSystemAllowKeys is the minimal system environment a strict worker
// inherits from the host. Everything else must be injected explicitly.
var posixSystemAllowKeys = []string{
	"PATH",
	"HOME",
	"TMPDIR",
	"LANG",
}

// windowsSystemAllowKeys are the Windows equivalents. Windows resolves the
// executable search path and the system directory through these, and a worker
// without them generally cannot start at all.
var windowsSystemAllowKeys = []string{
	"SystemRoot",
	"WINDIR",
	"TEMP",
	"TMP",
	"PATHEXT",
	"USERPROFILE",
}

// localeKeyPrefix admits the whole LC_* family on POSIX. Treating the family
// by prefix rather than listing locales keeps a strict worker usable without
// turning the allowlist into a locale inventory.
const localeKeyPrefix = "LC_"

// systemAllowKeys returns the minimal system allowlist for the current OS.
func systemAllowKeys() []string {
	if runtime.GOOS == "windows" {
		return slices.Clone(windowsSystemAllowKeys)
	}
	return slices.Clone(posixSystemAllowKeys)
}

// envKeyAllowedBySystemAllowlist reports whether a host key qualifies under the
// system allowlist for this OS.
func envKeyAllowedBySystemAllowlist(key string) bool {
	if key == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// Windows environment names are case-insensitive, so the allowlist
		// comparison must be too.
		return slices.ContainsFunc(systemAllowKeys(), func(k string) bool {
			return strings.EqualFold(k, key)
		})
	}
	if slices.Contains(posixSystemAllowKeys, key) {
		return true
	}
	return strings.HasPrefix(key, localeKeyPrefix) && len(key) > len(localeKeyPrefix)
}

// invalidEnvKey reports whether a key can never be a real environment
// variable name. Rejecting these keeps a malformed config from producing an
// entry the OS silently drops — which would then look, in a diagnostic, like
// the variable had been correctly excluded.
func invalidEnvKey(key string) bool {
	return key == "" || strings.ContainsAny(key, "=\x00\n\r")
}

// dedupeEnvKey normalizes an environment key for case-insensitive comparison
// on Windows and leaves POSIX keys untouched (POSIX env names ARE
// case-sensitive, and folding them would merge two distinct variables).
func dedupeEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

// envKeyIndex returns the index of key within env (a "KEY=value" slice), or -1.
// Comparison follows the OS's own semantics for key names.
func envKeyIndex(env []string, key string) int {
	want := dedupeEnvKey(key)
	for i, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if ok && dedupeEnvKey(k) == want {
			return i
		}
	}
	return -1
}

// SetEnv upserts key=value into env using OS-correct key semantics, replacing
// any existing entry for that key.
func SetEnv(env []string, key, value string) []string {
	entry := key + "=" + value
	if i := envKeyIndex(env, key); i >= 0 {
		env[i] = entry
		return env
	}
	return append(env, entry)
}

// EnvHasKey reports whether env already carries key.
func EnvHasKey(env []string, key string) bool {
	return envKeyIndex(env, key) >= 0
}

// StripEnv removes every entry for key, regardless of case semantics.
func StripEnv(env []string, key string) []string {
	want := dedupeEnvKey(key)
	out := env[:0]
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok && dedupeEnvKey(k) == want {
			continue
		}
		out = append(out, e)
	}
	return out
}

// NormalizeEnvProfile maps an absent or unrecognized profile onto compat.
// An unknown value must never be misread as strict, and must never widen the
// environment by being treated as "no profile at all" in a strict code path.
func NormalizeEnvProfile(profile string) string {
	if profile == worker.EnvProfileStrict {
		return worker.EnvProfileStrict
	}
	return worker.EnvProfileCompat
}
