package codexcli

import "sync"

// rpcGeneration owns completion of one process's response stream. Old reader
// cleanup only closes its captured generation, never a replacement's calls.
type rpcGeneration struct {
	once sync.Once
	done chan struct{}
}

func (g *rpcGeneration) finish() { g.once.Do(func() { close(g.done) }) }
func (m *CodexAppServerManager) currentRPCGeneration() *rpcGeneration {
	m.rpcMu.Lock()
	defer m.rpcMu.Unlock()
	if m.rpcRun == nil {
		m.rpcRun = &rpcGeneration{done: make(chan struct{})}
	}
	return m.rpcRun
}
func (m *CodexAppServerManager) beginRPCGeneration() *rpcGeneration {
	m.rpcMu.Lock()
	defer m.rpcMu.Unlock()
	if m.rpcRun != nil {
		m.rpcRun.finish()
	}
	m.rpcRun = &rpcGeneration{done: make(chan struct{})}
	return m.rpcRun
}
