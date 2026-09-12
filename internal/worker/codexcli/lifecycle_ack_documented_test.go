package codexcli

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
)

func TestD09ThreadRequiresNativeIdentity(t *testing.T) {
	for _, response := range []string{`{}`, `null`, `{"thread":{"id":"  "}}`, `{"thread":{"id":9}}`} {
		t.Run(response, func(t *testing.T) {
			t.Parallel()
			m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{CallTimeout: time.Hour, IdleDrainPeriod: time.Hour})
			m.stdin = &documentedSink{m: m, reply: true, result: json.RawMessage(response)}
			m.refs = 1
			m.state = stateRunning
			w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}
			defer w.Terminate(context.Background())
			err := w.Start(context.Background(), worker.SessionInfo{SessionID: "session", UserID: "user"})
			require.Error(t, err, "an invalid native thread acknowledgement is not a successful Start")
			require.Empty(t, m.subscribers)
			require.Empty(t, w.threadID)
			require.Equal(t, 1, m.refs)
		})
	}
}

func TestD09TurnRejectsInvalidAckWithoutReusingPreviousID(t *testing.T) {
	for _, response := range []string{`{}`, `null`, `{"turn":{"id":"  "}}`, `{"turn":{"id":9}}`} {
		t.Run(response, func(t *testing.T) {
			t.Parallel()
			w, s := documentedWorker(t)
			defer w.Terminate(context.Background())
			w.turnID = "previous-turn"
			s.setResult(true, false, response)
			err := w.startTurn(context.Background(), []TurnInputItem{{Type: "text", Text: "probe"}})
			require.Error(t, err)
			var workerErr *worker.WorkerError
			require.ErrorAs(t, err, &workerErr)
			require.Equal(t, worker.ErrKindTimeout, workerErr.Kind, "written request with invalid ACK is an unknown outcome, not safe to replay")
			require.Empty(t, w.turnID, "invalid ACK cannot leave the previous turn as stop target")
		})
	}
}
