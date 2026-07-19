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
	Pages         []Page
	tuplesPerPage int
	Indexes       map[string]*Index
}

type Database struct {
	mu     sync.RWMutex
	Tables map[string]*Table
}

func New() *Database {
	return &Database{Tables: make(map[string]*Table)}
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

func (db *Database) Insert(tableName string, row Row) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %q does not exist", tableName)
	}
	var inserted Tuple
	if len(t.Pages) > 0 {
		last := &t.Pages[len(t.Pages)-1]
		if len(last.Tuples) < t.tuplesPerPage {
			slotNum := len(last.Tuples)
			pageNum := len(t.Pages) - 1
			inserted = Tuple{PageNum: pageNum, SlotNum: slotNum, Data: row}
			last.Tuples = append(last.Tuples, inserted)
			t.updateIndexes(inserted)
			return nil
		}
	}
	pageNum := len(t.Pages)
	inserted = Tuple{PageNum: pageNum, SlotNum: 0, Data: row}
	pg := Page{Tuples: []Tuple{inserted}}
	t.Pages = append(t.Pages, pg)
	t.updateIndexes(inserted)
	return nil
}

func (t *Table) updateIndexes(tuple Tuple) {
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
				if val, ok := tpl.Data[idx.Column]; ok {
					idx.Tree.Insert(val, Tuple{PageNum: pageNum, SlotNum: slotNum, Data: tpl.Data})
				}
			}
		}
	}
}

// Scan returns a snapshot of all rows and the column schema.
func (db *Database) Scan(tableName string) ([]Row, []Column, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil, nil, fmt.Errorf("table %q does not exist", tableName)
	}
	var rows []Row
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			rows = append(rows, tuple.Data)
		}
	}
	return rows, t.Columns, nil
}

// ScanPages returns all tuples with their physical locations, plus page count.
func (db *Database) ScanPages(tableName string) ([]Tuple, []Column, int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return nil, nil, 0, fmt.Errorf("table %q does not exist", tableName)
	}
	var tuples []Tuple
	for _, pg := range t.Pages {
		tuples = append(tuples, pg.Tuples...)
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

// UpdateRows calls update on every row where predicate returns true.
// Rows are maps (reference types), so mutations are reflected in-place.
func (db *Database) UpdateRows(tableName string, predicate func(Row) bool, update func(Row)) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, fmt.Errorf("table %q does not exist", tableName)
	}
	count := 0
	for i := range t.Pages {
		for j := range t.Pages[i].Tuples {
			if predicate(t.Pages[i].Tuples[j].Data) {
				update(t.Pages[i].Tuples[j].Data)
				count++
			}
		}
	}
	t.rebuildIndexes()
	return count, nil
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

// DeleteRows removes every row where predicate returns true.
func (db *Database) DeleteRows(tableName string, predicate func(Row) bool) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.Tables[tableName]
	if !ok {
		return 0, fmt.Errorf("table %q does not exist", tableName)
	}
	var kept []Row
	deleted := 0
	for _, pg := range t.Pages {
		for _, tuple := range pg.Tuples {
			if predicate(tuple.Data) {
				deleted++
			} else {
				kept = append(kept, tuple.Data)
			}
		}
	}
	// Rebuild pages from kept rows (simulates VACUUM compaction).
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
	return deleted, nil
}
