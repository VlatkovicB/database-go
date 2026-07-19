package storage

import "fmt"

const (
	PageSize        = 8192
	PageHeaderSize  = 24
	TupleHeaderSize = 23
)

// Tuple is a stored row with its physical location identifier.
type Tuple struct {
	PageNum int
	SlotNum int
	Data    Row
}

// CTID returns the PostgreSQL-style tuple identifier, e.g. "(0,1)".
// PG uses 1-based slot numbers.
func (t Tuple) CTID() string {
	return fmt.Sprintf("(%d,%d)", t.PageNum, t.SlotNum+1)
}

// Page simulates one 8KB PostgreSQL heap page.
type Page struct {
	Tuples []Tuple
}

// estimateTupleSize estimates bytes per tuple from column types.
// Uses TupleHeaderSize (23 bytes, like PG's HeapTupleHeaderData) plus column data.
func estimateTupleSize(cols []Column) int {
	size := TupleHeaderSize
	for _, c := range cols {
		switch c.Type {
		case TypeInt:
			size += 8
		case TypeFloat:
			size += 8
		case TypeBoolean:
			size += 1
		case TypeText:
			size += 50 // average estimate
		default:
			size += 8
		}
	}
	return size
}

// TuplesPerPage returns how many tuples fit on one 8KB page given the table's columns.
func TuplesPerPage(cols []Column) int {
	usable := PageSize - PageHeaderSize
	ts := estimateTupleSize(cols)
	if ts <= 0 {
		return 100
	}
	n := usable / ts
	if n < 1 {
		return 1
	}
	return n
}
