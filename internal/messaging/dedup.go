package messaging

import (
	"sync"
	"time"
)

// Dedup is a bounded TTL dedup map that prevents duplicate message processing.
// When maxEntries is exceeded, the oldest entries are evicted in FIFO order.
// Supports both self-cleanup (via StartCleanup/Close) and manual Sweep.
type Dedup struct {
	mu         sync.Mutex
	entries    map[string]dedupEntry
	order      []dedupOrderEntry // FIFO eviction order
	nextHandle uint64
	maxEntries int
	ttl        time.Duration
	done       chan struct{}
	closeOnce  sync.Once
	startOnce  sync.Once
}

type dedupEntry struct {
	recordedAt time.Time
	handle     uint64
}

type dedupOrderEntry struct {
	id     string
	handle uint64
}

// DedupHandle identifies one successful TryRecordWithHandle call. It can be
// used to roll back an admission failure without removing a later retry.
type DedupHandle struct {
	id     string
	handle uint64
}

// NewDedup creates a new bounded dedup map.
// If maxEntries <= 0, defaults to 5000. If ttl <= 0, defaults to 12h.
func NewDedup(maxEntries int, ttl time.Duration) *Dedup {
	if maxEntries <= 0 {
		maxEntries = 5000
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Dedup{
		entries:    make(map[string]dedupEntry),
		order:      make([]dedupOrderEntry, 0, maxEntries),
		maxEntries: maxEntries,
		ttl:        ttl,
		done:       make(chan struct{}),
	}
}

// StartCleanup launches a background goroutine that periodically sweeps expired entries.
// Call Close to stop the goroutine. The sweep interval is ttl/2.
func (d *Dedup) StartCleanup() {
	d.startOnce.Do(func() {
		select {
		case <-d.done:
			return
		default:
			go d.cleanupLoop()
		}
	})
}

// TryRecord records an id and returns true if it was not previously seen.
// Returns false if the id is a duplicate.
func (d *Dedup) TryRecord(id string) bool {
	_, accepted := d.TryRecordWithHandle(id)
	return accepted
}

// TryRecordWithHandle records an id and returns a handle for conditionally
// rolling back that exact registration if downstream admission fails.
func (d *Dedup) TryRecordWithHandle(id string) (*DedupHandle, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if entry, seen := d.entries[id]; seen {
		if now.Before(entry.recordedAt.Add(d.ttl)) {
			return nil, false
		}
		delete(d.entries, id)
		d.removeOrderEntryLocked(id, entry.handle)
	}

	for len(d.entries) >= d.maxEntries && len(d.order) > 0 {
		oldest := d.order[0]
		d.order = d.order[1:]
		if entry, ok := d.entries[oldest.id]; ok && entry.handle == oldest.handle {
			delete(d.entries, oldest.id)
		}
	}

	d.nextHandle++
	handle := d.nextHandle
	d.entries[id] = dedupEntry{recordedAt: now, handle: handle}
	d.order = append(d.order, dedupOrderEntry{id: id, handle: handle})
	return &DedupHandle{id: id, handle: handle}, true
}

// Rollback removes an entry only when it still matches the supplied handle.
// A delayed failure therefore cannot erase a later platform retry record.
func (d *Dedup) Rollback(handle *DedupHandle) {
	if handle == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.entries[handle.id]
	if !ok || entry.handle != handle.handle {
		return
	}
	delete(d.entries, handle.id)
	d.removeOrderEntryLocked(handle.id, handle.handle)
}

func (d *Dedup) removeOrderEntryLocked(id string, handle uint64) {
	writeIdx := 0
	for _, entry := range d.order {
		if entry.id == id && entry.handle == handle {
			continue
		}
		d.order[writeIdx] = entry
		writeIdx++
	}
	d.order = d.order[:writeIdx]
}

// Sweep removes expired entries. Can be called manually or from a background goroutine.
func (d *Dedup) Sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()

	cutoff := time.Now().Add(-d.ttl)
	writeIdx := 0
	for _, ordered := range d.order {
		entry, ok := d.entries[ordered.id]
		if ok && entry.handle == ordered.handle && entry.recordedAt.After(cutoff) {
			d.order[writeIdx] = ordered
			writeIdx++
		} else if ok && entry.handle == ordered.handle {
			delete(d.entries, ordered.id)
		}
	}
	d.order = d.order[:writeIdx]
}

// Len returns the number of tracked entries.
func (d *Dedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// Close stops the cleanup goroutine started by StartCleanup.
func (d *Dedup) Close() {
	d.closeOnce.Do(func() {
		if d.done != nil {
			close(d.done)
		}
	})
}

func (d *Dedup) cleanupLoop() {
	// Positive TTLs smaller than 2ns must not produce a zero ticker period.
	interval := max(d.ttl/2, time.Nanosecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.Sweep()
		}
	}
}
