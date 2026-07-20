package storage

import (
	"sync"
	"time"
)

type WALOp string

const (
	WALInsert     WALOp = "INSERT"
	WALUpdate     WALOp = "UPDATE"
	WALDelete     WALOp = "DELETE"
	WALBegin      WALOp = "BEGIN"
	WALCommit     WALOp = "COMMIT"
	WALRollback   WALOp = "ROLLBACK"
	WALCheckpoint WALOp = "CHECKPOINT"
)

type WALRecord struct {
	LSN       uint64    `json:"lsn"`
	XID       uint64    `json:"xid"`
	Op        WALOp     `json:"op"`
	Table     string    `json:"table,omitempty"`
	OldRows   []Row     `json:"oldRows,omitempty"`
	NewRows   []Row     `json:"newRows,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type tableSnapshot struct {
	Columns     []Column
	ForeignKeys []FKConstraint
	Pages       []Page
}

type walCheckpoint struct {
	LSN     uint64
	Tables  map[string]tableSnapshot
	NextXID uint64
	TxState map[uint64]TxStatus
}

type WALManager struct {
	mu         sync.Mutex
	records    []WALRecord
	nextLSN    uint64
	checkpoint *walCheckpoint
}

func NewWALManager() *WALManager {
	return &WALManager{nextLSN: 1}
}

func (w *WALManager) Append(xid uint64, op WALOp, table string, oldRows, newRows []Row) WALRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := WALRecord{
		LSN:       w.nextLSN,
		XID:       xid,
		Op:        op,
		Table:     table,
		OldRows:   oldRows,
		NewRows:   newRows,
		Timestamp: time.Now(),
	}
	w.records = append(w.records, rec)
	w.nextLSN++
	return rec
}

func (w *WALManager) Records() []WALRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]WALRecord, len(w.records))
	copy(out, w.records)
	return out
}

func (w *WALManager) CheckpointLSN() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.checkpoint == nil {
		return 0
	}
	return w.checkpoint.LSN
}

func (w *WALManager) HasCheckpoint() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkpoint != nil
}

// TakeCheckpoint snapshots current DB state into WAL. Returns the CHECKPOINT record.
func (w *WALManager) TakeCheckpoint(db *Database) WALRecord {
	db.mu.RLock()

	tables := make(map[string]tableSnapshot, len(db.Tables))
	for name, t := range db.Tables {
		pages := make([]Page, len(t.Pages))
		for i, pg := range t.Pages {
			tuples := make([]Tuple, len(pg.Tuples))
			for j, tpl := range pg.Tuples {
				rowCopy := make(Row, len(tpl.Data))
				for k, v := range tpl.Data {
					rowCopy[k] = v
				}
				tuples[j] = Tuple{
					PageNum: tpl.PageNum, SlotNum: tpl.SlotNum,
					Data: rowCopy, Xmin: tpl.Xmin, Xmax: tpl.Xmax,
				}
			}
			pages[i] = Page{Tuples: tuples}
		}
		colsCopy := make([]Column, len(t.Columns))
		copy(colsCopy, t.Columns)
		fksCopy := make([]FKConstraint, len(t.ForeignKeys))
		copy(fksCopy, t.ForeignKeys)
		tables[name] = tableSnapshot{Columns: colsCopy, ForeignKeys: fksCopy, Pages: pages}
	}

	db.TxManager.mu.Lock()
	nextXID := db.TxManager.nextXID
	txState := make(map[uint64]TxStatus, len(db.TxManager.txs))
	for id, tx := range db.TxManager.txs {
		txState[id] = tx.Status
	}
	db.TxManager.mu.Unlock()

	db.mu.RUnlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	rec := WALRecord{
		LSN:       w.nextLSN,
		XID:       0,
		Op:        WALCheckpoint,
		Timestamp: time.Now(),
	}
	w.records = append(w.records, rec)
	w.nextLSN++
	w.checkpoint = &walCheckpoint{
		LSN:     rec.LSN,
		Tables:  tables,
		NextXID: nextXID,
		TxState: txState,
	}
	return rec
}

// RestoreCheckpoint reverts the DB to the last checkpoint state (crash simulation).
// Returns false if no checkpoint exists.
func (w *WALManager) RestoreCheckpoint(db *Database) bool {
	w.mu.Lock()
	cp := w.checkpoint
	w.mu.Unlock()

	if cp == nil {
		return false
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	db.Tables = make(map[string]*Table)
	for name, snap := range cp.Tables {
		pages := make([]Page, len(snap.Pages))
		for i, pg := range snap.Pages {
			tuples := make([]Tuple, len(pg.Tuples))
			for j, tpl := range pg.Tuples {
				rowCopy := make(Row, len(tpl.Data))
				for k, v := range tpl.Data {
					rowCopy[k] = v
				}
				tuples[j] = Tuple{
					PageNum: tpl.PageNum, SlotNum: tpl.SlotNum,
					Data: rowCopy, Xmin: tpl.Xmin, Xmax: tpl.Xmax,
				}
			}
			pages[i] = Page{Tuples: tuples}
		}
		colsCopy := make([]Column, len(snap.Columns))
		copy(colsCopy, snap.Columns)
		fksCopy := make([]FKConstraint, len(snap.ForeignKeys))
		copy(fksCopy, snap.ForeignKeys)
		tbl := &Table{
			Name:        name,
			Columns:     colsCopy,
			ForeignKeys: fksCopy,
			Pages:       pages,
			Indexes:     make(map[string]*Index),
		}
		tbl.tuplesPerPage = TuplesPerPage(colsCopy)
		tbl.rebuildIndexes()
		db.Tables[name] = tbl
	}

	db.TxManager.mu.Lock()
	db.TxManager.nextXID = cp.NextXID
	db.TxManager.txs = make(map[uint64]*Transaction)
	for id, status := range cp.TxState {
		db.TxManager.txs[id] = &Transaction{ID: id, Status: status}
	}
	db.TxManager.mu.Unlock()

	return true
}

// Replay re-applies WAL records written after the last checkpoint.
// Designed to run after RestoreCheckpoint to recover committed data.
func (w *WALManager) Replay(db *Database) (int, error) {
	w.mu.Lock()
	cp := w.checkpoint
	var toReplay []WALRecord
	for _, r := range w.records {
		if cp != nil && r.LSN <= cp.LSN {
			continue
		}
		toReplay = append(toReplay, r)
	}
	w.mu.Unlock()

	replayed := 0
	for _, rec := range toReplay {
		switch rec.Op {
		case WALInsert:
			for _, row := range rec.NewRows {
				rowCopy := make(Row, len(row))
				for k, v := range row {
					rowCopy[k] = v
				}
				_ = db.Insert(rec.Table, rowCopy, rec.XID)
			}
		case WALUpdate:
			for i, oldRow := range rec.OldRows {
				if i >= len(rec.NewRows) {
					break
				}
				captured := oldRow
				newCopy := make(Row, len(rec.NewRows[i]))
				for k, v := range rec.NewRows[i] {
					newCopy[k] = v
				}
				_, _, _, _ = db.UpdateRows(rec.Table,
					func(r Row) bool { return rowsEqual(r, captured) },
					func(_ Row) Row { return newCopy },
					rec.XID,
				)
			}
		case WALDelete:
			for _, row := range rec.OldRows {
				captured := row
				_, _, _ = db.DeleteRows(rec.Table, func(r Row) bool {
					return rowsEqual(r, captured)
				}, rec.XID)
			}
		case WALBegin:
			db.TxManager.mu.Lock()
			if _, exists := db.TxManager.txs[rec.XID]; !exists {
				db.TxManager.txs[rec.XID] = &Transaction{ID: rec.XID, Status: TxActive}
				if rec.XID >= db.TxManager.nextXID {
					db.TxManager.nextXID = rec.XID + 1
				}
			}
			db.TxManager.mu.Unlock()
		case WALCommit:
			db.TxManager.mu.Lock()
			if tx, exists := db.TxManager.txs[rec.XID]; exists {
				tx.Status = TxCommitted
			}
			db.TxManager.mu.Unlock()
		case WALRollback:
			db.TxManager.mu.Lock()
			if tx, exists := db.TxManager.txs[rec.XID]; exists {
				tx.Status = TxAborted
			}
			db.TxManager.mu.Unlock()
		}
		replayed++
	}
	return replayed, nil
}

func rowsEqual(a, b Row) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}
