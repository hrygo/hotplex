from common import write, replace, between, main


def tests():
    write('internal/gateway/deep_audit_997_test.go',r'''
package gateway

import (
    "context"
    "log/slog"
    "testing"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/hrygo/hotplex/pkg/events"
)

func deep997Flush(t *testing.T, envs ...*events.Envelope) []*events.Envelope {
    t.Helper()
    pc := &auditPlatformConn{writes:make(chan *events.Envelope,32)}
    e := newPCEntry(context.Background(),pc,pcEntryConfig{WriteBuffer:16,DropThreshold:15,CoalesceIntvl:time.Hour,CoalesceSize:1000,TerminalTimeout:time.Second},slog.Default())
    for _,env := range envs { require.NoError(t,e.EnqueueWrite(context.Background(),env,nil)) }
    require.NoError(t,e.Close())
    var got []*events.Envelope
    for len(pc.writes)>0 { got=append(got,<-pc.writes) }
    return got
}
func deep997Delta(id,text string,seq int64) *events.Envelope {
    return &events.Envelope{Version:events.Version,ID:"source",SessionID:"session",OwnerID:"owner",Seq:seq,Timestamp:seq,
        Metadata:map[string]any{"execution_id":"execution"},Event:events.Event{Type:events.MessageDelta,Data:events.MessageDeltaData{MessageID:id,Content:text}}}
}

func TestDeepAudit997CoalescingPreservesIdentity(t *testing.T) {
    t.Parallel()
    got := deep997Flush(t,deep997Delta("message","one",1),deep997Delta("message","two",2))
    require.Len(t,got,1)
    data := got[0].Event.Data.(events.MessageDeltaData)
    require.Equal(t,"message",data.MessageID)
    require.Equal(t,"onetwo",data.Content)
    require.Equal(t,"owner",got[0].OwnerID)
    require.Equal(t,map[string]any{"execution_id":"execution"},got[0].Metadata)
    require.EqualValues(t,2,got[0].Seq)
    require.EqualValues(t,2,got[0].Timestamp)
}

func TestDeepAudit997SemanticBoundariesNeverMerge(t *testing.T) {
    t.Parallel()
    for _,boundary := range []string{"message","session","owner","metadata"} {
        t.Run(boundary,func(t *testing.T){
            t.Parallel()
            first,second := deep997Delta("message","first",1),deep997Delta("message","second",2)
            switch boundary {
            case "message": second.Event.Data=events.MessageDeltaData{MessageID:"different",Content:"second"}
            case "session": second.SessionID="different"
            case "owner": second.OwnerID="different"
            case "metadata": second.Metadata["execution_id"]="different"
            }
            got := deep997Flush(t,first,second)
            require.Len(t,got,2,"distinct logical messages must not become one output")
            require.Equal(t,"first",extractDeltaContent(got[0]))
            require.Equal(t,"second",extractDeltaContent(got[1]))
        })
    }
}

func TestDeepAudit997MapPayloadAndOpaqueRawSurvive(t *testing.T) {
    t.Parallel()
    first := deep997Delta("message","first",1)
    first.Event.Data=map[string]any{"message_id":"message","content":"first","custom":"kept"}
    opaque := &events.Envelope{SessionID:"session",Event:events.Event{Type:events.Raw,Data:events.RawData{Raw:map[string]any{"status":"tool-running"}}}}
    got := deep997Flush(t,first,opaque)
    require.Len(t,got,2)
    require.Equal(t,first.Event.Data,got[0].Event.Data)
    require.Equal(t,opaque,got[1])
}
''')


