package codexcli

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
)

func TestD07PendingWakesOnTransportLoss(t *testing.T) {
	for _, cause := range []string{"shutdown", "eof"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{CallTimeout: time.Hour})
				m.stdin = &documentedSink{m: m}
				m.state = stateRunning
				result := make(chan error, 1)
				go func() { _, err := m.Call(context.Background(), "probe", nil); result <- err }()
				synctest.Wait()
				if cause == "shutdown" {
					m.Shutdown(context.Background())
				} else {
					m.readNotifications(context.Background(), strings.NewReader(""))
				}
				synctest.Wait()
				select {
				case err := <-result:
					require.ErrorIs(t, err, io.ErrClosedPipe)
				default:
					time.Sleep(2 * time.Hour) // virtual time drains baseline before failing
					<-result
					t.Fatal("pending Call ignored transport loss until CallTimeout")
				}
				pending := 0
				m.pending.Range(func(_, _ any) bool { pending++; return true })
				require.Zero(t, pending)
			})
		})
	}
}
