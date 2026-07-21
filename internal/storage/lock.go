package storage

import (
	"errors"
	"sync"
)

var ErrDeadlock = errors.New("deadlock detected")

type LockMode int

const (
	NoLock        LockMode = iota
	ShareLock              // SELECT FOR SHARE — compatible with other ShareLocks
	ExclusiveLock          // UPDATE/DELETE/SELECT FOR UPDATE — exclusive
)

type RowLockID struct {
	Table   string
	PageNum int
	SlotNum int
}

type lockWaiter struct {
	txID uint64
	mode LockMode
	ch   chan error
}

type lockEntry struct {
	mu      sync.Mutex
	holders map[uint64]LockMode
	waiters []*lockWaiter
}

func newLockEntry() *lockEntry {
	return &lockEntry{holders: make(map[uint64]LockMode)}
}

// compatible: ShareLock+ShareLock ok; everything else conflicts
func compatible(held, want LockMode) bool {
	return held == ShareLock && want == ShareLock
}

func (le *lockEntry) canGrant(txID uint64, want LockMode) bool {
	for hTx, hMode := range le.holders {
		if hTx == txID {
			continue
		}
		if !compatible(hMode, want) {
			return false
		}
	}
	return true
}

// grantNext wakes waiters that can now be granted their lock. Must hold le.mu.
func (le *lockEntry) grantNext() {
	remaining := le.waiters[:0]
	for _, w := range le.waiters {
		if le.canGrant(w.txID, w.mode) {
			le.holders[w.txID] = w.mode
			w.ch <- nil
		} else {
			remaining = append(remaining, w)
		}
	}
	le.waiters = remaining
}

// LockManager is a row-level lock table. Matches PostgreSQL's per-tuple locking model.
type LockManager struct {
	mu      sync.Mutex
	entries map[RowLockID]*lockEntry
	// waitFor: txID → set of txIDs it's waiting on (for deadlock detection)
	waitFor map[uint64]map[uint64]struct{}
	// held: txID → set of RowLockIDs it holds (for ReleaseAll)
	held map[uint64]map[RowLockID]struct{}
}

func NewLockManager() *LockManager {
	return &LockManager{
		entries: make(map[RowLockID]*lockEntry),
		waitFor: make(map[uint64]map[uint64]struct{}),
		held:    make(map[uint64]map[RowLockID]struct{}),
	}
}

func (lm *LockManager) entry(row RowLockID) *lockEntry {
	le, ok := lm.entries[row]
	if !ok {
		le = newLockEntry()
		lm.entries[row] = le
	}
	return le
}

// Acquire obtains a lock on row for txID. Blocks if conflicting lock held.
// Returns ErrDeadlock if granting would create a wait cycle.
func (lm *LockManager) Acquire(txID uint64, row RowLockID, mode LockMode) error {
	if mode == NoLock {
		return nil
	}

	lm.mu.Lock()
	le := lm.entry(row)
	le.mu.Lock()

	// If txID already holds this lock at equal or higher mode, no-op.
	if existing, ok := le.holders[txID]; ok {
		if existing == ExclusiveLock || mode == ShareLock {
			le.mu.Unlock()
			lm.mu.Unlock()
			return nil
		}
		// Upgrade Share → Exclusive (only safe if txID is sole holder).
		if le.canGrant(txID, ExclusiveLock) {
			le.holders[txID] = ExclusiveLock
			le.mu.Unlock()
			lm.mu.Unlock()
			return nil
		}
	}

	if le.canGrant(txID, mode) {
		le.holders[txID] = mode
		le.mu.Unlock()
		lm.trackHeld(txID, row)
		lm.mu.Unlock()
		return nil
	}

	// Add wait-for edges (txID waits on all current holders).
	if lm.waitFor[txID] == nil {
		lm.waitFor[txID] = make(map[uint64]struct{})
	}
	for hTx := range le.holders {
		if hTx != txID {
			lm.waitFor[txID][hTx] = struct{}{}
		}
	}

	// Deadlock check before blocking.
	if lm.detectDeadlock(txID) {
		delete(lm.waitFor, txID)
		le.mu.Unlock()
		lm.mu.Unlock()
		return ErrDeadlock
	}

	ch := make(chan error, 1)
	w := &lockWaiter{txID: txID, mode: mode, ch: ch}
	le.waiters = append(le.waiters, w)
	le.mu.Unlock()
	lm.mu.Unlock()

	err := <-ch

	lm.mu.Lock()
	delete(lm.waitFor, txID)
	if err == nil {
		lm.trackHeld(txID, row)
	}
	lm.mu.Unlock()

	return err
}

// detectDeadlock returns true if txID is in a wait-for cycle (DFS). Must hold lm.mu.
func (lm *LockManager) detectDeadlock(start uint64) bool {
	visited := map[uint64]bool{}
	var dfs func(tx uint64) bool
	dfs = func(tx uint64) bool {
		if tx == start && len(visited) > 0 {
			return true
		}
		if visited[tx] {
			return false
		}
		visited[tx] = true
		for dep := range lm.waitFor[tx] {
			if dfs(dep) {
				return true
			}
		}
		return false
	}
	for dep := range lm.waitFor[start] {
		if dfs(dep) {
			return true
		}
	}
	return false
}

func (lm *LockManager) trackHeld(txID uint64, row RowLockID) {
	if lm.held[txID] == nil {
		lm.held[txID] = make(map[RowLockID]struct{})
	}
	lm.held[txID][row] = struct{}{}
}

// Release releases txID's lock on row and notifies eligible waiters.
func (lm *LockManager) Release(txID uint64, row RowLockID) {
	lm.mu.Lock()
	le, ok := lm.entries[row]
	if !ok {
		lm.mu.Unlock()
		return
	}
	le.mu.Lock()
	delete(le.holders, txID)
	if lm.held[txID] != nil {
		delete(lm.held[txID], row)
	}
	le.grantNext()
	le.mu.Unlock()
	lm.mu.Unlock()
}

// ReleaseAll releases all locks held by txID. Call on COMMIT or ROLLBACK.
func (lm *LockManager) ReleaseAll(txID uint64) {
	lm.mu.Lock()
	rows := make([]RowLockID, 0, len(lm.held[txID]))
	for row := range lm.held[txID] {
		rows = append(rows, row)
	}
	lm.mu.Unlock()

	for _, row := range rows {
		lm.Release(txID, row)
	}

	lm.mu.Lock()
	delete(lm.held, txID)
	delete(lm.waitFor, txID)
	lm.mu.Unlock()
}

// HeldCount returns number of locks held by txID (for tracing).
func (lm *LockManager) HeldCount(txID uint64) int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return len(lm.held[txID])
}
