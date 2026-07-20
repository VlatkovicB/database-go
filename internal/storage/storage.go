package storage

import (
	"fmt"
	"sort"
	"sync"
)

type ColumnType string

const (
	TypeInt     ColumnType = "INT"
	TypeText    ColumnType = "TEXT"
	TypeBoolean ColumnType = "BOOLEAN"
	TypeFloat   ColumnType = "FLOAT"
)

type Column struct {
	Name    string
	Type    ColumnType
	Primary bool
}

type FKConstraint struct {
	Column    string
	RefTable  string
	RefColumn string
}

// Row is a single record — column name to value.
// Values are int64, float64, string, bool, or nil.
type Row map[string]interface{}

type Index struct {
	Name   string
	Column string
	Tree   *BTree
}

type IndexInfo struct {
	Name   string
	Column string
	Size   int
}

type Table struct {
	Name          string
	Columns       []Column
	ForeignKeys   []FKConstraint
	Pages         []Page
	tuplesPerPage int
	Indexes       map[string]*Index
	Stats         *TableStats // populated by ANALYZE
}

type Database struct {
	mu        sync.RWMutex
	Tables    map[string]*Table
	TxManager *TxManager
	WAL       *WALManager
}

func New() *Database {
	return &Database{
		Tables:    make(map[string]*Table),
		TxManager: NewTxManager(),
		WAL:       NewWALManager(),
	}
}

func (db *Database) CreateTable(name string, cols []Column) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, exists := db.Tables[name]; exists {
		return fmt.Errorf("table %q already exists", name)
	}
	db.Tables[name] = &Table{
		Name:          name,
		Columns:       cols,
		tuplesPerPage: TuplesPerPage(cols),
		Indexes:       make(map[string]*Index),
	}
	return nil
}

func (db *Database) DropTable(name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, exists := db.Tables[name]; !exists {
		return fmt.Errorf("table %q does not exist", name)
	}
	delete(db.Tables, name)
	return nil
}

func (db *Database) GetTable(name string) (*Table, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[name]
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", name)
	}
	return t, nil
}

func (db *Database) SetForeignKeys(tableName string, fks []FKConstraint) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %q does not exist", tableName)
	}
	t.ForeignKeys = fks
	return nil
}

// Insert appends a new tuple to the table. xid is the inserting transaction ID
// (0 means auto-committed / pre-MVCC — always visible to all transactions).
func (db *Database) Insert(tableName string, row Row, xid uint64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %q does not exist", tableName)
	}

	// Check PRIMARY KEY uniqueness
	for _, col := range t.Columns {
		if !col.Primary {
			continue
		}
		pkVal, hasPK := row[col.Name]
		if !hasPK || pkVal == nil {
			return fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
		}
		for _, pg := range t.Pages {
			for _, tpl := range pg.Tuples {
				if tpl.Xmax != 0 {
					continue
				}
				if tpl.Data[col.Name] == pkVal {
					return fmt.Errorf("duplicate key value violates unique constraint on %q: %q=%v already exists", tableName, col.Name, pkVal)
				}
			}
		}
	}

	// Check FOREIGN KEY constraints
	for _, fk := range t.ForeignKeys {
		val := row[fk.Column]
		if val == nil {
			continue // NULL is allowed
		}
		refT, ok := db.Tables[fk.RefTable]
		if !ok {
			return fmt.Errorf("foreign key references unknown table %q", fk.RefTable)
		}
		found := false
		for _, pg := range refT.Pages {
			for _, tpl := range pg.Tuples {
				if tpl.Xmax != 0 {
					continue
				}
				if tpl.Data[fk.RefColumn] == val {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return fmt.Errorf("insert violates foreign key constraint: %q.%q=%v has no match in %q.%q", tableName, fk.Column, val, fk.RefTable, fk.RefColumn)
		}
	}

	var inserted Tuple
	if len(t.Pages) > 0 {
		last := &t.Pages[len(t.Pages)-1]
		if len(last.Tuples) < t.tuplesPerPage {
			slotNum := len(last.Tuples)
			pageNum := len(t.Pages) - 1
			inserted = Tuple{PageNum: pageNum, SlotNum: slotNum, Data: row, Xmin: xid}
			last.Tuples = append(last.Tuples, inserted)
			t.updateIndexes(inserted)
			return nil
		}
	}
	pageNum := len(t.Pages)
	inserted = Tuple{PageNum: pageNum, SlotNum: 0, Data: row, Xmin: xid}
	pg := Page{Tuples: []Tuple{inserted}}
	t.Pages = append(t.Pages, pg)
	t.updateIndexes(inserted)
	return nil
}

func (t *Table) updateIndexes(tuple Tuple) {
	if tuple.Xmax != 0 {
		return // don't index dead tuples
	}
	for _, idx := range t.Indexes {
		if val, ok := tuple.Data[idx.Column]; ok {
			idx.Tree.Insert(val, tuple)
		}
	}
}

func (t *Table) rebuildIndexes() {
	for _, idx := range t.Indexes {
		idx.Tree = NewBTree()
		for pageNum, pg := range t.Pages {
			for slotNum, tpl := range pg.Tuples {
				// Don't index dead tuples (Xmax != 0 means deleted)
				if tpl.Xmax != 0 {
					continue
				}
				if val, ok := tpl.Data[idx.Column]; ok {
					idx.Tree.Insert(val, Tuple{PageNum: pageNum, SlotNum: slotNum, Data: tpl.Data, Xmin: tpl.Xmin, Xmax: tpl.Xmax})
				}
			}
		}
	}
}

// Scan returns visible rows and the column schema.
// snap==nil means auto-commit mode: shows tuples where xmin==0 or committed AND xmax==0.
func (db *Database) Scan(tableName string, snap *Snapshot, xid uint64) ([]Row, []Column, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil, nil, fmt.Errorf("table %q does not exist", tableName)
	}
	var rows []Row
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			if db.tupleVisible(tuple, snap, xid) {
				rows = append(rows, tuple.Data)
			}
		}
	}
	return rows, t.Columns, nil
}

