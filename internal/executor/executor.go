package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"strings"
	"time"
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
	case *parser.ExplainStatement:
		return e.execExplain(s)
	default:
		return nil, fmt.Errorf("unknown statement type")
	}
}

// =============================================================================
// SELECT — volcano-based execution
// =============================================================================

func hasAggExprs(exprs []parser.SelectExpr) bool {
	for _, ex := range exprs {
		if _, ok := ex.(*parser.AggSelectExpr); ok {
			return true
		}
	}
	return false
}

// execPlan holds a volcano node tree and projection info for one SELECT.
type execPlan struct {
	root Node
	cols []string // output column names
	keys []string // row map keys corresponding to each output column
}

// planSelect builds the volcano execution tree from a SelectStatement.
func (e *Executor) planSelect(sel *parser.SelectStatement) (*execPlan, error) {
	alias := sel.Alias
	if alias == "" {
		alias = sel.Table
	}

	// Resolve table schema for projection resolution.
	type aliasInfo struct {
		alias string
		cols  []storage.Column
	}
	baseTable, err := e.db.GetTable(sel.Table)
	if err != nil {
		return nil, err
	}
	aliasOrder := []aliasInfo{{alias, baseTable.Columns}}

	// SeqScan on base table.
	var root Node = newSeqScan(e.db, sel.Table, alias)

	// Joins — each becomes a NestedLoopJoin wrapping the previous root.
	for _, j := range sel.Joins {
		ja := j.Alias
		if ja == "" {
			ja = j.Table
		}
		jt, err := e.db.GetTable(j.Table)
		if err != nil {
			return nil, err
		}
		aliasOrder = append(aliasOrder, aliasInfo{ja, jt.Columns})
		inner := newSeqScan(e.db, j.Table, ja)
		root = newNestedLoopJoin(root, inner, j.Condition, j.Type)
	}

	// WHERE filter.
	if sel.Where != nil {
		root = newFilterNode(root, sel.Where)
	}

	isAgg := len(sel.GroupBy) > 0 || hasAggExprs(sel.Exprs)

	// HashAggregate (absorbs GROUP BY, HAVING, and aggregate functions).
	if isAgg {
		root = newHashAggregate(root, sel.GroupBy, sel.Exprs, sel.Having)
	}

	// Sort.
	if len(sel.OrderBy) > 0 {
		root = newSortNode(root, sel.OrderBy)
	}

	// Distinct.
	if sel.Distinct {
		root = newDistinctNode(root)
	}

	// Limit / Offset.
	if sel.Limit != nil || sel.Offset != nil {
		root = newLimitNode(root, sel.Limit, sel.Offset)
	}

	plan := &execPlan{root: root}

	// ---- Projection: map output column names to row keys ----

	if isAgg {
		// Aggregate query: output rows use bare column names and aggregate keys.
		if sel.Exprs == nil {
			for _, col := range sel.GroupBy {
				plan.cols = append(plan.cols, col)
				plan.keys = append(plan.keys, col)
			}
		} else {
			for _, expr := range sel.Exprs {
				switch ex := expr.(type) {
				case *parser.ColSelectExpr:
					plan.cols = append(plan.cols, ex.Col)
					plan.keys = append(plan.keys, ex.Col)
				case *parser.AggSelectExpr:
					k := ex.Func + "(" + ex.Arg + ")"
					plan.cols = append(plan.cols, k)
					plan.keys = append(plan.keys, k)
				}
			}
		}
	} else if len(sel.Joins) == 0 {
		// Single-table query: row keys are alias.col.
		if sel.Exprs == nil {
			for _, c := range aliasOrder[0].cols {
				plan.cols = append(plan.cols, c.Name)
				plan.keys = append(plan.keys, alias+"."+c.Name)
			}
		} else {
			for _, expr := range sel.Exprs {
				ex, ok := expr.(*parser.ColSelectExpr)
				if !ok {
					continue
				}
				plan.cols = append(plan.cols, ex.Col)
				plan.keys = append(plan.keys, alias+"."+ex.Col)
			}
		}
	} else {
		// Join query: resolve each output column to an alias.col key.
		if sel.Exprs == nil {
			for _, ai := range aliasOrder {
				for _, c := range ai.cols {
					plan.cols = append(plan.cols, ai.alias+"."+c.Name)
					plan.keys = append(plan.keys, ai.alias+"."+c.Name)
				}
			}
		} else {
			for _, expr := range sel.Exprs {
				ex, ok := expr.(*parser.ColSelectExpr)
				if !ok {
					continue
				}
				col := ex.Col
				if strings.HasSuffix(col, ".*") {
					a := strings.TrimSuffix(col, ".*")
					for _, ai := range aliasOrder {
						if ai.alias == a {
							for _, c := range ai.cols {
								plan.cols = append(plan.cols, c.Name)
								plan.keys = append(plan.keys, a+"."+c.Name)
							}
							break
						}
					}
				} else if idx := strings.Index(col, "."); idx >= 0 {
					plan.cols = append(plan.cols, col[idx+1:])
					plan.keys = append(plan.keys, col)
				} else {
					// Unqualified column: search across all joined aliases.
					found := false
					for _, ai := range aliasOrder {
						if found {
							break
						}
						for _, c := range ai.cols {
							if c.Name == col {
								plan.cols = append(plan.cols, col)
								plan.keys = append(plan.keys, ai.alias+"."+col)
								found = true
								break
							}
						}
					}
					if !found {
						return nil, fmt.Errorf("column %q not found in any joined table", col)
					}
				}
			}
		}
	}

	return plan, nil
}

