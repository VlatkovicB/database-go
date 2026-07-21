package storage

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const DefaultBufPoolSize = 64

type PageID struct {
	Table   string
	PageNum int
}

// BPStats tracks buffer pool access (safe for concurrent reads via atomic).
type BPStats struct {
	Hits    int64
	Misses  int64
	Evicted int64
}

func (s BPStats) Add(o BPStats) BPStats {
	return BPStats{Hits: s.Hits + o.Hits, Misses: s.Misses + o.Misses, Evicted: s.Evicted + o.Evicted}
}

// FormatExplain produces a PG-style buffer accounting line.
func (s BPStats) FormatExplain() string {
	if s.Hits+s.Misses == 0 {
		return ""
	}
	out := fmt.Sprintf("Buffers: shared hit=%d", s.Hits)
	if s.Misses > 0 {
		out += fmt.Sprintf(" read=%d", s.Misses)
	}
	if s.Evicted > 0 {
		out += fmt.Sprintf(" evicted=%d", s.Evicted)
	}
	return out
}

type bpSlot struct {
	id         PageID
	valid      bool
	pinCount   int
	usageCount int
	dirty      bool
}

// BufferPool is a fixed-size, clock-sweep buffer pool (simulates PostgreSQL's
// shared_buffers). Table.Pages remains the authoritative data store; this pool
// tracks access patterns and reports hit/miss for EXPLAIN ANALYZE.
type BufferPool struct {
	mu        sync.Mutex
	size      int
	slots     []bpSlot
	index     map[PageID]int
	clockHand int
	hits      int64
	misses    int64
	evicted   int64
}

func NewBufferPool(size int) *BufferPool {
	if size < 1 {
		size = DefaultBufPoolSize
	}
	return &BufferPool{
		size:  size,
		slots: make([]bpSlot, size),
		index: make(map[PageID]int),
	}
}

// FetchPage records access to a page and returns (slotIdx, hit).
// Call Unpin(slot, dirty) when done.
func (bp *BufferPool) FetchPage(id PageID) (int, bool) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if slot, ok := bp.index[id]; ok {
		bp.slots[slot].pinCount++
		bp.slots[slot].usageCount = 2
		atomic.AddInt64(&bp.hits, 1)
		return slot, true
	}

	slot := bp.evict()
	bp.slots[slot] = bpSlot{id: id, valid: true, pinCount: 1, usageCount: 1}
	bp.index[id] = slot
	atomic.AddInt64(&bp.misses, 1)
	return slot, false
}

func (bp *BufferPool) evict() int {
	for {
		s := &bp.slots[bp.clockHand]
		if !s.valid {
			v := bp.clockHand
			bp.clockHand = (bp.clockHand + 1) % bp.size
			return v
		}
		if s.pinCount > 0 {
			bp.clockHand = (bp.clockHand + 1) % bp.size
			continue
		}
		if s.usageCount > 0 {
			s.usageCount--
			bp.clockHand = (bp.clockHand + 1) % bp.size
			continue
		}
		delete(bp.index, s.id)
		atomic.AddInt64(&bp.evicted, 1)
		v := bp.clockHand
		bp.clockHand = (bp.clockHand + 1) % bp.size
		return v
	}
}

// Unpin decrements pin count. dirty=true marks for eviction accounting.
func (bp *BufferPool) Unpin(slot int, dirty bool) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if slot < 0 || slot >= bp.size || !bp.slots[slot].valid {
		return
	}
	if bp.slots[slot].pinCount > 0 {
		bp.slots[slot].pinCount--
	}
	if dirty {
		bp.slots[slot].dirty = true
	}
}

// InvalidatePage removes a page from the pool (call after DML writes to Table.Pages).
func (bp *BufferPool) InvalidatePage(id PageID) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if slot, ok := bp.index[id]; ok {
		bp.slots[slot] = bpSlot{}
		delete(bp.index, id)
	}
}

// SnapshotStats returns a point-in-time copy and resets counters for next query.
func (bp *BufferPool) SnapshotStats() BPStats {
	return BPStats{
		Hits:    atomic.SwapInt64(&bp.hits, 0),
		Misses:  atomic.SwapInt64(&bp.misses, 0),
		Evicted: atomic.SwapInt64(&bp.evicted, 0),
	}
}
