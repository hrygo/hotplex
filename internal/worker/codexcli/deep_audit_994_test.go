package codexcli

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit994PendingCallsFinishWhenTransportEnds(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"shutdown", "stdout_eof"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m := auditRPCManager()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := m.Call(ctx, "pending/probe", nil); result <- err }()
				synctest.Wait()
				if source == "shutdown" {
					m.Shutdown(context.Background())
				} else {
					m.readNotifications(context.Background(), strings.NewReader(""))
				}
				synctest.Wait()
				timely := false
				var err error
				select {
				case err = <-result:
					timely = true
				default:
				}
				if !timely {
					cancel()
					<-result
				}
				require.True(t, timely, "known response-stream termination left Call waiting for its timeout")
				require.ErrorIs(t, err, io.ErrClosedPipe)
				count := 0
				m.pending.Range(func(_, _ any) bool { count++; return true })
				require.Zero(t, count)
			})
		})
	}
}
