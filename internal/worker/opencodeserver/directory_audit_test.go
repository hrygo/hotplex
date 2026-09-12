package opencodeserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/pkg/events"
)

func TestAuditPromptPreservesWorkspaceDirectory(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"", "/tmp/workspace with spaces/项目&a=b"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			observed := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.URL.Query().Get("directory")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			c := &conn{sessionID: "session", projectDir: dir, httpAddr: server.URL, client: server.Client()}
			require.NoError(t, c.Send(context.Background(), &events.Envelope{Event: events.Event{
				Type: events.Input, Data: events.InputData{Content: "probe"},
			}}))
			require.Equal(t, dir, <-observed)
		})
	}
}