// TupleVisible applies MVCC visibility rules to a single tuple.
// Exported for use by volcano nodes (e.g., index scan).
func (db *Database) TupleVisible(tuple Tuple, snap *Snapshot, xid uint64) bool {
	return db.tupleVisible(tuple, snap, xid)
}

// tupleVisible applies MVCC visibility rules to a single tuple.
func (db *Database) tupleVisible(tuple Tuple, snap *Snapshot, xid uint64) bool {
	if snap == nil {
		// Auto-commit mode: show tuples whose inserting tx is committed (or legacy xmin=0)
		// AND whose deletion (if any) has not yet committed.
		xminOK := tuple.Xmin == 0 || db.TxManager.IsCommitted(tuple.Xmin)
		if !xminOK {
			return false
		}
		// Tuple is live if not deleted, or deletion not yet committed
		xmaxDead := tuple.Xmax != 0 && db.TxManager.IsCommitted(tuple.Xmax)
		return !xmaxDead
	}
	return Visible(tuple.Xmin, tuple.Xmax, *snap, xid, db.TxManager)
}

// ScanPages returns visible tuples with physical locations, plus total page count.
// snap==nil means auto-commit mode (same visibility rules as Scan).
func (db *Database) ScanPages(tableName string, snap *Snapshot, xid uint64) ([]Tuple, []Column, int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil, nil, 0, fmt.Errorf("table %q does not exist", tableName)
	}
	var tuples []Tuple
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			if db.tupleVisible(tuple, snap, xid) {
				tuples = append(tuples, tuple)
			}
		}
	}
	return tuples, t.Columns, len(t.Pages), nil
}

func (db *Database) RowCount(tableName string) (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, fmt.Errorf("table %q does not exist", tableName)
	}
	count := 0
	for _, pg := range t.Pages {
		count += len(pg.Tuples)
	}
	return count, nil
}

func (db *Database) PageCount(tableName string) (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, fmt.Errorf("table %q does not exist", tableName)
	}
	return len(t.Pages), nil
}

