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

func auditDelta(t *testing.T, sid, owner, mid, content string, meta map[string]any) *events.Envelope {
	t.Helper()
	return &events.Envelope{SessionID: sid, OwnerID: owner, Metadata: meta, Event: events.Event{
		Type: events.MessageDelta, Data: events.MessageDeltaData{MessageID: mid, Content: content},
	}}
}

func auditDeltaData(t *testing.T, env *events.Envelope) events.MessageDeltaData {
	t.Helper()
	d, ok := env.Event.Data.(events.MessageDeltaData)
	require.True(t, ok, "merged envelope keeps typed MessageDeltaData, got %T", env.Event.Data)
	return d
}

// #997: 同一消息可合并，且保留 message_id、OwnerID、metadata。
func TestAuditPlatformSameMessageMergesWithIdentity(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	ctx := context.Background()
	meta := map[string]any{"k": "v"}
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "hello ", meta)))
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "world", meta)))
	got := auditReceive(t, writes)
	d := auditDeltaData(t, got)
	require.Equal(t, "hello world", d.Content)
	require.Equal(t, "m1", d.MessageID)
	require.Equal(t, "u", got.OwnerID)
	require.Equal(t, meta, got.Metadata)
}

// #997: message_id 变化先 flush，不拼接不同逻辑消息。
func TestAuditPlatformMessageIDChangeFlushes(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	ctx := context.Background()
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "hello ", nil)))
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m2", "world", nil)))
	d1 := auditDeltaData(t, auditReceive(t, writes))
	require.Equal(t, "hello ", d1.Content)
	require.Equal(t, "m1", d1.MessageID)
	d2 := auditDeltaData(t, auditReceive(t, writes))
	require.Equal(t, "world", d2.Content)
	require.Equal(t, "m2", d2.MessageID)
}

// #997: owner 或 metadata 变化同样是边界。
func TestAuditPlatformOwnerOrMetadataChangeFlushes(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	ctx := context.Background()
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u1", "m1", "a", nil)))
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u2", "m1", "b", nil)))
	require.Equal(t, "a", auditDeltaData(t, auditReceive(t, writes)).Content)
	require.Equal(t, "b", auditDeltaData(t, auditReceive(t, writes)).Content)

	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "c", map[string]any{"k": "1"})))
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "d", map[string]any{"k": "2"})))
	require.Equal(t, "c", auditDeltaData(t, auditReceive(t, writes)).Content)
	require.Equal(t, "d", auditDeltaData(t, auditReceive(t, writes)).Content)
}

// #997: 非文本 Raw 不吞载荷，原样转发。
func TestAuditPlatformUnrecognizedRawPassesThrough(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	ctx := context.Background()
	raw := &events.Envelope{SessionID: "s", OwnerID: "u", Event: events.Event{
		Type: events.Raw, Data: events.RawData{Kind: "blob", Raw: map[string]any{"n": 1}},
	}}
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "hi ", nil)))
	require.NoError(t, e.WriteCtx(ctx, raw))
	require.Equal(t, "hi ", auditDeltaData(t, auditReceive(t, writes)).Content)
	got := auditReceive(t, writes)
	require.Equal(t, events.Raw, got.Event.Type)
	require.Equal(t, raw.Event.Data, got.Event.Data)
}

// #997: map 形态 delta 走同一边界规则。
func TestAuditPlatformMapDeltaRespectsBoundary(t *testing.T) {
	t.Parallel()
	e, writes := auditWriter(t)
	ctx := context.Background()
	mk := func(mid, content string) *events.Envelope {
		return &events.Envelope{SessionID: "s", OwnerID: "u", Event: events.Event{
			Type: events.MessageDelta, Data: map[string]any{"message_id": mid, "content": content},
		}}
	}
	require.NoError(t, e.WriteCtx(ctx, mk("m1", "x")))
	require.NoError(t, e.WriteCtx(ctx, mk("m2", "y")))
	require.Equal(t, "x", extractDeltaContent(auditReceive(t, writes)))
	require.Equal(t, "y", extractDeltaContent(auditReceive(t, writes)))
}

// #997: 关闭排空与常规路径用同一规则。
func TestAuditPlatformCloseDrainRespectsBoundary(t *testing.T) {
	t.Parallel()
	pc := &auditPlatformConn{writes: make(chan *events.Envelope, 32)}
	e := newPCEntry(context.Background(), pc, pcEntryConfig{
		WriteBuffer: 16, DropThreshold: 15, CoalesceIntvl: time.Hour,
		CoalesceSize: 1 << 20, TerminalTimeout: time.Second,
	}, slog.Default())
	ctx := context.Background()
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m1", "p", nil)))
	require.NoError(t, e.WriteCtx(ctx, auditDelta(t, "s", "u", "m2", "q", nil)))
	require.NoError(t, e.Close())
	require.Equal(t, "p", auditDeltaData(t, auditReceive(t, pc.writes)).Content)
	require.Equal(t, "q", auditDeltaData(t, auditReceive(t, pc.writes)).Content)
}
