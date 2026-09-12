package codexcli

import "io"

// setStdin is called with manager.mu held by native lifecycle paths. Its
// separate short state lock lets handshake writers snapshot without reentering
// manager.mu; neither state lock is held during pipe I/O.
func (m *CodexAppServerManager) setStdin(writer io.WriteCloser) {
	m.transportMu.Lock()
	defer m.transportMu.Unlock()
	m.stdin = writer
	m.transportVersion++
}
func (m *CodexAppServerManager) snapshotStdin() (io.WriteCloser, uint64) {
	m.transportMu.RLock()
	defer m.transportMu.RUnlock()
	return m.stdin, m.transportVersion
}
