package agentspec

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
)

// errUnknownType is the sentinel returned by the hermetic test validator.
var errUnknownType = errors.New("unknown worker type")

// knownTypes is the static set the test validator accepts — keeps Resolve
// hermetic (no dependency on the global worker registry).
var knownTypes = map[string]struct{}{
	"claude_code":     {},
	"opencode_server": {},
	"codex_cli":       {},
	"acp":             {},
}

func staticValidator(wt string) error {
	if _, ok := knownTypes[wt]; ok {
		return nil
	}
	return errUnknownType
}

// testResolver returns a Resolver wired with the static (registry-free) validator.
func testResolver() Resolver { return Resolver{ValidateWorkerType: staticValidator} }

func TestResolve_WorkerType_Explicit(t *testing.T) {
	t.Parallel()
	for _, wt := range []string{"claude_code", "opencode_server", "codex_cli", "acp"} {
		t.Run(wt, func(t *testing.T) {
			t.Parallel()
			spec, err := testResolver().Resolve(Input{
				InitMeta: InitMetadata{WorkerType: wt},
				Platform: "webchat",
			})
			require.NoError(t, err)
			require.Equal(t, wt, spec.Worker.Type)
		})
	}
}

func TestResolve_WorkerType_EmptyAllowed(t *testing.T) {
	t.Parallel()
	// WebChat passes an empty worker_type through today; Resolve must not reject it.
	spec, err := testResolver().Resolve(Input{Platform: "webchat"})
	require.NoError(t, err)
	require.Equal(t, "", spec.Worker.Type)
}

func TestResolve_WorkerType_UnknownRejected(t *testing.T) {
	t.Parallel()
	_, err := testResolver().Resolve(Input{
		InitMeta: InitMetadata{WorkerType: "bogus_worker"},
		Platform: "webchat",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errUnknownType)
}

// TestResolve_WorkerType_MessagingFallback exercises the documented 5-level
// fallback via config.ResolveWorkerType, level by level.
func TestResolve_WorkerType_MessagingFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*config.Config)
		bot    string
		want   string
	}{
		{
			name:   "level5 compile default",
			mutate: func(c *config.Config) { c.Messaging.WorkerType = "" },
			want:   "claude_code",
		},
		{
			name:   "level4 messaging shared default",
			mutate: func(c *config.Config) { c.Messaging.WorkerType = "codex_cli" },
			want:   "codex_cli",
		},
		{
			name: "level2 platform overrides messaging",
			mutate: func(c *config.Config) {
				c.Messaging.WorkerType = "codex_cli"
				c.Messaging.Slack.WorkerType = "acp"
			},
			want: "acp",
		},
		{
			name: "level1 per-bot overrides platform",
			mutate: func(c *config.Config) {
				c.Messaging.WorkerType = "codex_cli"
				c.Messaging.Slack.WorkerType = "acp"
				c.Messaging.Slack.Bots = []config.SlackBotConfig{
					{Name: "b1", WorkerType: "opencode_server"},
				}
			},
			bot:  "b1",
			want: "opencode_server",
		},
		{
			name: "per-bot without override falls through to platform",
			mutate: func(c *config.Config) {
				c.Messaging.Slack.WorkerType = "acp"
				c.Messaging.Slack.Bots = []config.SlackBotConfig{{Name: "b2"}}
			},
			bot:  "b2",
			want: "acp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			tc.mutate(cfg)
			spec, err := testResolver().Resolve(Input{
				Cfg:      cfg,
				Platform: "slack",
				BotName:  tc.bot,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, spec.Worker.Type)
		})
	}
}

// TestResolve_WorkerType_WebchatIgnoresConfigFallback: webchat is request-driven;
// Resolve must NOT apply the config messaging fallback for it.
func TestResolve_WorkerType_WebchatIgnoresConfigFallback(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Messaging.WorkerType = "codex_cli"
	spec, err := testResolver().Resolve(Input{Cfg: cfg, Platform: "webchat"})
	require.NoError(t, err)
	require.Equal(t, "", spec.Worker.Type) // not "codex_cli"
}

func TestResolve_PermissionMode(t *testing.T) {
	t.Parallel()

	t.Run("explicit tier", func(t *testing.T) {
		t.Parallel()
		spec, err := testResolver().Resolve(Input{
			InitMeta: InitMetadata{PermissionMode: "read-only"},
			Platform: "webchat",
		})
		require.NoError(t, err)
		require.Equal(t, "read-only", spec.Policy.PermissionMode)
	})

	t.Run("workspace fallback when no explicit", func(t *testing.T) {
		t.Parallel()
		spec, err := testResolver().Resolve(Input{
			WorkspacePerm: "bypass",
			Platform:      "webchat",
		})
		require.NoError(t, err)
		require.Equal(t, "bypass", spec.Policy.PermissionMode)
	})

	t.Run("explicit wins over workspace", func(t *testing.T) {
		t.Parallel()
		spec, err := testResolver().Resolve(Input{
			InitMeta:      InitMetadata{PermissionMode: "workspace"},
			WorkspacePerm: "bypass",
			Platform:      "webchat",
		})
		require.NoError(t, err)
		require.Equal(t, "workspace", spec.Policy.PermissionMode)
	})

	t.Run("empty is worker default", func(t *testing.T) {
		t.Parallel()
		spec, err := testResolver().Resolve(Input{Platform: "webchat"})
		require.NoError(t, err)
		require.Equal(t, "", spec.Policy.PermissionMode)
	})

	t.Run("unknown tier rejected", func(t *testing.T) {
		t.Parallel()
		_, err := testResolver().Resolve(Input{
			InitMeta: InitMetadata{PermissionMode: "god-mode"},
			Platform: "webchat",
		})
		require.Error(t, err)
	})
}

