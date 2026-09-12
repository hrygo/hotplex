from common import replace, main
from case_1002 import tests, fixes as original_fixes


def fixes():
    original_fixes()
    # Register now captures its immutable lifetime before spawning the watcher.
    # No remaining caller uses the old private wrapper; retain all lint rules.
    replace('internal/messaging/interaction.go', '''func (m *InteractionManager) watchTimeout(pi *PendingInteraction) {
    m.mu.RLock()
    cancelCh, timeout := pi.cancelCh, pi.Timeout
    m.mu.RUnlock()
    m.watchRegistrationTimeout(pi, cancelCh, timeout)
}

''', '')


if __name__=='__main__':main(tests,fixes)
