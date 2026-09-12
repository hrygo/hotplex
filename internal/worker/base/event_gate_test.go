package base

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestEventGateCloseUnblocksAndPreservesBufferedEvents(t *testing.T) {
	t.Parallel()
	var gate EventGate
	ch := make(chan *events.Envelope, 1)
	first := &events.Envelope{ID: "accepted"}
	require.True(t, gate.TrySend(ch, first))
	result := make(chan bool, 1)
	go func() { result <- gate.SendTimeout(ch, &events.Envelope{}, time.Minute) }()
	gate.Close(ch)
	select {
	case sent := <-result:
		require.False(t, sent)
	case <-time.After(time.Second):
		t.Fatal("blocked sender outlived Close")
	}
	require.Same(t, first, <-ch)
	_, open := <-ch
	require.False(t, open)
	require.False(t, gate.TrySend(ch, first))
	require.NotPanics(t, func() { gate.Close(ch) })
}

func TestEventGateConcurrentSendClose(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		var gate EventGate
		ch := make(chan *events.Envelope, 8)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 16; j++ {
				gate.TrySend(ch, &events.Envelope{})
			}
		}()
		go func() { defer wg.Done(); <-start; gate.Close(ch) }()
		close(start)
		wg.Wait()
	}
}
