package codexcli

import "io"

// setTransportWriter runs during process lifecycle changes. It does not wait
// for pipe I/O: a closing old pipe may still own writeMu until the OS wakes it.
func (m *CodexAppServerManager) setTransportWriter(writer io.WriteCloser) {
	m.stdinMu.Lock()
	m.stdin = writer
	m.stdinMu.Unlock()
}

func (m *CodexAppServerManager) transportWriter() io.WriteCloser {
	m.stdinMu.RLock()
	defer m.stdinMu.RUnlock()
	return m.stdin
}