// UpdateRows implements MVCC-style update: marks matching tuples dead (Xmax=xid)
// and inserts new tuple versions with Xmin=xid.
// xid==0 falls back to in-place mutation for auto-commit mode.
// Returns (count, oldRows, newRows, error) for WAL logging by the caller.
func (db *Database) UpdateRows(tableName string, predicate func(Row) bool, update func(Row) Row, xid uint64) (int, []Row, []Row, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, nil, nil, fmt.Errorf("table %q does not exist", tableName)
	}
	count := 0
	var oldRows []Row
	var newRows []Row
	for i := range t.Pages {
		for j := range t.Pages[i].Tuples {
			tpl := &t.Pages[i].Tuples[j]
			if tpl.Xmax != 0 {
				continue // already dead
			}
			if !predicate(tpl.Data) {
				continue
			}
			oldCopy := make(Row, len(tpl.Data))
			for k, v := range tpl.Data {
				oldCopy[k] = v
			}
			newRow := update(tpl.Data)
			if xid == 0 {
				// Auto-commit: mutate in place (legacy behaviour)
				tpl.Data = newRow
			} else {
				// MVCC: stamp old tuple as deleted, queue new version
				tpl.Xmax = xid
				newRows = append(newRows, newRow)
			}
			oldRows = append(oldRows, oldCopy)
			count++
		}
	}
	// Append new tuple versions for MVCC updates
	for _, row := range newRows {
		if len(t.Pages) > 0 {
			last := &t.Pages[len(t.Pages)-1]
			if len(last.Tuples) < t.tuplesPerPage {
				slotNum := len(last.Tuples)
				pageNum := len(t.Pages) - 1
				last.Tuples = append(last.Tuples, Tuple{PageNum: pageNum, SlotNum: slotNum, Data: row, Xmin: xid})
				continue
			}
		}
		pageNum := len(t.Pages)
		pg := Page{Tuples: []Tuple{{PageNum: pageNum, SlotNum: 0, Data: row, Xmin: xid}}}
		t.Pages = append(t.Pages, pg)
	}
	t.rebuildIndexes()
	if xid == 0 {
		// For auto-commit, newRows == oldRows (in-place), return the updated rows
		newRows = oldRows
	}
	return count, oldRows, newRows, nil
}

type TableInfo struct {
	Name     string
	Columns  []Column
	RowCount int
}

func (db *Database) ListTables() []TableInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var tables []TableInfo
	for _, t := range db.Tables {
		rowCount := 0
		for _, pg := range t.Pages {
			rowCount += len(pg.Tuples)
		}
		tables = append(tables, TableInfo{
			Name:     t.Name,
			Columns:  t.Columns,
			RowCount: rowCount,
		})
	}
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})
	return tables
}

func (db *Database) CreateIndex(indexName, tableName, column string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %q does not exist", tableName)
	}
	colExists := false
	for _, c := range t.Columns {
		if c.Name == column {
			colExists = true
			break
		}
	}
	if !colExists {
		return fmt.Errorf("column %q does not exist in table %q", column, tableName)
	}
	if _, exists := t.Indexes[indexName]; exists {
		return fmt.Errorf("index %q already exists", indexName)
	}
	tree := NewBTree()
	for pageNum, pg := range t.Pages {
		for slotNum, tpl := range pg.Tuples {
			if val, ok := tpl.Data[column]; ok {
				tree.Insert(val, Tuple{PageNum: pageNum, SlotNum: slotNum, Data: tpl.Data})
			}
		}
	}
	t.Indexes[indexName] = &Index{Name: indexName, Column: column, Tree: tree}
	return nil
}

func (db *Database) DropIndex(indexName, tableName string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %q does not exist", tableName)
	}
	if _, exists := t.Indexes[indexName]; !exists {
		return fmt.Errorf("index %q does not exist on table %q", indexName, tableName)
	}
	delete(t.Indexes, indexName)
	return nil
}

func (db *Database) DropIndexByName(indexName string, ifExists bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, t := range db.Tables {
		if _, ok := t.Indexes[indexName]; ok {
			delete(t.Indexes, indexName)
			return nil
		}
	}
	if ifExists {
		return nil
	}
	return fmt.Errorf("index %q does not exist", indexName)
}

func (db *Database) FindIndexForColumn(tableName, column string) (string, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return "", false
	}
	for name, idx := range t.Indexes {
		if idx.Column == column {
			return name, true
		}
	}
	return "", false
}

func (db *Database) IndexRangeScan(tableName, indexName string, lo interface{}, loOp string, hi interface{}, hiOp string) ([]Tuple, int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil, 0, fmt.Errorf("table %q does not exist", tableName)
	}
	idx, ok := t.Indexes[indexName]
	if !ok {
		return nil, 0, fmt.Errorf("index %q does not exist on table %q", indexName, tableName)
	}
	depth := idx.Tree.Depth()
	var tuples []Tuple
	if lo == nil && hi == nil {
		tuples = idx.Tree.All()
	} else {
		tuples = idx.Tree.RangeScan(lo, loOp, hi, hiOp)
	}
	return tuples, depth, nil
}

func (db *Database) GetIndexDepth(tableName, indexName string) int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 1
	}
	idx, ok := t.Indexes[indexName]
	if !ok {
		return 1
	}
	return idx.Tree.Depth()
}

func (db *Database) ListIndexesForTable(tableName string) []IndexInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil
	}
	var infos []IndexInfo
	for name, idx := range t.Indexes {
		infos = append(infos, IndexInfo{Name: name, Column: idx.Column, Size: idx.Tree.Size})
	}
	return infos
}

