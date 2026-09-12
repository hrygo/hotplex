package codexcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestDeepAudit994OldReaderCannotCancelNewGeneration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := auditRPCManager()
		old := m.currentRPCGeneration()
		current := m.beginRPCGeneration()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.Call(ctx, "new/probe", nil); result <- err }()
		synctest.Wait()
		m.readNotificationsForGeneration(context.Background(), strings.NewReader(""), old)
		select {
		case <-current.done:
			t.Fatal("old reader closed the new response lifetime")
		default:
		}
		m.pending.Range(func(_, value any) bool {
			value.(chan *JSONRPCResponse) <- &JSONRPCResponse{Result: json.RawMessage(`{"ok":true}`)}
			return true
		})
		require.NoError(t, <-result)
		m.Shutdown(context.Background())
	})
}
