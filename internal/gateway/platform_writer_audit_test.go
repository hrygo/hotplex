package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

type auditPlatformConn struct{ writes chan *events.Envelope }

func (c *auditPlatformConn) WriteCtx(ctx context.Context, env *events.Envelope) error {
	select {
	case c.writes <- env:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *auditPlatformConn) Close() error { return nil }

func auditWriter(t *testing.T) (*pcEntry, <-chan *events.Envelope) {
	t.Helper()
	pc := &auditPlatformConn{writes: make(chan *events.Envelope, 32)}
	e := newPCEntry(context.Background(), pc, pcEntryConfig{
		WriteBuffer: 16, DropThreshold: 15, CoalesceIntvl: 5 * time.Millisecond,
		CoalesceSize: 200, TerminalTimeout: time.Second,
	}, slog.Default())
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e, pc.writes
}

func auditReceive(t *testing.T, ch <-chan *events.Envelope) *events.Envelope {
	t.Helper()
	select {
	case env := <-ch:
		return env
	case <-time.After(time.Second):
		t.Fatal("accepted platform event did not flush within the test deadline")
		return nil
	}
}

func TestAuditPlatformTimerRearmsAfterFlush(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	for _, content := range []string{"first", "second", "third"} {
		env := &events.Envelope{SessionID: "session", Event: events.Event{
			Type: events.MessageDelta, Data: events.MessageDeltaData{Content: content},
		}}
		require.NoError(t, e.WriteCtx(context.Background(), env))
		require.Equal(t, content, extractDeltaContent(auditReceive(t, writes)))
	}
}

func TestAuditPlatformReasoningIsNotTextCoalesced(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	reasoning := &events.Envelope{SessionID: "session", Event: events.Event{
		Type: events.Reasoning, Data: map[string]any{"content": "reasoning delta"},
	}}
	done := &events.Envelope{SessionID: "session", Event: events.Event{Type: events.Done}}
	require.NoError(t, e.WriteCtx(context.Background(), reasoning))
	require.NoError(t, e.WriteCtx(context.Background(), done))
	got := auditReceive(t, writes)
	require.Equal(t, events.Reasoning, got.Event.Type, "droppable does not mean text-coalescible")
	require.Equal(t, reasoning.Event.Data, got.Event.Data)
	require.Equal(t, events.Done, auditReceive(t, writes).Event.Type)
}
