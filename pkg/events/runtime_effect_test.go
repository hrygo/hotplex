package events

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// #849: effect/reconcile/operator 事件向后兼容，老客户端忽略未知 Kind。
func TestRuntimeEffectKinds_AreAdditiveForOldClients(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind Kind
		data any
	}{
		{name: "planned", kind: RuntimeEffectPlanned,
			data: RuntimeEffectData{ExecutionID: "exec_test", EffectID: "eff_1", Status: "planned"}},
		{name: "reconciled", kind: RuntimeEffectReconciled,
			data: RuntimeEffectData{ExecutionID: "exec_test", EffectID: "eff_1", Status: "reconciled_succeeded", EvidenceRef: "lookup-1"}},
		{name: "fenced", kind: RuntimeEffectFenced,
			data: RuntimeEffectData{ExecutionID: "exec_test", EffectID: "eff_1", Status: "fenced"}},
		{name: "operator", kind: RuntimeOperatorAction,
			data: RuntimeOperatorActionData{ExecutionID: "exec_test", EffectID: "eff_1", Decision: "abandon", Status: "failed"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := NewEnvelope("msg_test", "session_test", 1, tc.kind, tc.data)
			env.Timestamp = 1700000000000

			raw, err := json.Marshal(env)
			require.NoError(t, err)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, string(tc.kind), decoded["event"].(map[string]any)["type"])

			var reEnv Envelope
			require.NoError(t, json.Unmarshal(raw, &reEnv))
			require.Equal(t, tc.kind, reEnv.Event.Type)
		})
	}
}

func TestRuntimeEffectKinds_KindStrings(t *testing.T) {
	t.Parallel()

	require.Equal(t, "runtime.effect.planned", string(RuntimeEffectPlanned))
	require.Equal(t, "runtime.effect.reconciled", string(RuntimeEffectReconciled))
	require.Equal(t, "runtime.effect.fenced", string(RuntimeEffectFenced))
	require.Equal(t, "runtime.operator.action", string(RuntimeOperatorAction))
}

// #849: 脱敏——payload 无 content/secret/metadata value/raw error 字段。
func TestRuntimeEffectData_CarriesNoSecrets(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(RuntimeEffectData{
		ExecutionID: "e", EffectID: "f", Status: "reconciled_succeeded", EvidenceRef: "ref",
	})
	require.NoError(t, err)
	for _, banned := range []string{"content", "secret", "metadata", "token", "tool_args"} {
		require.NotContains(t, string(raw), banned)
	}
	raw, err = json.Marshal(RuntimeOperatorActionData{ExecutionID: "e", EffectID: "f", Decision: "abandon", Status: "failed"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "content")
}
