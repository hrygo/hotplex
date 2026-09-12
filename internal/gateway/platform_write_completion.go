package gateway

import (
	"context"
	"sync"
)

// platformWriteCompletion arbitrates local write, timeout and shutdown results.
// This is a local one-shot receipt, not an external delivery exactly-once claim.
type platformWriteCompletion struct {
	once   sync.Once
	result chan<- error
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *platformWriteCompletion) finish(err error) {
	c.once.Do(func() {
		c.cancel()
		close(c.done)
		if c.result != nil {
			select {
			case c.result <- err:
			default:
			}
		}
	})
}

func (c *platformWriteCompletion) abandon() {
	c.once.Do(func() {
		c.cancel()
		close(c.done)
	})
}
