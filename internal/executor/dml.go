package executor

// =============================================================================
// DML statements
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
)

func (e *Executor) execInsert(s *parser.InsertStatement) (*Result, error) {
	tbl, err := e.db.GetTable(s.Table)
	if err != nil {
		return nil, err
	}

	row := make(storage.Row)
	if len(s.Columns) == 0 {
		if len(s.Values) != len(tbl.Columns) {
			return nil, fmt.Errorf(
				"column count mismatch: got %d values but table %q has %d columns",
				len(s.Values), s.Table, len(tbl.Columns),
			)
		}
		for i, col := range tbl.Columns {
			row[col.Name] = s.Values[i]
		}
	} else {
		if len(s.Columns) != len(s.Values) {
			return nil, fmt.Errorf("column/value count mismatch: %d columns, %d values", len(s.Columns), len(s.Values))
		}
		for i, col := range s.Columns {
			row[col] = s.Values[i]
		}
	}

	xid := e.currentXID()
	if err := e.db.Insert(s.Table, row, xid); err != nil {
		return nil, err
	}
	e.db.WAL.Append(xid, storage.WALInsert, s.Table, nil, []storage.Row{row})
	traceXmin := "xmin=0 (auto-commit)"
	if xid != 0 {
		traceXmin = fmt.Sprintf("xmin=%d", xid)
	}
	return &Result{
		Message: "1 row inserted",
		Trace: []string{
			fmt.Sprintf("Build row from %d value(s)", len(s.Values)),
			fmt.Sprintf("Append tuple to %q (%s xmax=0)", s.Table, traceXmin),
		},
	}, nil
}

func (e *Executor) execUpdate(s *parser.UpdateStatement) (*Result, error) {
	xid := e.currentXID()
	count, oldRows, newRows, err := e.db.UpdateRows(s.Table,
		func(row storage.Row) bool {
			if s.Where == nil {
				return true
			}
			match, _ := evalExpr(s.Where, row)
			return boolVal(match)
		},
		func(row storage.Row) storage.Row {
			newRow := make(storage.Row, len(row))
			for k, v := range row {
				newRow[k] = v
			}
			for k, v := range s.Assignments {
				newRow[k] = v
			}
			return newRow
		},
		xid,
	)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		e.db.WAL.Append(xid, storage.WALUpdate, s.Table, oldRows, newRows)
	}
	whereDesc := "none (all rows)"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	trace := []string{
		fmt.Sprintf("Scan %q for rows matching WHERE %s", s.Table, whereDesc),
		fmt.Sprintf("Apply %d assignment(s) to %d matching row(s)", len(s.Assignments), count),
	}
	if xid != 0 {
		trace = append(trace,
			fmt.Sprintf("MVCC: stamped %d old tuple(s) xmax=%d", count, xid),
			fmt.Sprintf("MVCC: inserted %d new tuple version(s) xmin=%d", count, xid),
		)
	}
	return &Result{
		Message: fmt.Sprintf("%d row(s) updated", count),
		Trace:   trace,
	}, nil
}

func (e *Executor) execDelete(s *parser.DeleteStatement) (*Result, error) {
	xid := e.currentXID()
	count, deletedRows, err := e.db.DeleteRows(s.Table, func(row storage.Row) bool {
		if s.Where == nil {
			return true
		}
		match, _ := evalExpr(s.Where, row)
		return boolVal(match)
	}, xid)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		e.db.WAL.Append(xid, storage.WALDelete, s.Table, deletedRows, nil)
	}
	whereDesc := "none (all rows)"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	trace := []string{
		fmt.Sprintf("Scan %q for rows matching WHERE %s", s.Table, whereDesc),
	}
	if xid != 0 {
		trace = append(trace, fmt.Sprintf("MVCC: stamped %d tuple(s) xmax=%d (soft-delete)", count, xid))
	} else {
		trace = append(trace, fmt.Sprintf("Remove %d row(s), keep remainder", count))
	}
	return &Result{
		Message: fmt.Sprintf("%d row(s) deleted", count),
		Trace:   trace,
	}, nil
}
