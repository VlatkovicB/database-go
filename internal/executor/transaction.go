package executor

// =============================================================================
// Transaction control (MVCC Phase 5)
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
)

func (e *Executor) execBegin() (*Result, error) {
	if e.CurrentTx != nil {
		return nil, fmt.Errorf("there is already an open transaction (xid=%d)", e.CurrentTx.ID)
	}
	e.CurrentTx = e.db.TxManager.Begin()
	e.db.WAL.Append(e.CurrentTx.ID, storage.WALBegin, "", nil, nil)
	return &Result{
		Message: "BEGIN",
		Trace: []string{
			fmt.Sprintf("Assigned xid=%d", e.CurrentTx.ID),
			fmt.Sprintf("Snapshot: xmin=%d xmax=%d active=%v",
				e.CurrentTx.Snapshot.Xmin,
				e.CurrentTx.Snapshot.Xmax,
				e.CurrentTx.Snapshot.Active),
		},
	}, nil
}

func (e *Executor) execCommit() (*Result, error) {
	if e.CurrentTx == nil {
		return nil, fmt.Errorf("there is no open transaction")
	}
	xid := e.CurrentTx.ID
	if err := e.db.TxManager.Commit(xid); err != nil {
		return nil, err
	}
	e.db.WAL.Append(xid, storage.WALCommit, "", nil, nil)
	e.CurrentTx = nil
	return &Result{
		Message: "COMMIT",
		Trace: []string{
			fmt.Sprintf("xid=%d marked COMMITTED", xid),
			"Tuple versions written by this tx are now visible to other transactions",
		},
	}, nil
}

func (e *Executor) execRollback() (*Result, error) {
	if e.CurrentTx == nil {
		return nil, fmt.Errorf("there is no open transaction")
	}
	xid := e.CurrentTx.ID
	if err := e.db.TxManager.Abort(xid); err != nil {
		return nil, err
	}
	e.db.WAL.Append(xid, storage.WALRollback, "", nil, nil)
	e.CurrentTx = nil
	return &Result{
		Message: "ROLLBACK",
		Trace: []string{
			fmt.Sprintf("xid=%d marked ABORTED", xid),
			"Tuple versions written by this tx are invisible (xmin not committed)",
			"Dead tuples will be reclaimed by VACUUM",
		},
	}, nil
}

func (e *Executor) execVacuum(s *parser.VacuumStatement) (*Result, error) {
	reclaimed, err := e.db.Vacuum(s.Table)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("VACUUM %q — %d dead tuple(s) reclaimed", s.Table, reclaimed),
		Trace: []string{
			fmt.Sprintf("Scan %q for tuples where xmax != 0 AND xmax is committed", s.Table),
			fmt.Sprintf("Reclaimed %d dead tuple(s)", reclaimed),
			"Rebuilt indexes",
		},
	}, nil
}
