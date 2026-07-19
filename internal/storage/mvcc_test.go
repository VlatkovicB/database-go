package storage

import (
	"testing"
)

// ─── TxManager ────────────────────────────────────────────────────────────────

func TestBeginAssignsSequentialXIDs(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin()
	tx2 := m.Begin()
	if tx2.ID != tx1.ID+1 {
		t.Errorf("expected sequential XIDs, got %d and %d", tx1.ID, tx2.ID)
	}
}

func TestCommit(t *testing.T) {
	m := NewTxManager()
	tx := m.Begin()
	if err := m.Commit(tx.ID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !m.IsCommitted(tx.ID) {
		t.Error("transaction should be committed")
	}
}

func TestAbort(t *testing.T) {
	m := NewTxManager()
	tx := m.Begin()
	if err := m.Abort(tx.ID); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if m.IsCommitted(tx.ID) {
		t.Error("aborted transaction should not be committed")
	}
}

func TestCommitNonExistent(t *testing.T) {
	m := NewTxManager()
	if err := m.Commit(9999); err == nil {
		t.Error("expected error committing non-existent tx")
	}
}

func TestAbortNonExistent(t *testing.T) {
	m := NewTxManager()
	if err := m.Abort(9999); err == nil {
		t.Error("expected error aborting non-existent tx")
	}
}

func TestCommitAlreadyCommitted(t *testing.T) {
	m := NewTxManager()
	tx := m.Begin()
	m.Commit(tx.ID)
	if err := m.Commit(tx.ID); err == nil {
		t.Error("expected error double-committing")
	}
}

func TestIsCommittedXIDZero(t *testing.T) {
	m := NewTxManager()
	// xid=0 is auto-committed, always committed
	if !m.IsCommitted(0) {
		t.Error("xid=0 should always be considered committed")
	}
}

func TestGetTx(t *testing.T) {
	m := NewTxManager()
	tx := m.Begin()
	got, ok := m.GetTx(tx.ID)
	if !ok || got.ID != tx.ID {
		t.Errorf("GetTx: ok=%v id=%d", ok, got.ID)
	}
	_, ok = m.GetTx(9999)
	if ok {
		t.Error("GetTx for unknown xid should return false")
	}
}

func TestSnapshotCapturesActiveXIDs(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin() // xid=1, active
	tx2 := m.Begin() // xid=2, snapshot should list tx1 as active

	// tx2's snapshot should include tx1 in Active
	found := false
	for _, id := range tx2.Snapshot.Active {
		if id == tx1.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("tx2 snapshot should list tx1 (xid=%d) as active, got %v", tx1.ID, tx2.Snapshot.Active)
	}
}

// ─── Visible ──────────────────────────────────────────────────────────────────

func TestVisibleLegacyTuple(t *testing.T) {
	m := NewTxManager()
	snap := Snapshot{Xmin: 1, Xmax: 1, Active: nil}
	// xmin=0 xmax=0: legacy live tuple — always visible
	if !Visible(0, 0, snap, 1, m) {
		t.Error("legacy live tuple should be visible")
	}
}

func TestVisibleLegacyDeletedByUs(t *testing.T) {
	m := NewTxManager()
	snap := Snapshot{Xmin: 1, Xmax: 1, Active: nil}
	// xmin=0 xmax=currentXID: we deleted it — invisible
	if Visible(0, 1, snap, 1, m) {
		t.Error("legacy tuple deleted by current tx should be invisible")
	}
}

func TestVisibleInsertedByUs(t *testing.T) {
	m := NewTxManager()
	snap := Snapshot{Xmin: 1, Xmax: 1, Active: nil}
	// xmin=currentXID xmax=0: we inserted it, still live
	if !Visible(1, 0, snap, 1, m) {
		t.Error("tuple inserted by current tx should be visible")
	}
}

func TestVisibleInsertedAndDeletedByUs(t *testing.T) {
	m := NewTxManager()
	snap := Snapshot{Xmin: 1, Xmax: 1, Active: nil}
	// xmin=currentXID xmax=currentXID: we inserted and deleted it
	if Visible(1, 1, snap, 1, m) {
		t.Error("tuple inserted and deleted by current tx should be invisible")
	}
}

func TestVisibleCommittedBeforeSnapshot(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin()
	m.Commit(tx1.ID)

	tx2 := m.Begin() // snapshot sees tx1 as committed, xmax > tx1.ID
	if !Visible(tx1.ID, 0, tx2.Snapshot, tx2.ID, m) {
		t.Error("tuple from committed tx should be visible to later tx")
	}
}

func TestVisibleActiveAtSnapshot(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin() // starts active
	tx2 := m.Begin() // takes snapshot: tx1 is active

	// tx1 inserts a tuple. tx2 should NOT see it (tx1 in tx2.Snapshot.Active)
	m.Commit(tx1.ID) // commit AFTER tx2 took its snapshot

	if Visible(tx1.ID, 0, tx2.Snapshot, tx2.ID, m) {
		t.Error("tuple from tx active at snapshot time should be invisible even after it commits")
	}
}

func TestVisibleDeletedByCommittedTx(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin()
	m.Commit(tx1.ID)

	tx2 := m.Begin()
	m.Commit(tx2.ID)

	tx3 := m.Begin() // takes snapshot after both tx1, tx2 committed

	// tx2 deleted the tuple inserted by tx1; tx3 should not see it
	if Visible(tx1.ID, tx2.ID, tx3.Snapshot, tx3.ID, m) {
		t.Error("tuple deleted by committed tx should be invisible")
	}
}

func TestVisibleDeletedByUncommittedTx(t *testing.T) {
	m := NewTxManager()
	tx1 := m.Begin()
	m.Commit(tx1.ID)

	tx2 := m.Begin() // active deleter

	tx3 := m.Begin() // tx2 is active at snapshot time → deletion not visible

	// tx3 should still see the tuple (deletion by tx2 not committed yet)
	if !Visible(tx1.ID, tx2.ID, tx3.Snapshot, tx3.ID, m) {
		t.Error("tuple with uncommitted deletion should still be visible")
	}
}

// ─── MVCC isolation via Database ─────────────────────────────────────────────

func TestMVCCInsertVisibility(t *testing.T) {
	db := newDB()
	db.CreateTable("t", []Column{{Name: "id", Type: TypeInt}})

	// tx1: insert, not yet committed
	tx1 := db.TxManager.Begin()
	db.Insert("t", Row{"id": int64(1)}, tx1.ID)

	// tx2: takes snapshot before tx1 commits
	tx2 := db.TxManager.Begin()
	snap2 := tx2.Snapshot

	// tx2 should NOT see tx1's insert
	rows, _, _ := db.Scan("t", &snap2, tx2.ID)
	if len(rows) != 0 {
		t.Errorf("tx2 should see 0 rows before tx1 commits, got %d", len(rows))
	}

	// commit tx1
	db.TxManager.Commit(tx1.ID)

	// tx2 still cannot see it (snapshot taken before commit)
	rows, _, _ = db.Scan("t", &snap2, tx2.ID)
	if len(rows) != 0 {
		t.Errorf("tx2 should still see 0 rows (snapshot before commit), got %d", len(rows))
	}

	// new tx3 (after tx1 committed) CAN see it
	tx3 := db.TxManager.Begin()
	snap3 := tx3.Snapshot
	rows, _, _ = db.Scan("t", &snap3, tx3.ID)
	if len(rows) != 1 {
		t.Errorf("tx3 should see 1 row, got %d", len(rows))
	}
}

func TestMVCCRollback(t *testing.T) {
	db := newDB()
	db.CreateTable("t", []Column{{Name: "id", Type: TypeInt}})

	tx1 := db.TxManager.Begin()
	db.Insert("t", Row{"id": int64(42)}, tx1.ID)
	db.TxManager.Abort(tx1.ID) // rollback

	// no other tx should see the aborted insert
	tx2 := db.TxManager.Begin()
	snap2 := tx2.Snapshot
	rows, _, _ := db.Scan("t", &snap2, tx2.ID)
	if len(rows) != 0 {
		t.Errorf("aborted insert should be invisible, got %d rows", len(rows))
	}
}

func TestMVCCUpdateCreatesNewVersion(t *testing.T) {
	db := newDB()
	db.CreateTable("t", []Column{{Name: "id", Type: TypeInt}, {Name: "v", Type: TypeInt}})

	// auto-commit insert
	db.Insert("t", Row{"id": int64(1), "v": int64(10)}, 0)

	// tx1: update v = 20
	tx1 := db.TxManager.Begin()
	db.UpdateRows("t",
		func(r Row) bool { return r["id"] == int64(1) },
		func(r Row) Row { return Row{"id": r["id"], "v": int64(20)} },
		tx1.ID,
	)

	// Before commit: tx1 sees new version (xmin=tx1.ID, inserted by us)
	snap1 := tx1.Snapshot
	rows, _, _ := db.Scan("t", &snap1, tx1.ID)
	// tx1 should see v=20 (its own write)
	found20 := false
	for _, r := range rows {
		if r["v"] == int64(20) {
			found20 = true
		}
	}
	if !found20 {
		t.Error("tx1 should see its own updated row (v=20)")
	}

	db.TxManager.Commit(tx1.ID)

	// tx2 after commit sees v=20
	tx2 := db.TxManager.Begin()
	snap2 := tx2.Snapshot
	rows, _, _ = db.Scan("t", &snap2, tx2.ID)
	if len(rows) != 1 || rows[0]["v"] != int64(20) {
		t.Errorf("after commit tx2 should see v=20, got %v", rows)
	}
}
