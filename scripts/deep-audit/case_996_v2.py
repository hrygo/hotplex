from common import replace,write,main
from case_996 import tests as original_tests, fixes as original_fixes


def tests():
    original_tests()
    write('pkg/events/deep_audit_996_named_pointer_test.go',r'''
package events

import (
    "testing"
    "github.com/stretchr/testify/require"
)

func TestDeepAudit996NamedPointerTypeIsPreserved(t *testing.T){
    t.Parallel()
    type payload struct { Names []string }
    type reference *payload
    p:=reference(&payload{Names:[]string{"source"}})
    got:=Clone(&Envelope{Event:Event{Data:p}})
    copied,ok:=got.Event.Data.(reference)
    require.True(t,ok,"Clone must not erase a named pointer type")
    copied.Names[0]="copy"
    require.Equal(t,"source",p.Names[0])
}
''')


def fixes():
    original_fixes()
    replace('pkg/events/events.go','    return deepCopyValue(src).(map[string]any)','''    copied,ok:=deepCopyValue(src).(map[string]any)
    if !ok {panic("events: clone changed map payload type")}
    return copied''')
    replace('pkg/events/clone_payload.go','''        out := reflect.New(v.Type().Elem())
        c.seen[key] = out''','''        out := reflect.New(v.Type().Elem())
        if out.Type()!=v.Type(){out=out.Convert(v.Type())}
        c.seen[key] = out''')


if __name__=='__main__':main(tests,fixes)