// TestResolve_AllowedModelsNotInjected (finding F1): normalization must NOT
// populate AllowedModels — injection is a deferred behavior change.
func TestResolve_AllowedModelsNotInjected(t *testing.T) {
	t.Parallel()
	spec, err := testResolver().Resolve(Input{
		InitMeta: InitMetadata{Model: "claude-sonnet-4", WorkerType: "claude_code"},
		Platform: "webchat",
	})
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4", spec.Worker.Model)
	require.Nil(t, spec.Worker.AllowedModels, "F1: AllowedModels must not be injected in first-cut")
}

// TestResolve_SecretFree asserts the secret-free invariant: secrets present in
// config and request never leak into the AgentSpec, and the AgentSpec type has
// no secret-shaped fields.
func TestResolve_SecretFree(t *testing.T) {
	t.Parallel()

	const (
		envSecret    = "hunter2-env-secret"
		tokenSecret  = "xoxb-slack-token-secret"
		feishuSecret = "feishu-app-secret-value"
	)
	cfg := config.Default()
	cfg.Worker.Environment = []string{"MY_KEY=" + envSecret}
	cfg.Worker.ClaudeCode.Command = "claude"
	cfg.Messaging.Slack.BotToken = tokenSecret
	cfg.Messaging.Feishu.AppSecret = feishuSecret

	spec, err := testResolver().Resolve(Input{
		Cfg:      cfg,
		InitMeta: InitMetadata{WorkerType: "claude_code", AllowedTools: []string{"Bash"}},
		Platform: "slack",
		UserID:   "u1",
	})
	require.NoError(t, err)

	// 1) JSON serialization must not contain any secret sentinel.
	raw, jerr := json.Marshal(spec)
	require.NoError(t, jerr)
	s := string(raw)
	for _, secret := range []string{envSecret, tokenSecret, feishuSecret} {
		require.NotContains(t, s, secret, "AgentSpec leaked a secret")
	}

	// 2) Structural: no field name shaped like a secret carrier.
	denylist := []string{"env", "key", "token", "secret", "credential", "password"}
	assertNoSecretFields(t, reflect.TypeFor[AgentSpec](), denylist)
}

func assertNoSecretFields(t *testing.T, typ reflect.Type, denylist []string) {
	t.Helper()
	for f := range typ.Fields() {
		lower := strings.ToLower(f.Name)
		for _, d := range denylist {
			require.NotContains(t, lower, d, "AgentSpec has a secret-shaped field %q", f.Name)
		}
		if f.Type.Kind() == reflect.Struct {
			assertNoSecretFields(t, f.Type, denylist)
		}
	}
}

// TestResolve_Determinism: the same Input always yields the same AgentSpec
// (pure function — the foundation of the WS≡REST equivalence proof).
func TestResolve_Determinism(t *testing.T) {
	t.Parallel()
	in := Input{
		InitMeta: InitMetadata{WorkerType: "codex_cli", AllowedTools: []string{"Read", "Grep"}},
		Platform: "webchat",
		UserID:   "u1",
	}
	a, err := testResolver().Resolve(in)
	require.NoError(t, err)
	b, err := testResolver().Resolve(in)
	require.NoError(t, err)
	require.Equal(t, a, b)
}

// TestResolve_WSRESTDivergenceContained documents the only first-cut divergence
// between the WS and REST entries (finding F4): AllowedTools. Given two Inputs
// that are semantically equal except for the AllowedTools source, the AgentSpecs
// must differ ONLY in Policy.AllowedTools — the divergence is contained, never
// silently injected.
func TestResolve_WSRESTDivergenceContained(t *testing.T) {
	t.Parallel()
	base := Input{
		InitMeta: InitMetadata{WorkerType: "claude_code"},
		Platform: "webchat",
		UserID:   "u1",
	}
	wsLike := base
	wsLike.InitMeta.AllowedTools = []string{"Bash"} // WS init carries AllowedTools
	restLike := base                                // REST create has no AllowedTools source → nil

	wsSpec, err := testResolver().Resolve(wsLike)
	require.NoError(t, err)
	restSpec, err := testResolver().Resolve(restLike)
	require.NoError(t, err)

	require.Equal(t, []string{"Bash"}, wsSpec.Policy.AllowedTools)
	require.Nil(t, restSpec.Policy.AllowedTools, "REST must not have AllowedTools injected")
	// The presence flag is part of the SAME decision, not a new divergence:
	// it records that one entry declared a tool list and the other declared
	// nothing, which is exactly the F4 gap.
	require.True(t, wsSpec.Policy.AllowedToolsSet)
	require.False(t, restSpec.Policy.AllowedToolsSet)

	// Everything else identical.
	wsSpec.Policy.AllowedTools = nil
	restSpec.Policy.AllowedTools = nil
	wsSpec.Policy.AllowedToolsSet = false
	restSpec.Policy.AllowedToolsSet = false
	require.Equal(t, wsSpec, restSpec)
}
