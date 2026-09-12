from common import write, replace, between, main


def tests():
    write('pkg/events/deep_audit_996_test.go',r'''
package events

import (
    "encoding/json"
    "sync"
    "testing"

    "github.com/stretchr/testify/require"
)

func TestDeepAudit996TypedPayloadIsIndependent(t *testing.T) {
    t.Parallel()
    original := &Envelope{OwnerID:"owner",Seq:9007199254740993,Event:Event{Type:Input,Data:InputData{
        Content:"request",Metadata:map[string]any{"tags":[]string{"original"},"large":int64(9007199254740993)},
    }}}
    snapshot := Clone(original)
    require.Equal(t,original,snapshot)
    data := snapshot.Event.Data.(InputData)
    data.Metadata["tags"].([]string)[0] = "clone"
    require.Equal(t,"original",original.Event.Data.(InputData).Metadata["tags"].([]string)[0])
    original.Event.Data.(InputData).Metadata["large"] = int64(1)
    require.Equal(t,int64(9007199254740993),data.Metadata["large"])
    require.Equal(t,"owner",snapshot.OwnerID)
    require.EqualValues(t,9007199254740993,snapshot.Seq)
}

func TestDeepAudit996TypedCollectionsPreserveTypes(t *testing.T) {
    t.Parallel()
    original := &Envelope{Event:Event{Type:Raw,Data:RawData{Raw:map[string]any{"bytes":[]byte{1,2,3}}}},Metadata:map[string]any{
        "names":[]string{"source"},"typed":map[string][]int{"key":{1,2}},"nil":([]string)(nil),
    }}
    before,err := json.Marshal(original); require.NoError(t,err)
    snapshot := Clone(original)
    encoded,err := json.Marshal(snapshot); require.NoError(t,err)
    require.JSONEq(t,string(before),string(encoded))
    snapshot.Event.Data.(RawData).Raw.(map[string]any)["bytes"].([]byte)[0] = 9
    snapshot.Metadata["names"].([]string)[0] = "copy"
    snapshot.Metadata["typed"].(map[string][]int)["key"][0] = 9
    require.Equal(t,byte(1),original.Event.Data.(RawData).Raw.(map[string]any)["bytes"].([]byte)[0])
    require.Equal(t,"source",original.Metadata["names"].([]string)[0])
    require.Equal(t,1,original.Metadata["typed"].(map[string][]int)["key"][0])
    require.Nil(t,snapshot.Metadata["nil"].([]string))
}

func TestDeepAudit996PointerGraphAndConcurrentOwnership(t *testing.T) {
    t.Parallel()
    type node struct { Values []int; Next *node }
    head := &node{Values:[]int{7}}
    head.Next = head
    original := &Envelope{Event:Event{Data:head}}
    snapshot := Clone(original)
    copied := snapshot.Event.Data.(*node)
    require.NotSame(t,head,copied)
    require.Same(t,copied,copied.Next)
    var wg sync.WaitGroup
    wg.Add(2)
    go func(){defer wg.Done();for i:=0;i<1000;i++{head.Values[0]=i}}()
    go func(){defer wg.Done();for i:=0;i<1000;i++{copied.Values[0]=i+1}}()
    wg.Wait()
    require.Equal(t,999,head.Values[0])
    require.Equal(t,1000,copied.Values[0])
}

func BenchmarkDeepAudit996CloneDelta(b *testing.B) {
    e := &Envelope{Event:Event{Type:MessageDelta,Data:MessageDeltaData{MessageID:"message",Content:"delta"}}}
    b.ReportAllocs()
    for b.Loop(){_ = Clone(e)}
}
''')


