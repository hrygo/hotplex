package events

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit996MapAndSliceCycles(t *testing.T) {
	t.Parallel()
	m := map[string]any{}
	m["self"] = m
	s := make([]any, 1)
	s[0] = s
	copied := Clone(&Envelope{Event: Event{Data: m}, Metadata: map[string]any{"slice": s}})
	cm := copied.Event.Data.(map[string]any)
	cm["self"].(map[string]any)["new"] = true
	require.Equal(t, true, cm["new"])
	require.NotContains(t, m, "new")
	cs := copied.Metadata["slice"].([]any)
	cs[0].([]any)[0] = "copied"
	require.Equal(t, "copied", cs[0])
	_, sourceStillCycle := s[0].([]any)
	require.True(t, sourceStillCycle)
}