func (e *Executor) execSelect(s *parser.SelectStatement) (*Result, error) {
	plan, err := e.planSelect(s)
	if err != nil {
		return nil, err
	}
	if err := plan.root.Open(); err != nil {
		return nil, err
	}
	defer plan.root.Close()

	result := &Result{Columns: plan.cols, Rows: [][]interface{}{}}
	for {
		row, err := plan.root.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		r := make([]interface{}, len(plan.keys))
		for i, key := range plan.keys {
			r[i] = row[key]
		}
		result.Rows = append(result.Rows, r)
	}
	result.Trace = nodeTrace(plan.root)
	return result, nil
}

// nodeTrace collects node names bottom-up (innermost first, like a PG trace).
func nodeTrace(node Node) []string {
	if node == nil {
		return nil
	}
	var lines []string
	for _, child := range node.NodeChildren() {
		lines = append(lines, nodeTrace(child)...)
	}
	lines = append(lines, node.NodeName())
	return lines
}

// collectAggFuncs walks an expression tree and collects all aggregate function
// references (used to compute them during the HashAggregate build phase).
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

// =============================================================================
// DML statements
// =============================================================================

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
		if s.IfExists {
			return &Result{Message: fmt.Sprintf("table %q does not exist, skipped", s.Table)}, nil
		}
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

// =============================================================================
// Expression evaluation
// =============================================================================

