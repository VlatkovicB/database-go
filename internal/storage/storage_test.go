package storage

import (
	"testing"
)

func newDB() *Database {
	return New()
}

func createUsersTable(t *testing.T, db *Database) {
	t.Helper()
	err := db.CreateTable("users", []Column{
		{Name: "id", Type: TypeInt, Primary: true},
		{Name: "name", Type: TypeText},
		{Name: "age", Type: TypeInt},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
}

// ─── CreateTable / DropTable ──────────────────────────────────────────────────

func TestCreateTable(t *testing.T) {
	db := newDB()
	if err := db.CreateTable("t", []Column{{Name: "id", Type: TypeInt}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if _, err := db.GetTable("t"); err != nil {
		t.Errorf("GetTable after create: %v", err)
	}
}

func TestCreateTableDuplicate(t *testing.T) {
	db := newDB()
	db.CreateTable("t", nil)
	if err := db.CreateTable("t", nil); err == nil {
		t.Error("expected error creating duplicate table")
	}
}

func TestDropTable(t *testing.T) {
	db := newDB()
	db.CreateTable("t", nil)
	if err := db.DropTable("t"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	if _, err := db.GetTable("t"); err == nil {
		t.Error("expected error getting dropped table")
	}
}

func TestDropNonExistentTable(t *testing.T) {
	db := newDB()
	if err := db.DropTable("nonexistent"); err == nil {
		t.Error("expected error dropping non-existent table")
	}
}

func TestGetTableNotFound(t *testing.T) {
	db := newDB()
	if _, err := db.GetTable("missing"); err == nil {
		t.Error("expected error")
	}
}

// ─── Insert / Scan ────────────────────────────────────────────────────────────

func TestInsertAndScan(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)
	db.Insert("users", Row{"id": int64(2), "name": "Bob", "age": int64(25)}, 0)

	rows, cols, err := db.Scan("users", nil, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
	if len(cols) != 3 {
		t.Errorf("expected 3 columns, got %d", len(cols))
	}
}

func TestInsertNonExistentTable(t *testing.T) {
	db := newDB()
	if err := db.Insert("nope", Row{}, 0); err == nil {
		t.Error("expected error inserting into non-existent table")
	}
}

func TestScanNonExistentTable(t *testing.T) {
	db := newDB()
	if _, _, err := db.Scan("nope", nil, 0); err == nil {
		t.Error("expected error scanning non-existent table")
	}
}

// ─── UpdateRows ───────────────────────────────────────────────────────────────

func TestUpdateRows(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)
	db.Insert("users", Row{"id": int64(2), "name": "Bob", "age": int64(25)}, 0)

	count, _, _, err := db.UpdateRows("users",
		func(r Row) bool { return r["name"] == "Alice" },
		func(r Row) Row {
			n := Row{"id": r["id"], "name": r["name"], "age": int64(31)}
			return n
		},
		0,
	)
	if err != nil || count != 1 {
		t.Fatalf("UpdateRows: count=%d err=%v", count, err)
	}

	rows, _, _ := db.Scan("users", nil, 0)
	for _, row := range rows {
		if row["name"] == "Alice" && row["age"] != int64(31) {
			t.Errorf("Alice's age not updated: %v", row["age"])
		}
	}
}

func TestUpdateNoMatch(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)

	count, _, _, err := db.UpdateRows("users",
		func(r Row) bool { return r["name"] == "Nobody" },
		func(r Row) Row { return r },
		0,
	)
	if err != nil || count != 0 {
		t.Errorf("expected 0 updates, got count=%d err=%v", count, err)
	}
}

// ─── DeleteRows ───────────────────────────────────────────────────────────────

func TestDeleteRows(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)
	db.Insert("users", Row{"id": int64(2), "name": "Bob", "age": int64(20)}, 0)

	count, _, err := db.DeleteRows("users", func(r Row) bool {
		return r["age"].(int64) < 25
	}, 0)
	if err != nil || count != 1 {
		t.Fatalf("DeleteRows: count=%d err=%v", count, err)
	}

	rows, _, _ := db.Scan("users", nil, 0)
	if len(rows) != 1 {
		t.Errorf("expected 1 row after delete, got %d", len(rows))
	}
	if rows[0]["name"] != "Alice" {
		t.Errorf("wrong row survived: %v", rows[0]["name"])
	}
}

func TestDeleteAllRows(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)
	db.DeleteRows("users", func(r Row) bool { return true }, 0)

	rows, _, _ := db.Scan("users", nil, 0)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after delete all, got %d", len(rows))
	}
}

// ─── RowCount / PageCount ─────────────────────────────────────────────────────

func TestRowCount(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "A", "age": int64(1)}, 0)
	db.Insert("users", Row{"id": int64(2), "name": "B", "age": int64(2)}, 0)

	n, err := db.RowCount("users")
	if err != nil || n != 2 {
		t.Errorf("RowCount: %d %v", n, err)
	}
}

