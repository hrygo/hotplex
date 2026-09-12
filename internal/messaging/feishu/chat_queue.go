package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// chatTaskTimeout bounds the maximum execution time of a single chat task.
// Prevents goroutine leaks from tasks blocked on external APIs indefinitely.
const chatTaskTimeout = 10 * time.Minute

// chatIdleTimeout is how long a worker goroutine waits for new tasks before
// self-cleaning. Prevents goroutine leaks from one-off chats.
const chatIdleTimeout = 5 * time.Minute

// ErrChatQueueClosed is returned when a task is submitted after shutdown begins.
var ErrChatQueueClosed = errors.New("feishu: chat queue closed")

// ChatQueue serializes message sends per chatID to prevent reordering.
// Each chatID gets a dedicated goroutine that processes tasks sequentially
// through a buffered channel, eliminating race conditions that existed in the
// previous goroutine-chaining approach.
type ChatQueue struct {
	log       *slog.Logger
	mu        sync.Mutex
	workers   map[string]*chatWorker
	closed    bool
	wg        sync.WaitGroup
	lifecycle context.Context
	cancel    context.CancelFunc
	closeDone chan struct{}
	closeWait sync.Once
}

type chatWorker struct {
	tasks  chan func(ctx context.Context) error
	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewChatQueue(log *slog.Logger) *ChatQueue {
	lifecycle, cancel := context.WithCancel(context.Background())
	return &ChatQueue{
		lifecycle: lifecycle, cancel: cancel, closeDone: make(chan struct{}),
		log:     log,
		workers: make(map[string]*chatWorker),
	}
}

// Enqueue submits a task for serial execution on the given chatID.
// If no worker exists for the chatID, one is created.
// Returns an error if the per-chatID task channel is full.
func (q *ChatQueue) Enqueue(chatID string, task func(ctx context.Context) error) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrChatQueueClosed
	}

	w, exists := q.workers[chatID]
	if !exists {
		w = &chatWorker{
			tasks: make(chan func(ctx context.Context) error, 64),
		}
		q.workers[chatID] = w
		q.wg.Add(1)
		go q.runWorker(chatID, w)
	}

	select {
	case w.tasks <- task:
		q.mu.Unlock()
		return nil
	default:
		q.mu.Unlock()
		return fmt.Errorf("feishu: chat queue full for %s", chatID)
	}
}

// runWorker processes tasks from the channel sequentially for a single chatID.
// It exits when: (1) the channel is closed (via Close), or (2) idle timeout
// elapses with no new tasks. On exit, it removes itself from the workers map.
func (q *ChatQueue) runWorker(chatID string, w *chatWorker) {
	defer q.wg.Done()
	defer q.removeWorkerIfCurrent(chatID, w)
	defer func() {
		if r := recover(); r != nil {
			if q.log != nil {
				q.log.Error("feishu: panic in chat queue worker", "chat_id", chatID, "panic", r, "stack", string(debug.Stack()))
			}
		}
	}()

	idleTimer := time.NewTimer(chatIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case task, ok := <-w.tasks:
			if !ok {
				return
			}
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(chatIdleTimeout)

			q.runTask(chatID, w, task)

		case <-idleTimer.C:
			if q.tryRemoveIdleWorker(chatID, w) {
				return
			}
			idleTimer.Reset(chatIdleTimeout)
		}
	}
}

// runTask contains failures to one accepted task. Recovering only in
// runWorker would retire the worker and strand the rest of its accepted queue.
func (q *ChatQueue) runTask(chatID string, w *chatWorker, task func(context.Context) error) {
	if q.lifecycle.Err() != nil {
		if q.log != nil {
			q.log.Warn("feishu: queued task discarded after shutdown deadline", "chat_id", chatID)
		}
		return
	}
	ctx, cancel := context.WithTimeout(q.lifecycle, chatTaskTimeout)
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
		if r := recover(); r != nil && q.log != nil {
			q.log.Error("feishu: panic in chat queue task", "chat_id", chatID, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := task(ctx); err != nil && q.log != nil {
		if ctx.Err() != nil {
			q.log.Warn("feishu: chat queue task timed out", "chat_id", chatID, "err", err)
		} else {
			q.log.Warn("feishu: chat queue task error", "chat_id", chatID, "err", err)
		}
	}
}

// tryRemoveIdleWorker retires w only when it is still current and no accepted
// task is waiting. Enqueue holds q.mu through its non-blocking channel send,
// so an accepted task cannot be stranded by the idle boundary.
func (q *ChatQueue) tryRemoveIdleWorker(chatID string, w *chatWorker) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.workers[chatID] != w || len(w.tasks) != 0 {
		return false
	}
	delete(q.workers, chatID)
	return true
}

func (q *ChatQueue) removeWorkerIfCurrent(chatID string, w *chatWorker) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.workers[chatID] == w {
		delete(q.workers, chatID)
	}
}

// Abort cancels the currently running task for the given chatID.
func (q *ChatQueue) Abort(chatID string) {
	q.mu.Lock()
	w, exists := q.workers[chatID]
	q.mu.Unlock()

	if !exists {
		return
	}

	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()

	if q.log != nil {
		q.log.Debug("feishu: aborted task for chat", "chat_id", chatID)
	}
}

// Close shuts down all worker goroutines by closing their task channels.
// It waits for all in-flight tasks to complete.
func (q *ChatQueue) Close() {
	_ = q.CloseContext(context.Background())
}

// CloseContext drains within the caller budget, then cancels active tasks and
// prevents queued tasks from starting. Uncooperative tasks may outlive the call;
// one queue-owned waiter is retained, never a new waiter per caller.
func (q *ChatQueue) CloseContext(ctx context.Context) error {
	q.mu.Lock()
	if q.closeDone == nil {
		q.closeDone = make(chan struct{})
	}
	if q.cancel == nil {
		q.lifecycle, q.cancel = context.WithCancel(context.Background())
	}
	if !q.closed {
		q.closed = true
		for _, w := range q.workers {
			close(w.tasks)
		}
	}
	done := q.closeDone
	cancel := q.cancel
	q.closeWait.Do(func() { go func() { q.wg.Wait(); cancel(); close(done) }() })
	q.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}
