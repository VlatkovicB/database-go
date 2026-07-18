package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
)

type Result struct {
	Columns []string
	Rows    [][]interface{}
	Message string
}

type Executor struct {
	db *storage.Database
}

func New(db *storage.Database) *Executor {
	return &Executor{db: db}
}

func (e *Executor) Execute(stmt parser.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *parser.SelectStatement:
		return e.execSelect(s)
	case *parser.InsertStatement:
		return e.execInsert(s)
	case *parser.UpdateStatement:
		return e.execUpdate(s)
	case *parser.DeleteStatement:
		return e.execDelete(s)
	case *parser.CreateTableStatement:
		return e.execCreate(s)
	case *parser.DropTableStatement:
		return e.execDrop(s)
	default:
		return nil, fmt.Errorf("unknown statement type")
	}
}

func (e *Executor) execSelect(s *parser.SelectStatement) (*Result, error) {
	rows, cols, err := e.db.Scan(s.Table)
	if err != nil {
		return nil, err
	}

	// Filter rows using the WHERE clause.
	var filtered []storage.Row
	for _, row := range rows {
		if s.Where == nil {
			filtered = append(filtered, row)
			continue
		}
		match, err := evalExpr(s.Where, row)
		if err != nil {
			return nil, err
		}
		if boolVal(match) {
			filtered = append(filtered, row)
		}
	}

	// Determine which columns to project.
	var colNames []string
	if s.Columns == nil {
		for _, c := range cols {
			colNames = append(colNames, c.Name)
		}
	} else {
		colNames = s.Columns
	}

	result := &Result{
		Columns: colNames,
		Rows:    [][]interface{}{},
	}
	for _, row := range filtered {
		r := make([]interface{}, len(colNames))
		for i, col := range colNames {
			r[i] = row[col]
		}
		result.Rows = append(result.Rows, r)
	}
	return result, nil
}

func (e *Executor) execInsert(s *parser.InsertStatement) (*Result, error) {
	tbl, err := e.db.GetTable(s.Table)
	if err != nil {
		return nil, err
	}

	row := make(storage.Row)
	if len(s.Columns) == 0 {
		// Positional insert: values must match table column order.
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

	if err := e.db.Insert(s.Table, row); err != nil {
		return nil, err
	}
	return &Result{Message: "1 row inserted"}, nil
}

func (e *Executor) execUpdate(s *parser.UpdateStatement) (*Result, error) {
	count, err := e.db.UpdateRows(s.Table,
		func(row storage.Row) bool {
			if s.Where == nil {
				return true
			}
			match, _ := evalExpr(s.Where, row)
			return boolVal(match)
		},
		func(row storage.Row) {
			for k, v := range s.Assignments {
				row[k] = v
			}
		},
	)
	if err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("%d row(s) updated", count)}, nil
}

func (e *Executor) execDelete(s *parser.DeleteStatement) (*Result, error) {
	count, err := e.db.DeleteRows(s.Table, func(row storage.Row) bool {
		if s.Where == nil {
			return true
		}
		match, _ := evalExpr(s.Where, row)
		return boolVal(match)
	})
	if err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("%d row(s) deleted", count)}, nil
}

func (e *Executor) execCreate(s *parser.CreateTableStatement) (*Result, error) {
	var cols []storage.Column
	for _, cd := range s.Columns {
		cols = append(cols, storage.Column{
			Name:    cd.Name,
			Type:    storage.ColumnType(cd.Type),
			Primary: cd.Primary,
		})
	}
	if err := e.db.CreateTable(s.Table, cols); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("table %q created", s.Table)}, nil
}

func (e *Executor) execDrop(s *parser.DropTableStatement) (*Result, error) {
	if err := e.db.DropTable(s.Table); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("table %q dropped", s.Table)}, nil
}

// evalExpr evaluates an expression against a row and returns the result value.
func evalExpr(expr parser.Expression, row storage.Row) (interface{}, error) {
	switch e := expr.(type) {
	case *parser.IdentExpr:
		val, ok := row[e.Name]
		if !ok {
			return nil, fmt.Errorf("column %q not found", e.Name)
		}
		return val, nil
	case *parser.LiteralExpr:
		return e.Value, nil
	case *parser.BinaryExpr:
		return evalBinary(e, row)
	}
	return nil, fmt.Errorf("unknown expression type %T", expr)
}

func evalBinary(e *parser.BinaryExpr, row storage.Row) (interface{}, error) {
	// Short-circuit AND/OR before evaluating both sides.
	if e.Op == "AND" {
		left, err := evalExpr(e.Left, row)
		if err != nil {
			return nil, err
		}
		if !boolVal(left) {
			return false, nil
		}
		return evalExpr(e.Right, row)
	}
	if e.Op == "OR" {
		left, err := evalExpr(e.Left, row)
		if err != nil {
			return nil, err
		}
		if boolVal(left) {
			return true, nil
		}
		return evalExpr(e.Right, row)
	}

	left, err := evalExpr(e.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := evalExpr(e.Right, row)
	if err != nil {
		return nil, err
	}

	return compare(left, e.Op, right)
}

func compare(left interface{}, op string, right interface{}) (bool, error) {
	// Numeric path: coerce both sides to float64 if possible.
	lf, lok := toFloat(left)
	rf, rok := toFloat(right)
	if lok && rok {
		switch op {
		case "=":
			return lf == rf, nil
		case "!=":
			return lf != rf, nil
		case "<":
			return lf < rf, nil
		case ">":
			return lf > rf, nil
		case "<=":
			return lf <= rf, nil
		case ">=":
			return lf >= rf, nil
		}
	}

	// String/equality fallback.
	ls := fmt.Sprintf("%v", left)
	rs := fmt.Sprintf("%v", right)
	switch op {
	case "=":
		return ls == rs, nil
	case "!=":
		return ls != rs, nil
	}

	return false, fmt.Errorf("cannot apply operator %q to types %T and %T", op, left, right)
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func boolVal(v interface{}) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}
