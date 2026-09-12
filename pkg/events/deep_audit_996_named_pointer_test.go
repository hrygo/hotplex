package events

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit996NamedPointerTypeIsPreserved(t *testing.T) {
	t.Parallel()
	type payload struct{ Names []string }
	type reference *payload
	p := reference(&payload{Names: []string{"source"}})
	got := Clone(&Envelope{Event: Event{Data: p}})
	copied, ok := got.Event.Data.(reference)
	require.True(t, ok, "Clone must not erase a named pointer type")
	copied.Names[0] = "copy"
	require.Equal(t, "source", p.Names[0])
}