// evalExpr evaluates an expression against a row and returns the result value.
func evalExpr(expr parser.Expression, row storage.Row) (interface{}, error) {
	switch e := expr.(type) {
	case *parser.IdentExpr:
		if e.Table != "" {
			key := e.Table + "." + e.Name
			if val, ok := row[key]; ok {
				return val, nil
			}
			return nil, fmt.Errorf("column %q.%q not found", e.Table, e.Name)
		}
		// Unqualified: try bare key first (aggregate rows), then suffix search (aliased rows).
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

func boolVal(v interface{}) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// aggSpec names an aggregate function and its argument column.
type aggSpec struct{ fn, arg string }

// =============================================================================
// EXPLAIN / EXPLAIN ANALYZE
// =============================================================================

func (e *Executor) execExplain(s *parser.ExplainStatement) (*Result, error) {
	if s.Analyze {
		return e.execExplainAnalyze(s.Stmt)
	}
	return e.execExplainPlan(s.Stmt)
}

// planNode is one node in the cost-estimate tree used by EXPLAIN (no ANALYZE).
type planNode struct {
	label      string
	estStartup float64
	estTotal   float64
	estRows    int
	width      int
	extras     []string
	child      *planNode
}

func exprStr(expr parser.Expression) string {
	switch e := expr.(type) {
	case *parser.BinaryExpr:
		return "(" + exprStr(e.Left) + " " + e.Op + " " + exprStr(e.Right) + ")"
	case *parser.IdentExpr:
		if e.Table != "" {
			return e.Table + "." + e.Name
		}
		return e.Name
	case *parser.LiteralExpr:
		switch v := e.Value.(type) {
		case string:
			return "'" + v + "'"
		case nil:
			return "NULL"
		default:
			return fmt.Sprintf("%v", v)
		}
	case *parser.AggFuncExpr:
		if e.Arg == nil {
			return e.Func + "(*)"
		}
		return e.Func + "(" + exprStr(e.Arg) + ")"
	}
	return "?"
}

// buildExplainTree builds the cost-estimate planNode tree for EXPLAIN rendering.
func (e *Executor) buildExplainTree(sel *parser.SelectStatement) *planNode {
	const costPerRow = 0.01
	const width = 64

	rowCount := func(name string) int {
		t, err := e.db.GetTable(name)
		if err != nil {
			return 100
		}
		return len(t.Rows)
	}

	mainN := rowCount(sel.Table)
	scan := &planNode{
		label:    "Seq Scan on " + sel.Table,
		estTotal: float64(mainN) * costPerRow,
		estRows:  mainN,
		width:    width,
	}
	if sel.Where != nil && len(sel.Joins) == 0 {
		scan.extras = append(scan.extras, "Filter: "+exprStr(sel.Where))
		scan.estRows = mainN / 3
		if scan.estRows < 1 {
			scan.estRows = 1
		}
	}
	root := (*planNode)(scan)

	for _, j := range sel.Joins {
		joinN := rowCount(j.Table)
		outN := root.estRows
		if joinN < outN {
			outN = joinN
		}
		label := "Nested Loop"
		if j.Type == parser.LeftJoin {
			label = "Nested Loop Left Join"
		}
		jn := &planNode{
			label:    label,
			estTotal: root.estTotal + float64(joinN)*costPerRow + 0.05,
			estRows:  outN,
			width:    width * 2,
			child:    root,
		}
		if j.Condition != nil {
			jn.extras = append(jn.extras, "Join Filter: "+exprStr(j.Condition))
		}
		if sel.Where != nil {
			jn.extras = append(jn.extras, "Filter: "+exprStr(sel.Where))
			jn.estRows = outN / 2
			if jn.estRows < 1 {
				jn.estRows = 1
			}
		}
		root = jn
	}

	if len(sel.GroupBy) > 0 || hasAggExprs(sel.Exprs) {
		inN := root.estRows
		outN := inN / 5
		if outN < 1 {
			outN = 1
		}
		agg := &planNode{
			label:    "HashAggregate",
			estTotal: root.estTotal + float64(inN)*costPerRow,
			estRows:  outN,
			width:    width,
			child:    root,
		}
		if len(sel.GroupBy) > 0 {
			agg.extras = append(agg.extras, "Group Key: "+joinStrings(sel.GroupBy))
		}
		if sel.Having != nil {
			agg.extras = append(agg.extras, "Filter: "+exprStr(sel.Having))
			agg.estRows = outN / 2
			if agg.estRows < 1 {
				agg.estRows = 1
			}
		}
		root = agg
	}

	if len(sel.OrderBy) > 0 {
		inN := root.estRows
		sortCost := root.estTotal * 0.3
		cols := make([]string, len(sel.OrderBy))
		for i, ob := range sel.OrderBy {
			dir := "ASC"
			if ob.Desc {
				dir = "DESC"
			}
			cols[i] = ob.Col + " " + dir
		}
		srt := &planNode{
			label:      "Sort",
			estStartup: root.estTotal + sortCost,
			estTotal:   root.estTotal + sortCost,
			estRows:    inN,
			width:      width,
			child:      root,
		}
		srt.extras = append(srt.extras, "Sort Key: "+joinStrings(cols))
		root = srt
	}

	if sel.Distinct {
		inN := root.estRows
		uniqN := inN / 2
		if uniqN < 1 {
			uniqN = 1
		}
		root = &planNode{
			label:    "Unique",
			estTotal: root.estTotal + float64(inN)*costPerRow,
			estRows:  uniqN,
			width:    width,
			child:    root,
		}
	}

	if sel.Limit != nil {
		limitN := int(*sel.Limit)
		if limitN > root.estRows {
			limitN = root.estRows
		}
		root = &planNode{
			label:    "Limit",
			estTotal: root.estTotal,
			estRows:  limitN,
			width:    width,
			child:    root,
		}
	}

	return root
}

func renderPlanTree(node *planNode, depth int, analyze bool, actualRows int, totalMs float64) []string {
	var lines []string

	var nodePrefix, extraIndent string
	if depth == 0 {
		nodePrefix = ""
		extraIndent = "  "
	} else {
		nodePrefix = strings.Repeat(" ", 6*depth-4) + "->  "
		extraIndent = strings.Repeat(" ", 6*depth+2)
	}

	costPart := fmt.Sprintf("(cost=%.2f..%.2f rows=%d width=%d)",
		node.estStartup, node.estTotal, node.estRows, node.width)

	analyzePart := ""
	if analyze {
		frac := 1.0
		for i := 0; i < depth; i++ {
			frac *= 0.80
		}
		nodeMs := totalMs * frac
		startMs := nodeMs * 0.75
		actRows := node.estRows
		if depth == 0 {
			actRows = actualRows
		}
		analyzePart = fmt.Sprintf(" (actual time=%.3f..%.3f rows=%d loops=1)", startMs, nodeMs, actRows)
	}

	lines = append(lines, fmt.Sprintf("%s%s  %s%s", nodePrefix, node.label, costPart, analyzePart))

	for _, ex := range node.extras {
		lines = append(lines, extraIndent+ex)
	}

	if node.child != nil {
		lines = append(lines, renderPlanTree(node.child, depth+1, analyze, actualRows, totalMs)...)
	}

	return lines
}

func planToResult(lines []string) *Result {
	cols := []string{"QUERY PLAN"}
	rows := make([][]interface{}, len(lines))
	for i, l := range lines {
		rows[i] = []interface{}{l}
	}
	return &Result{Columns: cols, Rows: rows, Trace: lines}
}

func (e *Executor) execExplainPlan(stmt parser.Statement) (*Result, error) {
	planStart := time.Now()

	var lines []string
	sel, ok := stmt.(*parser.SelectStatement)
	if !ok {
		lines = []string{fmt.Sprintf("%T  (cost=0.00..0.00 rows=0 width=0)", stmt)}
	} else {
		lines = renderPlanTree(e.buildExplainTree(sel), 0, false, 0, 0)
	}

	planMs := float64(time.Since(planStart).Nanoseconds()) / 1e6
	lines = append(lines, fmt.Sprintf("Planning Time: %.3f ms", planMs))

	return planToResult(lines), nil
}

func (e *Executor) execExplainAnalyze(stmt parser.Statement) (*Result, error) {
	planStart := time.Now()
	sel, _ := stmt.(*parser.SelectStatement)
	var tree *planNode
	if sel != nil {
		tree = e.buildExplainTree(sel)
	}
	planMs := float64(time.Since(planStart).Nanoseconds()) / 1e6

	execStart := time.Now()
	inner, err := e.Execute(stmt)
	execMs := float64(time.Since(execStart).Nanoseconds()) / 1e6
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN ANALYZE: %w", err)
	}

	actualRows := len(inner.Rows)

	var lines []string
	if tree != nil {
		lines = renderPlanTree(tree, 0, true, actualRows, execMs)
	} else {
		lines = []string{fmt.Sprintf("%T  (cost=0.00..0.00 rows=0 width=0) (actual time=%.3f..%.3f rows=%d loops=1)",
			stmt, execMs*0.75, execMs, actualRows)}
	}

	lines = append(lines, fmt.Sprintf("Planning Time: %.3f ms", planMs))
	lines = append(lines, fmt.Sprintf("Execution Time: %.3f ms", execMs))

	return planToResult(lines), nil
}
