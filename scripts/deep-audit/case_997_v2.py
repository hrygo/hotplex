from common import replace,write,main
from case_997 import tests, fixes as original_fixes


def fixes():
    original_fixes()
    path='internal/gateway/platform_writer.go'
    replace(path,'ok && sourceKind != events.Raw {','ok && sourceKind != events.Raw && platformDeltaHasExtensions(data) {')
    replace(path,'''        for key,value := range am {
            if key!="content" && !reflect.DeepEqual(value,bm[key]){return false}
        }''','''        for key,value := range am {
            other,exists:=bm[key]
            if !exists || (key!="content" && !reflect.DeepEqual(value,other)){return false}
        }''')
    replace(path,'func platformDeltaContent(env *events.Envelope)', '''// Keep the established typed delta for the canonical two-field payload.
// Maps carrying extension fields retain those fields instead of narrowing.
func platformDeltaHasExtensions(data map[string]any) bool {
    for key:=range data {if key!="content" && key!="message_id"{return true}}
    return false
}

func platformDeltaContent(env *events.Envelope)''')
    write('internal/gateway/deep_audit_997_extensions_test.go',r'''
package gateway

import (
    "testing"
    "github.com/stretchr/testify/require"
    "github.com/hrygo/hotplex/pkg/events"
)

func TestDeepAudit997CanonicalMapsRemainTyped(t *testing.T){
    t.Parallel()
    e:=deep997Delta("message","x",1)
    e.Event.Data=map[string]any{"content":"x","message_id":"message"}
    got:=deep997Flush(t,e)
    require.Len(t,got,1)
    data,ok:=got[0].Event.Data.(events.MessageDeltaData)
    require.True(t,ok)
    require.Equal(t,"message",data.MessageID)
}

func TestDeepAudit997DifferentNilExtensionKeysDoNotMerge(t *testing.T){
    t.Parallel()
    a,b:=deep997Delta("message","a",1),deep997Delta("message","b",2)
    a.Event.Data=map[string]any{"content":"a","first":nil}
    b.Event.Data=map[string]any{"content":"b","second":nil}
    require.Len(t,deep997Flush(t,a,b),2)
}
''')


if __name__=='__main__':main(tests,fixes)
