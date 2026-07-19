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

type Table struct {
	Name          string
	Columns       []Column
	Pages         []Page
	tuplesPerPage int
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
	if len(t.Pages) > 0 {
		last := &t.Pages[len(t.Pages)-1]
		if len(last.Tuples) < t.tuplesPerPage {
			slotNum := len(last.Tuples)
			pageNum := len(t.Pages) - 1
			last.Tuples = append(last.Tuples, Tuple{PageNum: pageNum, SlotNum: slotNum, Data: row})
			return nil
		}
	}
	pageNum := len(t.Pages)
	pg := Page{Tuples: []Tuple{{PageNum: pageNum, SlotNum: 0, Data: row}}}
	t.Pages = append(t.Pages, pg)
	return nil
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
	return deleted, nil
}
