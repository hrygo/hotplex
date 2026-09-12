package events

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit996TypedPayloadIsIndependent(t *testing.T) {
	t.Parallel()
	original := &Envelope{OwnerID: "owner", Seq: 9007199254740993, Event: Event{Type: Input, Data: InputData{
		Content: "request", Metadata: map[string]any{"tags": []string{"original"}, "large": int64(9007199254740993)},
	}}}
	snapshot := Clone(original)
	require.Equal(t, original, snapshot)
	data := snapshot.Event.Data.(InputData)
	data.Metadata["tags"].([]string)[0] = "clone"
	require.Equal(t, "original", original.Event.Data.(InputData).Metadata["tags"].([]string)[0])
	original.Event.Data.(InputData).Metadata["large"] = int64(1)
	require.Equal(t, int64(9007199254740993), data.Metadata["large"])
	require.Equal(t, "owner", snapshot.OwnerID)
	require.EqualValues(t, 9007199254740993, snapshot.Seq)
}

func TestDeepAudit996TypedCollectionsPreserveTypes(t *testing.T) {
	t.Parallel()
	original := &Envelope{Event: Event{Type: Raw, Data: RawData{Raw: map[string]any{"bytes": []byte{1, 2, 3}}}}, Metadata: map[string]any{
		"names": []string{"source"}, "typed": map[string][]int{"key": {1, 2}}, "nil": ([]string)(nil),
	}}
	before, err := json.Marshal(original)
	require.NoError(t, err)
	snapshot := Clone(original)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(encoded))
	snapshot.Event.Data.(RawData).Raw.(map[string]any)["bytes"].([]byte)[0] = 9
	snapshot.Metadata["names"].([]string)[0] = "copy"
	snapshot.Metadata["typed"].(map[string][]int)["key"][0] = 9
	require.Equal(t, byte(1), original.Event.Data.(RawData).Raw.(map[string]any)["bytes"].([]byte)[0])
	require.Equal(t, "source", original.Metadata["names"].([]string)[0])
	require.Equal(t, 1, original.Metadata["typed"].(map[string][]int)["key"][0])
	require.Nil(t, snapshot.Metadata["nil"].([]string))
}

func TestDeepAudit996PointerGraphAndConcurrentOwnership(t *testing.T) {
	t.Parallel()
	type node struct {
		Values []int
		Next   *node
	}
	head := &node{Values: []int{7}}
	head.Next = head
	original := &Envelope{Event: Event{Data: head}}
	snapshot := Clone(original)
	copied := snapshot.Event.Data.(*node)
	require.NotSame(t, head, copied)
	require.Same(t, copied, copied.Next)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			head.Values[0] = i
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			copied.Values[0] = i + 1
		}
	}()
	wg.Wait()
	require.Equal(t, 999, head.Values[0])
	require.Equal(t, 1000, copied.Values[0])
}

func BenchmarkDeepAudit996CloneDelta(b *testing.B) {
	e := &Envelope{Event: Event{Type: MessageDelta, Data: MessageDeltaData{MessageID: "message", Content: "delta"}}}
	b.ReportAllocs()
	for b.Loop() {
		_ = Clone(e)
	}
}