// AnalyzeTable computes statistics for all columns in the named table (like PostgreSQL ANALYZE).
func (db *Database) AnalyzeTable(name string) ([]string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[name]
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", name)
	}
	t.Stats = computeStats(t)
	return t.Stats.FormatAnalyzeOutput(name, t.Columns), nil
}

// GetTableStats returns the most recent ANALYZE results for a table, or nil if never analyzed.
func (db *Database) GetTableStats(name string) *TableStats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[name]
	if !ok {
		return nil
	}
	return t.Stats
}

// DeleteRows implements MVCC-style delete: stamps matching live tuples with Xmax=xid.
// When xid==0 (auto-commit), physically removes rows (legacy behaviour).
// Returns (count, deletedRows, error) for WAL logging by the caller.
func (db *Database) DeleteRows(tableName string, predicate func(Row) bool, xid uint64) (int, []Row, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, nil, fmt.Errorf("table %q does not exist", tableName)
	}

	// Check FK: no child table may reference rows we're about to delete
	for childName, child := range db.Tables {
		for _, fk := range child.ForeignKeys {
			if fk.RefTable != tableName {
				continue
			}
			for _, pg := range t.Pages {
				for _, tpl := range pg.Tuples {
					if tpl.Xmax != 0 || !predicate(tpl.Data) {
						continue
					}
					val := tpl.Data[fk.RefColumn]
					for _, cpg := range child.Pages {
						for _, ctpl := range cpg.Tuples {
							if ctpl.Xmax != 0 {
								continue
							}
							if ctpl.Data[fk.Column] == val {
								return 0, nil, fmt.Errorf("delete violates foreign key constraint: %q.%q=%v is referenced by %q.%q", tableName, fk.RefColumn, val, childName, fk.Column)
							}
						}
					}
				}
			}
		}
	}

	deleted := 0
	var deletedRows []Row

	if xid != 0 {
		// MVCC: mark matching live tuples as deleted
		for i := range t.Pages {
			for j := range t.Pages[i].Tuples {
				tpl := &t.Pages[i].Tuples[j]
				if tpl.Xmax != 0 {
					continue // already dead
				}
				if predicate(tpl.Data) {
					rowCopy := make(Row, len(tpl.Data))
					for k, v := range tpl.Data {
						rowCopy[k] = v
					}
					deletedRows = append(deletedRows, rowCopy)
					tpl.Xmax = xid
					deleted++
				}
			}
		}
		t.rebuildIndexes()
		return deleted, deletedRows, nil
	}

	// Auto-commit (xid==0): physical removal for backward compat
	var kept []Row
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			if tuple.Xmax != 0 {
				continue // skip already-dead tuples
			}
			if predicate(tuple.Data) {
				rowCopy := make(Row, len(tuple.Data))
				for k, v := range tuple.Data {
					rowCopy[k] = v
				}
				deletedRows = append(deletedRows, rowCopy)
				deleted++
			} else {
				kept = append(kept, tuple.Data)
			}
		}
	}
	// Rebuild pages from kept rows.
	t.Pages = nil
	for pageNum := 0; len(kept) > 0; pageNum++ {
		end := t.tuplesPerPage
		if end > len(kept) {
			end = len(kept)
		}
		pg := Page{}
		for i, row := range kept[:end] {
			pg.Tuples = append(pg.Tuples, Tuple{PageNum: pageNum, SlotNum: i, Data: row})
		}
		t.Pages = append(t.Pages, pg)
		kept = kept[end:]
	}
	t.rebuildIndexes()
	return deleted, deletedRows, nil
}

// Vacuum physically removes tuples that are dead (Xmax != 0) and whose deleting
// transaction has committed. Returns the number of tuples reclaimed.
func (db *Database) Vacuum(tableName string) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, fmt.Errorf("table %q does not exist", tableName)
	}
	var kept []Tuple
	reclaimed := 0
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			dead := tuple.Xmax != 0 && db.TxManager.IsCommitted(tuple.Xmax)
			if dead {
				reclaimed++
			} else {
				kept = append(kept, tuple)
			}
		}
	}
	// Rebuild pages
	t.Pages = nil
	for pageNum := 0; len(kept) > 0; pageNum++ {
		end := t.tuplesPerPage
		if end > len(kept) {
			end = len(kept)
		}
		pg := Page{}
		for i, tpl := range kept[:end] {
			pg.Tuples = append(pg.Tuples, Tuple{
				PageNum: pageNum, SlotNum: i,
				Data: tpl.Data, Xmin: tpl.Xmin, Xmax: tpl.Xmax,
			})
		}
		t.Pages = append(t.Pages, pg)
		kept = kept[end:]
	}
	t.rebuildIndexes()
	return reclaimed, nil
}
