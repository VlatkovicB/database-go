package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"strings"
)

type Result struct {
	Columns []string
	Rows    [][]interface{}
	Message string
	Trace   []string
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
	if len(s.Joins) > 0 || s.Alias != s.Table {
		return e.execSelectJoin(s)
	}
	return e.execSelectSingle(s)
}

// execSelectSingle handles the original single-table path (no joins, no alias).
func (e *Executor) execSelectSingle(s *parser.SelectStatement) (*Result, error) {
	rows, cols, err := e.db.Scan(s.Table)
	if err != nil {
		return nil, err
	}

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

	var colNames []string
	if s.Columns == nil {
		for _, c := range cols {
			colNames = append(colNames, c.Name)
		}
	} else {
		colNames = s.Columns
	}

	whereDesc := "none"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	colDesc := "*"
	if s.Columns != nil {
		colDesc = fmt.Sprintf("[%s]", joinStrings(colNames))
	}

	result := &Result{
		Columns: colNames,
		Rows:    [][]interface{}{},
		Trace: []string{
			fmt.Sprintf("Full table scan on %q — %d row(s) read", s.Table, len(rows)),
			fmt.Sprintf("WHERE %s — %d of %d row(s) passed", whereDesc, len(filtered), len(rows)),
			fmt.Sprintf("Project columns: %s", colDesc),
		},
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

// projCol maps a combined-row key to an output column name.
type projCol struct {
	key  string
	name string
}

// execSelectJoin handles multi-table joins and aliased single-table queries.
func (e *Executor) execSelectJoin(s *parser.SelectStatement) (*Result, error) {
	// 1. Scan base table, tag rows with alias.
	baseRows, baseCols, err := e.db.Scan(s.Table)
	if err != nil {
		return nil, err
	}
	alias := s.Alias
	if alias == "" {
		alias = s.Table
	}

	combined := make([]storage.Row, len(baseRows))
	for i, row := range baseRows {
		cr := make(storage.Row)
		for k, v := range row {
			cr[alias+"."+k] = v
		}
		combined[i] = cr
	}

	// Track schema order per alias for projection.
	aliasOrder := []string{alias}
	aliasSchema := map[string][]storage.Column{alias: baseCols}

	// 2. Nested-loop join for each JOIN clause.
	for _, join := range s.Joins {
		joinRows, joinCols, err := e.db.Scan(join.Table)
		if err != nil {
			return nil, err
		}
		ja := join.Alias
		if ja == "" {
			ja = join.Table
		}
		aliasOrder = append(aliasOrder, ja)
		aliasSchema[ja] = joinCols

		var next []storage.Row
		for _, cr := range combined {
			matched := false
			for _, jr := range joinRows {
				candidate := make(storage.Row)
				for k, v := range cr {
					candidate[k] = v
				}
				for k, v := range jr {
					candidate[ja+"."+k] = v
				}
				ok, err := evalExpr(join.Condition, candidate)
				if err != nil {
					return nil, err
				}
				if !boolVal(ok) {
					continue
				}
				matched = true
				next = append(next, candidate)
			}
			// LEFT JOIN: include unmatched base rows with NULLs.
			if join.Type == parser.LeftJoin && !matched {
				candidate := make(storage.Row)
				for k, v := range cr {
					candidate[k] = v
				}
				for _, c := range joinCols {
					candidate[ja+"."+c.Name] = nil
				}
				next = append(next, candidate)
			}
		}
		combined = next
	}

	// 3. Apply WHERE on combined rows.
	var filtered []storage.Row
	for _, cr := range combined {
		if s.Where == nil {
			filtered = append(filtered, cr)
			continue
		}
		match, err := evalExpr(s.Where, cr)
		if err != nil {
			return nil, err
		}
		if boolVal(match) {
			filtered = append(filtered, cr)
		}
	}

	// 4. Resolve projection columns.
	var proj []projCol
	if s.Columns == nil {
		// SELECT * — all columns in schema order across all aliases.
		for _, a := range aliasOrder {
			for _, c := range aliasSchema[a] {
				proj = append(proj, projCol{key: a + "." + c.Name, name: a + "." + c.Name})
			}
		}
	} else {
		for _, col := range s.Columns {
			if strings.HasSuffix(col, ".*") {
				// alias.* — expand to all columns from that alias.
				a := strings.TrimSuffix(col, ".*")
				schema, ok := aliasSchema[a]
				if !ok {
					return nil, fmt.Errorf("alias %q not found in query", a)
				}
				for _, c := range schema {
					proj = append(proj, projCol{key: a + "." + c.Name, name: c.Name})
				}
			} else if idx := strings.Index(col, "."); idx >= 0 {
				// alias.col
				proj = append(proj, projCol{key: col, name: col[idx+1:]})
			} else {
				// bare col — find which alias owns it.
				found := false
				for _, a := range aliasOrder {
					for _, c := range aliasSchema[a] {
						if c.Name == col {
							proj = append(proj, projCol{key: a + "." + col, name: col})
							found = true
							break
						}
					}
					if found {
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("column %q not found in any joined table", col)
				}
			}
		}
	}

	// 5. Build result.
	colNames := make([]string, len(proj))
	for i, p := range proj {
		colNames[i] = p.name
	}

	joinDesc := make([]string, len(s.Joins))
	for i, j := range s.Joins {
		joinDesc[i] = fmt.Sprintf("%s JOIN %q AS %q ON %s", j.Type, j.Table, j.Alias, ExprString(j.Condition))
	}
	whereDesc := "none"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}

	trace := []string{
		fmt.Sprintf("Scan %q AS %q — %d row(s)", s.Table, alias, len(baseRows)),
	}
	for _, jd := range joinDesc {
		trace = append(trace, jd)
	}
	trace = append(trace,
		fmt.Sprintf("Combined rows after joins: %d", len(combined)),
		fmt.Sprintf("WHERE %s — %d of %d row(s) passed", whereDesc, len(filtered), len(combined)),
		fmt.Sprintf("Project %d column(s)", len(proj)),
	)

	result := &Result{Columns: colNames, Rows: [][]interface{}{}, Trace: trace}
	for _, row := range filtered {
		r := make([]interface{}, len(proj))
		for i, p := range proj {
			r[i] = row[p.key]
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
	return &Result{
		Message: "1 row inserted",
		Trace: []string{
			fmt.Sprintf("Build row from %d value(s)", len(s.Values)),
			fmt.Sprintf("Append row to %q", s.Table),
		},
	}, nil
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
	whereDesc := "none (all rows)"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	return &Result{
		Message: fmt.Sprintf("%d row(s) updated", count),
		Trace: []string{
			fmt.Sprintf("Scan %q for rows matching WHERE %s", s.Table, whereDesc),
			fmt.Sprintf("Apply %d assignment(s) to %d matching row(s)", len(s.Assignments), count),
		},
	}, nil
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
	whereDesc := "none (all rows)"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	return &Result{
		Message: fmt.Sprintf("%d row(s) deleted", count),
		Trace: []string{
			fmt.Sprintf("Scan %q for rows matching WHERE %s", s.Table, whereDesc),
			fmt.Sprintf("Remove %d row(s), keep remainder", count),
		},
	}, nil
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
	return &Result{
		Message: fmt.Sprintf("table %q created", s.Table),
		Trace: []string{
			fmt.Sprintf("Validate %d column definition(s)", len(cols)),
			fmt.Sprintf("Register %q in database catalog", s.Table),
			"Initialize empty row storage",
		},
	}, nil
}

func (e *Executor) execDrop(s *parser.DropTableStatement) (*Result, error) {
	if err := e.db.DropTable(s.Table); err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("table %q dropped", s.Table),
		Trace: []string{
			fmt.Sprintf("Verify %q exists in catalog", s.Table),
			fmt.Sprintf("Remove %q and all its rows", s.Table),
		},
	}, nil
}

// evalExpr evaluates an expression against a row and returns the result value.
func evalExpr(expr parser.Expression, row storage.Row) (interface{}, error) {
	switch e := expr.(type) {
	case *parser.IdentExpr:
		if e.Table != "" {
			// Qualified: alias.col
			key := e.Table + "." + e.Name
			if val, ok := row[key]; ok {
				return val, nil
			}
			return nil, fmt.Errorf("column %q.%q not found", e.Table, e.Name)
		}
		// Unqualified: try bare key first (single-table), then suffix search (join).
		if val, ok := row[e.Name]; ok {
			return val, nil
		}
		suffix := "." + e.Name
		for k, v := range row {
			if strings.HasSuffix(k, suffix) {
				return v, nil
			}
		}
		return nil, fmt.Errorf("column %q not found", e.Name)
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

func ExprString(expr parser.Expression) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *parser.BinaryExpr:
		return ExprString(e.Left) + " " + e.Op + " " + ExprString(e.Right)
	case *parser.IdentExpr:
		if e.Table != "" {
			return e.Table + "." + e.Name
		}
		return e.Name
	case *parser.LiteralExpr:
		if e.Value == nil {
			return "NULL"
		}
		return fmt.Sprintf("%v", e.Value)
	}
	return "?"
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
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
