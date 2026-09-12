package codexcli

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/worker/base"
)

func TestD02ResetFailureReleasesOnlyOwnedReference(t *testing.T) {
	t.Parallel()
	w, s := documentedWorker(t)
	require.Equal(t, 2, w.manager.refs)
	s.setResult(true, true, `{}`)
	_, err := w.ResetContext(context.Background())
	require.Error(t, err)
	_ = w.Terminate(context.Background())
	_ = w.Terminate(context.Background())
	_ = w.Kill()
	require.Equal(t, 1, w.manager.refs, "failed reset must not leak or double release its Acquire")
}
func TestD02UnstartedWorkerDoesNotReleaseOtherSession(t *testing.T) {
	t.Parallel()
	m := NewCodexAppServerManager(slog.Default(), config.CodexCLIConfig{})
	m.refs = 1
	w := &AppServerWorker{BaseWorker: base.NewBaseWorker(nil, nil), manager: m}
	require.NoError(t, w.Terminate(context.Background()))
	require.Equal(t, 1, m.refs)
}
