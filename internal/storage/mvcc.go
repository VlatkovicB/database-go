package storage

import (
	"fmt"
	"sync"
)

// TxStatus represents the lifecycle state of a transaction.
type TxStatus int

const (
	TxActive    TxStatus = iota // transaction is in progress
	TxCommitted                  // transaction committed successfully
	TxAborted                    // transaction was rolled back
)

// Snapshot captures the database state at BEGIN time (PostgreSQL-style).
// Used to determine which tuple versions are visible to a transaction.
type Snapshot struct {
	Xmin   uint64   // lowest active xid at snapshot time — all xids below are committed
	Xmax   uint64   // next xid to be assigned — all xids >= this are invisible
	Active []uint64 // xids in-progress at snapshot time (committed later but invisible now)
}

// Transaction represents an open database transaction.
type Transaction struct {
	ID       uint64
	Status   TxStatus
	Snapshot Snapshot
}

// TxManager manages all active and historical transactions.
// It issues monotonically increasing transaction IDs and tracks their status.
type TxManager struct {
	mu      sync.Mutex
	nextXID uint64
	txs     map[uint64]*Transaction
}

// NewTxManager creates a new transaction manager. XIDs start at 1.
func NewTxManager() *TxManager {
	return &TxManager{
		nextXID: 1,
		txs:     make(map[uint64]*Transaction),
	}
}

// Begin starts a new transaction, takes a snapshot of the current state, and returns it.
func (m *TxManager) Begin() *Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()

	xid := m.nextXID
	m.nextXID++

	// Build snapshot: collect all currently active transaction IDs.
	var active []uint64
	xmin := xid // default: no lower active xids
	for id, tx := range m.txs {
		if tx.Status == TxActive {
			active = append(active, id)
			if id < xmin {
				xmin = id
			}
		}
	}
	if len(active) == 0 {
		xmin = xid
	}

	tx := &Transaction{
		ID:     xid,
		Status: TxActive,
		Snapshot: Snapshot{
			Xmin:   xmin,
			Xmax:   xid, // our own xid and anything higher is invisible
			Active: active,
		},
	}
	m.txs[xid] = tx
	return tx
}

// Commit marks a transaction as committed.
func (m *TxManager) Commit(xid uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[xid]
	if !ok {
		return fmt.Errorf("transaction %d not found", xid)
	}
	if tx.Status != TxActive {
		return fmt.Errorf("transaction %d is not active (status=%d)", xid, tx.Status)
	}
	tx.Status = TxCommitted
	return nil
}

// Abort marks a transaction as aborted (rolled back).
func (m *TxManager) Abort(xid uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[xid]
	if !ok {
		return fmt.Errorf("transaction %d not found", xid)
	}
	if tx.Status != TxActive {
		return fmt.Errorf("transaction %d is not active (status=%d)", xid, tx.Status)
	}
	tx.Status = TxAborted
	return nil
}

// GetTx returns the transaction with the given ID.
func (m *TxManager) GetTx(xid uint64) (*Transaction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[xid]
	return tx, ok
}

// IsCommitted reports whether transaction xid has committed.
// xid == 0 means auto-committed (legacy pre-MVCC), always true.
func (m *TxManager) IsCommitted(xid uint64) bool {
	if xid == 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[xid]
	return ok && tx.Status == TxCommitted
}

// isInSlice checks if xid appears in the active list.
func isInSlice(xid uint64, active []uint64) bool {
	for _, id := range active {
		if id == xid {
			return true
		}
	}
	return false
}

// Visible implements PostgreSQL-style MVCC tuple visibility.
//
// Rules (mirrors HeapTupleSatisfiesMVCC in PG):
//
//  1. xmin == 0: legacy/auto-committed tuple — always visible (backward compat)
//  2. xmin == currentXID: we inserted it — visible unless we also deleted it
//  3. xmin not committed, or xmin >= snap.Xmax, or xmin in snap.Active:
//     the inserting transaction wasn't visible at snapshot time → invisible
//  4. Once xmin is visible, check xmax:
//     - xmax == 0: not deleted → visible
//     - xmax == currentXID: we deleted it → invisible
//     - xmax not committed, or xmax >= snap.Xmax, or xmax in snap.Active:
//     the deleting tx wasn't committed at snapshot time → still visible
//     - else: deleted and committed → invisible
func Visible(xmin, xmax uint64, snap Snapshot, currentXID uint64, mgr *TxManager) bool {
	// Rule 1: legacy auto-committed tuple
	if xmin == 0 {
		if xmax == 0 {
			return true
		}
		// xmax check for legacy tuples
		if xmax == currentXID {
			return false
		}
		if !mgr.IsCommitted(xmax) || xmax >= snap.Xmax || isInSlice(xmax, snap.Active) {
			return true
		}
		return false
	}

	// Rule 2: we inserted this tuple ourselves
	if xmin == currentXID {
		if xmax == 0 {
			return true // live, inserted by us
		}
		if xmax == currentXID {
			return false // we also deleted it
		}
		// someone else deleted it (rare, but handle gracefully)
		return true
	}

	// Rule 3: check if xmin was visible at snapshot time
	xminCommitted := mgr.IsCommitted(xmin)
	if !xminCommitted || xmin >= snap.Xmax || isInSlice(xmin, snap.Active) {
		return false // inserting tx not yet committed at snapshot time
	}

	// xmin is visible — now check xmax
	if xmax == 0 {
		return true // live
	}
	if xmax == currentXID {
		return false // we deleted it
	}
	// Is the deletion committed and visible at snapshot time?
	if !mgr.IsCommitted(xmax) || xmax >= snap.Xmax || isInSlice(xmax, snap.Active) {
		return true // deletion not committed yet — tuple still visible
	}
	return false // deleted and deletion committed
}
