package storage

import "fmt"

// checkPrimaryKey verifies that the new row does not violate any PRIMARY KEY constraint on t.
// Returns non-nil error if a duplicate or NULL PK value is found.
func checkPrimaryKey(t *Table, row Row) error {
	for _, col := range t.Columns {
		if !col.Primary {
			continue
		}
		pkVal, hasPK := row[col.Name]
		if !hasPK || pkVal == nil {
			return fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
		}
		var dup bool
		t.ScanTuples(func(tpl Tuple) bool {
			if tpl.Xmax != 0 {
				return true
			}
			if tpl.Data[col.Name] == pkVal {
				dup = true
				return false
			}
			return true
		})
		if dup {
			return fmt.Errorf("duplicate key value violates unique constraint on %q: %q=%v already exists", t.Name, col.Name, pkVal)
		}
	}
	return nil
}

// checkForeignKeys verifies that the new row satisfies all FK constraints on t.
// lookup must return the referenced table (or nil if not found).
func checkForeignKeys(t *Table, row Row, lookup func(name string) *Table) error {
	for _, fk := range t.ForeignKeys {
		val := row[fk.Column]
		if val == nil {
			continue // NULL is allowed in FK columns
		}
		refT := lookup(fk.RefTable)
		if refT == nil {
			return fmt.Errorf("foreign key references unknown table %q", fk.RefTable)
		}
		found := false
		refT.ScanTuples(func(tpl Tuple) bool {
			if tpl.Xmax != 0 {
				return true
			}
			if tpl.Data[fk.RefColumn] == val {
				found = true
				return false
			}
			return true
		})
		if !found {
			return fmt.Errorf("insert violates foreign key constraint: %q.%q=%v has no match in %q.%q",
				t.Name, fk.Column, val, fk.RefTable, fk.RefColumn)
		}
	}
	return nil
}

// checkFKRestrict verifies that no child table references rows in parentTable that match predicate.
// tables is the full set of DB tables. Returns an error if any referencing row is found.
func checkFKRestrict(parentTable *Table, predicate func(Row) bool, tables map[string]*Table) error {
	for childName, child := range tables {
		for _, fk := range child.ForeignKeys {
			if fk.RefTable != parentTable.Name {
				continue
			}
			var violation error
			parentTable.ScanTuples(func(tpl Tuple) bool {
				if tpl.Xmax != 0 || !predicate(tpl.Data) {
					return true
				}
				val := tpl.Data[fk.RefColumn]
				child.ScanTuples(func(ctpl Tuple) bool {
					if ctpl.Xmax != 0 {
						return true
					}
					if ctpl.Data[fk.Column] == val {
						violation = fmt.Errorf(
							"delete violates foreign key constraint: %q.%q=%v is referenced by %q.%q",
							parentTable.Name, fk.RefColumn, val, childName, fk.Column,
						)
						return false
					}
					return true
				})
				return violation == nil
			})
			if violation != nil {
				return violation
			}
		}
	}
	return nil
}
