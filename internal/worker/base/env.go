package base

import (
	"os"
	"slices"
	"strings"

	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/worker"
)

// hasAnyPrefix reports whether s has any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

// workerSecretPrefix is the .env prefix for secrets that should be passed to
// worker subprocesses. Only vars with this prefix are stripped and injected.
// All other HOTPLEX_* vars are gateway-internal and never reach workers.
const workerSecretPrefix = "HOTPLEX_WORKER_"

// BuildEnv constructs the environment variables for a CLI worker process.
//
// Priority (low → high):
//  1. os.Environ() — filtered through blocklist (compat), or through the
//     per-OS system allowlist plus EnvAllowKeys (strict)
//  2. HOTPLEX_WORKER_ prefix-stripped injections from .env
//  3. session.Env — per-session overrides
//  4. ConfigEnv — highest priority config overrides
//
// HOTPLEX_WORKER_ prefix stripping:
//
//	Only vars prefixed with HOTPLEX_WORKER_ are stripped and passed to workers.
//	Example: HOTPLEX_WORKER_GITHUB_TOKEN=xxx → GITHUB_TOKEN=xxx in worker env.
//	All other HOTPLEX_* vars (ADMIN_TOKEN, etc.) are gateway-internal
//	and blocked from reaching workers.
//	When a stripped var exists, the system-level version is dynamically blocked
//	to prevent the gateway's own secrets from leaking to workers.
//
// Blocklist entries ending with "_" are treated as prefix matches.
//
// The nested-agent scrub runs LAST, after every merge, because it is a
// force-disabled item rather than a default. Running it earlier let
// worker.environment reintroduce CLAUDECODE on the very next phase, so an
// operator override could silently restore something the system forbids.
func BuildEnv(session worker.SessionInfo, blocklist []string, workerTypeLabel string) []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ))
	strict := NormalizeEnvProfile(session.EnvProfile) == worker.EnvProfileStrict

	// Build blocklist set, tracking prefix entries.
	blockSet := make(map[string]bool)
	prefixKeys := make([]string, 0)
	for _, k := range blocklist {
		if strings.HasSuffix(k, "_") {
			prefixKeys = append(prefixKeys, k)
		} else {
			blockSet[k] = true
		}
	}

	// Merge config-driven blocklist (worker.env_blocklist).
	for _, k := range session.ConfigBlocklist {
		if strings.HasSuffix(k, "_") {
			prefixKeys = append(prefixKeys, k)
		} else {
			blockSet[k] = true
		}
	}

	// Phase 1: Scan HOTPLEX_WORKER_* vars for prefix stripping.
	// HOTPLEX_WORKER_GITHUB_TOKEN → GITHUB_TOKEN.
	// Only HOTPLEX_WORKER_ prefix is stripped; other HOTPLEX_* vars are untouched.
	stripMap := make(map[string]string)
	for _, e := range environ {
		key, val, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, workerSecretPrefix) {
			continue
		}
		strippedKey := strings.TrimPrefix(key, workerSecretPrefix)
		if strippedKey == "" {
			continue
		}
		stripMap[strippedKey] = val
	}

	// Phase 2: Filter os.Environ() through blocklist + dynamic blocks.
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if blockSet[key] || hasAnyPrefix(key, prefixKeys) {
			continue
		}
		// HOTPLEX_WORKER_* vars are handled by stripping, not passed through directly.
		if strings.HasPrefix(key, workerSecretPrefix) {
			continue
		}
		// Dynamic block: system-level version of a stripped var is blocked
		// (e.g., system GITHUB_TOKEN blocked when HOTPLEX_WORKER_GITHUB_TOKEN exists).
		if _, blocked := stripMap[key]; blocked {
			continue
		}
		// Strict narrows the host contribution to the system allowlist plus
		// the operator's explicit additions. The blocklist above still
		// applies: strict is not a reason to re-admit a blocked key.
		if strict && !envKeyAllowedBySystemAllowlist(key) && !slices.Contains(session.EnvAllowKeys, key) {
			continue
		}

		env = append(env, e)
	}

	// Phase 3: Add HOTPLEX session vars.
	env = append(env,
		"HOTPLEX_SESSION_ID="+session.SessionID,
		"HOTPLEX_WORKER_TYPE="+workerTypeLabel,
	)

	// Phase 4: Inject stripped HOTPLEX_WORKER_* vars (override system env).
	for k, v := range stripMap {
		env = SetEnv(env, k, v)
	}

	// Phase 5: Add session-specific env vars (override stripped vars).
	for k, v := range session.Env {
		if invalidEnvKey(k) {
			continue
		}
		env = SetEnv(env, k, v)
	}

	// Phase 6: Apply config-driven env vars (worker.environment). Highest priority.
	for _, e := range session.ConfigEnv {
		if e == "" || !strings.Contains(e, "=") {
			continue
		}
		k, v, _ := strings.Cut(e, "=")
		if invalidEnvKey(k) {
			continue
		}
		env = SetEnv(env, k, v)
	}

	// Phase 7: Strip nested agent config (CLAUDECODE=). This is a
	// FORCE-disabled item, so it runs after every merge — otherwise the
	// config phase above could put it straight back.
	env = security.StripNestedAgent(env)

	return env
}

// BuildProcessEnv builds the environment for a Worker process that serves MANY
// sessions (opencode serve, codex app-server).
//
// The profile is passed explicitly because there is no SessionInfo to carry
// it: for a shared process the environment belongs to the PROCESS, and every
// session on it sees exactly the same one. Reporting a per-session env here
// would be fiction, so the caller gets a process-scoped answer instead.
func BuildProcessEnv(
	profile string, allowKeys []string, blocklist []string, workerTypeLabel string,
) []string {
	return BuildEnv(worker.SessionInfo{
		EnvProfile:   profile,
		EnvAllowKeys: allowKeys,
	}, blocklist, workerTypeLabel)
}