func TestPageCount(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	// insert enough rows to span multiple pages
	for i := int64(0); i < 100; i++ {
		db.Insert("users", Row{"id": i, "name": "x", "age": i}, 0)
	}
	n, err := db.PageCount("users")
	if err != nil || n < 1 {
		t.Errorf("PageCount: %d %v", n, err)
	}
}

// ─── Indexes ──────────────────────────────────────────────────────────────────

func TestCreateAndDropIndex(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)

	if err := db.CreateIndex("idx_age", "users", "age"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	idxes := db.ListIndexesForTable("users")
	if len(idxes) != 1 || idxes[0].Name != "idx_age" {
		t.Errorf("expected idx_age, got %v", idxes)
	}

	if err := db.DropIndex("idx_age", "users"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if len(db.ListIndexesForTable("users")) != 0 {
		t.Error("index should be gone after drop")
	}
}

func TestCreateIndexDuplicate(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.CreateIndex("idx", "users", "age")
	if err := db.CreateIndex("idx", "users", "age"); err == nil {
		t.Error("expected error creating duplicate index")
	}
}

func TestCreateIndexBadColumn(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	if err := db.CreateIndex("idx", "users", "nonexistent"); err == nil {
		t.Error("expected error for non-existent column")
	}
}

func TestIndexRangeScan(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	for i := int64(1); i <= 10; i++ {
		db.Insert("users", Row{"id": i, "name": "u", "age": i * 10}, 0)
	}
	db.CreateIndex("idx_age", "users", "age")

	tuples, _, err := db.IndexRangeScan("users", "idx_age", int64(30), ">=", int64(60), "<=")
	if err != nil {
		t.Fatalf("IndexRangeScan: %v", err)
	}
	if len(tuples) != 4 {
		t.Errorf("expected 4 tuples (30,40,50,60), got %d", len(tuples))
	}
}

func TestFindIndexForColumn(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.CreateIndex("idx_age", "users", "age")

	name, found := db.FindIndexForColumn("users", "age")
	if !found || name != "idx_age" {
		t.Errorf("FindIndexForColumn: found=%v name=%q", found, name)
	}

	_, found = db.FindIndexForColumn("users", "name")
	if found {
		t.Error("should not find index for unindexed column")
	}
}

func TestDropIndexByName(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.CreateIndex("idx_age", "users", "age")
	if err := db.DropIndexByName("idx_age", false); err != nil {
		t.Fatalf("DropIndexByName: %v", err)
	}
}

func TestDropIndexByNameIfExists(t *testing.T) {
	db := newDB()
	if err := db.DropIndexByName("nonexistent", true); err != nil {
		t.Errorf("DropIndexByName(ifExists): %v", err)
	}
	if err := db.DropIndexByName("nonexistent", false); err == nil {
		t.Error("expected error when index missing and ifExists=false")
	}
}

// ─── ANALYZE / Stats ──────────────────────────────────────────────────────────

func TestAnalyzeTable(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	for i := int64(1); i <= 20; i++ {
		db.Insert("users", Row{"id": i, "name": "u", "age": i}, 0)
	}
	lines, err := db.AnalyzeTable("users")
	if err != nil {
		t.Fatalf("AnalyzeTable: %v", err)
	}
	if len(lines) == 0 {
		t.Error("expected analyze output lines")
	}
	stats := db.GetTableStats("users")
	if stats == nil {
		t.Error("GetTableStats should return stats after ANALYZE")
	}
	if stats.Columns["age"] == nil {
		t.Error("expected stats for age column")
	}
}

func TestGetTableStatsNoAnalyze(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	if stats := db.GetTableStats("users"); stats != nil {
		t.Error("expected nil stats before ANALYZE")
	}
}

// ─── ListTables ───────────────────────────────────────────────────────────────

func TestListTables(t *testing.T) {
	db := newDB()
	db.CreateTable("a", nil)
	db.CreateTable("b", nil)
	tables := db.ListTables()
	if len(tables) != 2 {
		t.Errorf("expected 2 tables, got %d", len(tables))
	}
	if tables[0].Name != "a" || tables[1].Name != "b" {
		t.Errorf("tables not sorted: %v, %v", tables[0].Name, tables[1].Name)
	}
}

// ─── Vacuum ───────────────────────────────────────────────────────────────────

func TestVacuum(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)

	// Begin + insert + commit
	tx := db.TxManager.Begin()
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, tx.ID)
	db.TxManager.Commit(tx.ID)

	// Begin + delete + commit (MVCC soft-delete)
	tx2 := db.TxManager.Begin()
	db.DeleteRows("users", func(r Row) bool { return true }, tx2.ID)
	db.TxManager.Commit(tx2.ID)

	reclaimed, err := db.Vacuum("users")
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("expected 1 reclaimed, got %d", reclaimed)
	}
}

func TestVacuumNoDeadTuples(t *testing.T) {
	db := newDB()
	createUsersTable(t, db)
	db.Insert("users", Row{"id": int64(1), "name": "Alice", "age": int64(30)}, 0)

	reclaimed, err := db.Vacuum("users")
	if err != nil || reclaimed != 0 {
		t.Errorf("expected 0 reclaimed, got %d %v", reclaimed, err)
	}
}