def fixes():
    path='internal/gateway/platform_writer.go'
    replace(path,'\t"runtime/debug"','\t"runtime/debug"\n\t"reflect"')
    between(path,'func (e *pcEntry) writeLoop() {','func (e *pcEntry) writeOne(',r'''
func (e *pcEntry) writeLoop() {
    defer close(e.done)
    defer func() {
        if r:=recover();r!=nil { e.log.Error("pcEntry writeLoop panic","panic",r,"stack",string(debug.Stack())) }
    }()
    var text strings.Builder
    var pending *events.Envelope
    var messageID string
    var runeCount int
    var timer *time.Timer
    var timerCh <-chan time.Time
    defer func(){if timer!=nil{timer.Stop()}}()

    flush := func() {
        if pending == nil { return }
        merged := pending
        merged.ID = aep.NewID()
        merged.Version = events.Version
        merged.Event.Type = events.MessageDelta
        // Keep typed/map shape and extension fields. Sequence and timestamp
        // identify the last source delta represented by this aggregate.
        if data,ok := merged.Event.Data.(map[string]any);ok && pending.Event.Type != events.Raw {
            data["content"] = text.String()
        } else {
            merged.Event.Data = events.MessageDeltaData{MessageID:messageID,Content:text.String()}
        }
        pending = nil
        text.Reset()
        runeCount = 0
        if timer!=nil {timer.Stop();timerCh=nil}
        observability.GatewayDeltaFlush().Add(e.ctx,1)
        e.writeOne(platformWrite{env:merged,ctx:e.ctx})
    }
    consume := func(write platformWrite) {
        content,id,ok := platformDeltaContent(write.env)
        if !ok || content == "" {
            flush()
            e.writeOne(write)
            return
        }
        if pending != nil && (!samePlatformDeltaBoundary(pending,write.env) || id != messageID) { flush() }
        if pending == nil {
            pending = events.Clone(write.env)
            messageID = id
        }
        pending.Seq = write.env.Seq
        pending.Timestamp = write.env.Timestamp
        text.WriteString(content)
        runeCount += utf8.RuneCountInString(content)
        observability.GatewayDeltaCoalesced().Add(e.ctx,1)
        if runeCount >= e.cfg.CoalesceSize { flush(); return }
        if timer == nil {
            timer = time.NewTimer(e.cfg.CoalesceIntvl)
        } else {
            if !timer.Stop(){select{case <-timer.C:default:}}
            timer.Reset(e.cfg.CoalesceIntvl)
        }
        timerCh = timer.C
    }
    for {
        select {
        case write,ok := <-e.ch:
            if !ok {flush();return}
            consume(write)
        case <-timerCh:
            flush()
        case <-e.closeCh:
            // Close marks closed and signals closeCh before this lock. Senders
            // waiting for capacity can therefore finish without deadlock, and
            // no accepted write can arrive after the final empty observation.
            e.sendMu.Lock()
            defer e.sendMu.Unlock()
            for {
                select {
                case write := <-e.ch: consume(write)
                default: flush();return
                }
            }
        }
    }
}

// samePlatformDeltaBoundary excludes only content and aggregate position.
// A new owner/session/message/metadata context must start a separate output.
func samePlatformDeltaBoundary(a,b *events.Envelope) bool {
    if a.SessionID!=b.SessionID || a.OwnerID!=b.OwnerID || a.Event.Type!=b.Event.Type ||
        a.Priority!=b.Priority || !reflect.DeepEqual(a.Metadata,b.Metadata) {return false}
    am,aok := a.Event.Data.(map[string]any)
    bm,bok := b.Event.Data.(map[string]any)
    if aok != bok {return false}
    if aok {
        if len(am)!=len(bm){return false}
        for key,value := range am {
            if key!="content" && !reflect.DeepEqual(value,bm[key]){return false}
        }
    }
    return true
}

func platformDeltaContent(env *events.Envelope) (content,messageID string,ok bool) {
    if !isCoalesciblePlatformEvent(env.Event.Type){return "","",false}
    if env.Event.Type==events.MessageDelta {
        switch data:=env.Event.Data.(type) {
        case events.MessageDeltaData: return data.Content,data.MessageID,true
        case map[string]any:
            content,ok=data["content"].(string)
            messageID,_=data["message_id"].(string)
            return content,messageID,ok
        case string: return data,"",true
        }
        return "","",false
    }
    // Preserve unknown Raw payloads rather than reducing them to empty text.
    if data,yes:=env.Event.Data.(events.RawData);yes {
        if raw,yes:=data.Raw.(map[string]any);yes {
            content,ok=raw["text"].(string)
            return content,"",ok
        }
    }
    if text,yes:=env.Event.Data.(string);yes{return text,"",true}
    return "","",false
}
''')
    # The original event kind must be examined before rewriting it in flush.
    replace(path,'''        merged.Event.Type = events.MessageDelta
        // Keep typed/map shape''', '''        sourceKind := merged.Event.Type
        merged.Event.Type = events.MessageDelta
        // Keep typed/map shape''')
    replace(path,'ok && pending.Event.Type != events.Raw','ok && sourceKind != events.Raw')


if __name__=='__main__':main(tests,fixes)