def fixes():
    path='pkg/events/events.go'
    replace(path,'''\tif m, ok := env.Event.Data.(map[string]any); ok && m != nil {
\t\tc.Event.Data = deepCopyMap(m)
\t}''','\tc.Event.Data = deepCopyValue(env.Event.Data)')
    between(path,'func deepCopyMap(src map[string]any) map[string]any {','// CloneDeep returns',r'''
func deepCopyMap(src map[string]any) map[string]any {
    if src == nil { return nil }
    return deepCopyValue(src).(map[string]any)
}

// deepCopyValue preserves concrete Go payload types and mutable exported data.
// Functions, channels and opaque unexported state are not wire payloads and
// retain their original identity; callers must treat those values as immutable.
func deepCopyValue(v any) any {
    switch v.(type) {
    case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
        float32, float64, MessageDeltaData, MessageEndData, StateData, ErrorData, InputAckData:
        return v
    default:
        return clonePayloadGraph(v)
    }
}
''')
    replace(path,'''// map[string]any Event.Data is recursively deep-copied so that nested
// mutable reference types (maps, slices) are never shared.''','''// Exported payload data, including typed structs, maps, slices and pointers,
// is recursively copied without a JSON round-trip. Cyclic reference graphs
// are supported. Opaque unexported state, functions and channels are not wire
// payload data and must be treated as immutable by their owners.''')
    write('pkg/events/clone_payload.go',r'''
package events

import "reflect"

// A graph memo prevents cycles and preserves repeated references within each
// copied payload. Slice length is part of its view identity; capacity is not a
// wire property and is limited to length in the independently owned copy.
type payloadVisit struct {
    typ reflect.Type
    ptr uintptr
    length int
}

type payloadCopier struct { seen map[payloadVisit]reflect.Value }

func clonePayloadGraph(value any) any {
    if value == nil { return nil }
    c := payloadCopier{seen:make(map[payloadVisit]reflect.Value)}
    return c.copy(reflect.ValueOf(value)).Interface()
}

func (c *payloadCopier) copy(v reflect.Value) reflect.Value {
    switch v.Kind() {
    case reflect.Interface:
        if v.IsNil() { return reflect.Zero(v.Type()) }
        out := reflect.New(v.Type()).Elem()
        out.Set(c.copy(v.Elem()))
        return out
    case reflect.Pointer:
        if v.IsNil() { return reflect.Zero(v.Type()) }
        key := payloadVisit{typ:v.Type(),ptr:v.Pointer()}
        if found,ok := c.seen[key];ok{return found}
        out := reflect.New(v.Type().Elem())
        c.seen[key] = out
        out.Elem().Set(c.copy(v.Elem()))
        return out
    case reflect.Map:
        if v.IsNil() { return reflect.Zero(v.Type()) }
        key := payloadVisit{typ:v.Type(),ptr:uintptr(v.UnsafePointer())}
        if found,ok := c.seen[key];ok{return found}
        out := reflect.MakeMapWithSize(v.Type(),v.Len())
        c.seen[key] = out
        it := v.MapRange()
        for it.Next(){out.SetMapIndex(c.copy(it.Key()),c.copy(it.Value()))}
        return out
    case reflect.Slice:
        if v.IsNil() { return reflect.Zero(v.Type()) }
        key := payloadVisit{typ:v.Type(),ptr:v.Pointer(),length:v.Len()}
        if found,ok := c.seen[key];ok{return found}
        out := reflect.MakeSlice(v.Type(),v.Len(),v.Len())
        c.seen[key] = out
        for i:=0;i<v.Len();i++{out.Index(i).Set(c.copy(v.Index(i)))}
        return out
    case reflect.Array:
        out := reflect.New(v.Type()).Elem()
        for i:=0;i<v.Len();i++{out.Index(i).Set(c.copy(v.Index(i)))}
        return out
    case reflect.Struct:
        out := reflect.New(v.Type()).Elem()
        out.Set(v)
        for i:=0;i<v.NumField();i++{
            if out.Field(i).CanSet(){out.Field(i).Set(c.copy(v.Field(i)))}
        }
        return out
    default:
        return v
    }
}
''')
    write('pkg/events/clone_payload_cycle_test.go',r'''
package events

import (
    "testing"
    "github.com/stretchr/testify/require"
)

func TestDeepAudit996MapAndSliceCycles(t *testing.T) {
    t.Parallel()
    m := map[string]any{}
    m["self"] = m
    s := make([]any,1)
    s[0] = s
    copied := Clone(&Envelope{Event:Event{Data:m},Metadata:map[string]any{"slice":s}})
    cm := copied.Event.Data.(map[string]any)
    cm["self"].(map[string]any)["new"] = true
    require.Equal(t,true,cm["new"])
    require.NotContains(t,m,"new")
    cs := copied.Metadata["slice"].([]any)
    cs[0].([]any)[0] = "copied"
    require.Equal(t,"copied",cs[0])
    _,sourceStillCycle := s[0].([]any)
    require.True(t,sourceStillCycle)
}
''')


if __name__ == '__main__': main(tests,fixes)
