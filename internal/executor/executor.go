package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"sort"
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
	// Route to group-by engine if aggregates or GROUP BY present.
	if len(s.GroupBy) > 0 || hasAggExprs(s.Exprs) {
		return e.execSelectGroupBy(s)
	}
	if len(s.Joins) > 0 || s.Alias != s.Table {
		return e.execSelectJoin(s)
	}
	return e.execSelectSingle(s)
}

func hasAggExprs(exprs []parser.SelectExpr) bool {
	for _, ex := range exprs {
		if _, ok := ex.(*parser.AggSelectExpr); ok {
			return true
		}
	}
	return false
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
	if s.Exprs == nil {
		for _, c := range cols {
			colNames = append(colNames, c.Name)
		}
	} else {
		for _, expr := range s.Exprs {
			if ex, ok := expr.(*parser.ColSelectExpr); ok {
				colNames = append(colNames, ex.Col)
			}
		}
	}

	whereDesc := "none"
	if s.Where != nil {
		whereDesc = ExprString(s.Where)
	}
	colDesc := "*"
	if s.Exprs != nil {
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
	postProcess(result, s)
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
	if s.Exprs == nil {
		// SELECT * — all columns in schema order across all aliases.
		for _, a := range aliasOrder {
			for _, c := range aliasSchema[a] {
				proj = append(proj, projCol{key: a + "." + c.Name, name: a + "." + c.Name})
			}
		}
	} else {
		for _, expr := range s.Exprs {
			ex, ok := expr.(*parser.ColSelectExpr)
			if !ok {
				return nil, fmt.Errorf("aggregate functions in JOINs require GROUP BY (not yet supported)")
			}
			col := ex.Col
			if strings.HasSuffix(col, ".*") {
				a := strings.TrimSuffix(col, ".*")
				schema, ok := aliasSchema[a]
				if !ok {
					return nil, fmt.Errorf("alias %q not found in query", a)
				}
				for _, c := range schema {
					proj = append(proj, projCol{key: a + "." + c.Name, name: c.Name})
				}
			} else if idx := strings.Index(col, "."); idx >= 0 {
				proj = append(proj, projCol{key: col, name: col[idx+1:]})
			} else {
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
	postProcess(result, s)
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
	case *parser.AggFuncExpr:
		// In HAVING context, pre-computed aggregate values live in the synthetic row.
		arg := "*"
		if e.Arg != nil {
			arg = ExprString(e.Arg)
		}
		key := e.Func + "(" + arg + ")"
		if val, ok := row[key]; ok {
			return val, nil
		}
		return nil, fmt.Errorf("aggregate %s not computed (use in HAVING after GROUP BY)", key)
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
	case *parser.AggFuncExpr:
		arg := "*"
		if e.Arg != nil {
			arg = ExprString(e.Arg)
		}
		return e.Func + "(" + arg + ")"
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

type aggSpec struct{ fn, arg string }

func (e *Executor) execSelectGroupBy(s *parser.SelectStatement) (*Result, error) {
	if len(s.Joins) > 0 {
		return nil, fmt.Errorf("GROUP BY with JOINs not yet supported")
	}

	rows, cols, err := e.db.Scan(s.Table)
	if err != nil {
		return nil, err
	}

	// Apply WHERE.
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

	// Group rows.
	type group struct {
		keyVals map[string]interface{}
		rows    []storage.Row
	}
	var groups []*group
	groupIndex := map[string]int{}

	if len(s.GroupBy) == 0 {
		// No GROUP BY but has aggregates — treat all rows as one group.
		groups = []*group{{keyVals: map[string]interface{}{}, rows: filtered}}
	} else {
		for _, row := range filtered {
			var keyParts []string
			for _, col := range s.GroupBy {
				keyParts = append(keyParts, fmt.Sprintf("%v", row[col]))
			}
			key := strings.Join(keyParts, "\x00")
			if idx, ok := groupIndex[key]; ok {
				groups[idx].rows = append(groups[idx].rows, row)
			} else {
				kv := map[string]interface{}{}
				for _, col := range s.GroupBy {
					kv[col] = row[col]
				}
				groupIndex[key] = len(groups)
				groups = append(groups, &group{keyVals: kv, rows: []storage.Row{row}})
			}
		}
	}

	// Collect all aggregate specs needed (SELECT + HAVING).
	needed := map[string]aggSpec{}
	for _, expr := range s.Exprs {
		if agg, ok := expr.(*parser.AggSelectExpr); ok {
			k := agg.Func + "(" + agg.Arg + ")"
			needed[k] = aggSpec{agg.Func, agg.Arg}
		}
	}
	collectAggFuncs(s.Having, needed)

	// Build synthetic row per group and compute aggregates.
	var synthRows []storage.Row
	for _, g := range groups {
		sr := storage.Row{}
		for k, v := range g.keyVals {
			sr[k] = v
		}
		for key, spec := range needed {
			val, err := computeAgg(spec.fn, spec.arg, g.rows)
			if err != nil {
				return nil, err
			}
			sr[key] = val
		}
		synthRows = append(synthRows, sr)
	}

	// Apply HAVING.
	if s.Having != nil {
		var passed []storage.Row
		for _, sr := range synthRows {
			match, err := evalExpr(s.Having, sr)
			if err != nil {
				return nil, err
			}
			if boolVal(match) {
				passed = append(passed, sr)
			}
		}
		synthRows = passed
	}

	// Project output columns.
	var colNames []string
	if s.Exprs == nil {
		for _, c := range cols {
			colNames = append(colNames, c.Name)
		}
	} else {
		for _, expr := range s.Exprs {
			switch ex := expr.(type) {
			case *parser.ColSelectExpr:
				colNames = append(colNames, ex.Col)
			case *parser.AggSelectExpr:
				colNames = append(colNames, ex.Func+"("+ex.Arg+")")
			}
		}
	}

	result := &Result{Columns: colNames, Rows: [][]interface{}{}}
	for _, sr := range synthRows {
		r := make([]interface{}, len(colNames))
		for i, col := range colNames {
			r[i] = sr[col]
		}
		result.Rows = append(result.Rows, r)
	}
	postProcess(result, s)

	groupByDesc := joinStrings(s.GroupBy)
	if groupByDesc == "" {
		groupByDesc = "(none)"
	}
	havingDesc := "none"
	if s.Having != nil {
		havingDesc = ExprString(s.Having)
	}
	result.Trace = []string{
		fmt.Sprintf("Scan %q — %d row(s)", s.Table, len(rows)),
		fmt.Sprintf("WHERE — %d row(s) after filter", len(filtered)),
		fmt.Sprintf("GROUP BY [%s] — %d group(s) formed", groupByDesc, len(groups)),
		fmt.Sprintf("Compute aggregates: %s", formatAggKeys(needed)),
		fmt.Sprintf("HAVING %s — %d group(s) passed", havingDesc, len(synthRows)),
		fmt.Sprintf("Project %d column(s) → %d row(s)", len(colNames), len(result.Rows)),
	}
	return result, nil
}

func collectAggFuncs(expr parser.Expression, into map[string]aggSpec) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *parser.AggFuncExpr:
		arg := "*"
		if e.Arg != nil {
			arg = ExprString(e.Arg)
		}
		key := e.Func + "(" + arg + ")"
		into[key] = aggSpec{e.Func, arg}
	case *parser.BinaryExpr:
		collectAggFuncs(e.Left, into)
		collectAggFuncs(e.Right, into)
	}
}

func computeAgg(fn, arg string, rows []storage.Row) (interface{}, error) {
	switch fn {
	case "COUNT":
		if arg == "*" {
			return int64(len(rows)), nil
		}
		var count int64
		for _, row := range rows {
			if row[arg] != nil {
				count++
			}
		}
		return count, nil
	case "SUM":
		var sum float64
		for _, row := range rows {
			f, ok := toFloat(row[arg])
			if !ok {
				return nil, fmt.Errorf("SUM: column %q is not numeric", arg)
			}
			sum += f
		}
		return sum, nil
	case "AVG":
		if len(rows) == 0 {
			return nil, nil
		}
		var sum float64
		for _, row := range rows {
			f, ok := toFloat(row[arg])
			if !ok {
				return nil, fmt.Errorf("AVG: column %q is not numeric", arg)
			}
			sum += f
		}
		return sum / float64(len(rows)), nil
	case "MIN":
		if len(rows) == 0 {
			return nil, nil
		}
		minVal := rows[0][arg]
		for _, row := range rows[1:] {
			v := row[arg]
			lf, lok := toFloat(minVal)
			rf, rok := toFloat(v)
			if lok && rok {
				if rf < lf {
					minVal = v
				}
			} else if fmt.Sprintf("%v", v) < fmt.Sprintf("%v", minVal) {
				minVal = v
			}
		}
		return minVal, nil
	case "MAX":
		if len(rows) == 0 {
			return nil, nil
		}
		maxVal := rows[0][arg]
		for _, row := range rows[1:] {
			v := row[arg]
			lf, lok := toFloat(maxVal)
			rf, rok := toFloat(v)
			if lok && rok {
				if rf > lf {
					maxVal = v
				}
			} else if fmt.Sprintf("%v", v) > fmt.Sprintf("%v", maxVal) {
				maxVal = v
			}
		}
		return maxVal, nil
	}
	return nil, fmt.Errorf("unknown aggregate function %q", fn)
}

func postProcess(result *Result, s *parser.SelectStatement) {
	if s.Distinct {
		applyDistinct(result)
		result.Trace = append(result.Trace, fmt.Sprintf("DISTINCT — %d unique row(s)", len(result.Rows)))
	}
	if len(s.OrderBy) > 0 {
		applyOrderBy(result, s.OrderBy)
		cols := make([]string, len(s.OrderBy))
		for i, ob := range s.OrderBy {
			dir := "ASC"
			if ob.Desc {
				dir = "DESC"
			}
			cols[i] = ob.Col + " " + dir
		}
		result.Trace = append(result.Trace, fmt.Sprintf("ORDER BY %s", joinStrings(cols)))
	}
	if s.Offset != nil || s.Limit != nil {
		total := len(result.Rows)
		applyLimitOffset(result, s.Limit, s.Offset)
		result.Trace = append(result.Trace, fmt.Sprintf("LIMIT/OFFSET — %d of %d row(s) returned", len(result.Rows), total))
	}
}

func applyDistinct(result *Result) {
	seen := map[string]bool{}
	var unique [][]interface{}
	for _, row := range result.Rows {
		key := fmt.Sprintf("%v", row)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, row)
		}
	}
	result.Rows = unique
}

func applyOrderBy(result *Result, orderBy []parser.OrderByExpr) {
	colIdx := map[string]int{}
	for i, col := range result.Columns {
		colIdx[col] = i
	}
	sort.SliceStable(result.Rows, func(i, j int) bool {
		for _, ob := range orderBy {
			idx, ok := colIdx[ob.Col]
			if !ok {
				continue
			}
			cmp := compareVals(result.Rows[i][idx], result.Rows[j][idx])
			if cmp == 0 {
				continue
			}
			if ob.Desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
}

func compareVals(a, b interface{}) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if af < bf {
			return -1
		}
		if af > bf {
			return 1
		}
		return 0
	}
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	if as < bs {
		return -1
	}
	if as > bs {
		return 1
	}
	return 0
}

func applyLimitOffset(result *Result, limit, offset *int64) {
	rows := result.Rows
	if offset != nil && *offset > 0 {
		o := int(*offset)
		if o >= len(rows) {
			result.Rows = [][]interface{}{}
			return
		}
		rows = rows[o:]
	}
	if limit != nil {
		l := int(*limit)
		if l < len(rows) {
			rows = rows[:l]
		}
	}
	result.Rows = rows
}

func formatAggKeys(m map[string]aggSpec) string {
	if len(m) == 0 {
		return "none"
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return joinStrings(keys)
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
