package codexcli

import (
	"encoding/json"
	"fmt"
	"io"
)

// codexTransport is a process-generation snapshot. Its signal is never shared
// with a replacement. Only transportMu-protected lifecycle code may close it.
type codexTransport struct {
	writer  io.WriteCloser
	version uint64
	done    chan struct{}
}

func (m *CodexAppServerManager) setStdin(writer io.WriteCloser) {
	m.transportMu.Lock()
	defer m.transportMu.Unlock()
	if m.transportDone != nil {
		select {
		case <-m.transportDone:
		default:
			close(m.transportDone)
		}
	}
	m.stdin = writer
	m.transportVersion++
	m.transportDone = make(chan struct{})
	if writer == nil {
		close(m.transportDone)
	}
}

func (m *CodexAppServerManager) snapshotTransport() codexTransport {
	m.transportMu.Lock()
	defer m.transportMu.Unlock()
	if m.transportDone == nil {
		m.transportDone = make(chan struct{})
	}
	return codexTransport{writer: m.stdin, version: m.transportVersion, done: m.transportDone}
}

func (m *CodexAppServerManager) closeTransport(transport codexTransport) {
	m.transportMu.Lock()
	defer m.transportMu.Unlock()
	if m.transportVersion != transport.version || m.transportDone != transport.done {
		return
	}
	select {
	case <-transport.done:
	default:
		close(transport.done)
	}
}

func decodeCallResponse(method string, resp *JSONRPCResponse) (json.RawMessage, error) {
	if resp.Error != nil {
		return nil, fmt.Errorf("codex-app-server: %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
	}
	return resp.Result, nil
}
