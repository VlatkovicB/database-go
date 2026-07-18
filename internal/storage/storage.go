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
	Name    string
	Columns []Column
	Rows    []Row
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
	db.Tables[name] = &Table{Name: name, Columns: cols}
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
	t.Rows = append(t.Rows, row)
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
	rows := make([]Row, len(t.Rows))
	copy(rows, t.Rows)
	return rows, t.Columns, nil
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
	for _, row := range t.Rows {
		if predicate(row) {
			update(row)
			count++
		}
	}
	return count, nil
}

type TableInfo struct {
	Name    string
	Columns []Column
	RowCount int
}

func (db *Database) ListTables() []TableInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var tables []TableInfo
	for _, t := range db.Tables {
		tables = append(tables, TableInfo{
			Name:     t.Name,
			Columns:  t.Columns,
			RowCount: len(t.Rows),
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
	for _, row := range t.Rows {
		if predicate(row) {
			deleted++
		} else {
			kept = append(kept, row)
		}
	}
	t.Rows = kept
	return deleted, nil
}
