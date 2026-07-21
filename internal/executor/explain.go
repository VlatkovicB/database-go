package executor

// =============================================================================
// EXPLAIN / EXPLAIN ANALYZE
// =============================================================================

import (
	"database/internal/parser"
	"fmt"
	"strings"
	"time"
)

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

func exprToSQL(expr parser.Expression) string {
	switch e := expr.(type) {
	case *parser.BinaryExpr:
		return "(" + exprToSQL(e.Left) + " " + e.Op + " " + exprToSQL(e.Right) + ")"
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
		return e.Func + "(" + exprToSQL(e.Arg) + ")"
	}
	return "?"
}

// buildScanNode chooses between Index Scan and Seq Scan for the base table.
func (e *Executor) buildScanNode(sel *parser.SelectStatement, mainN int, pageCount func(string) int, costPerRow float64, width int) *planNode {
	if sel.Where != nil && len(sel.Joins) == 0 {
		if ip := e.findIndexPlan(sel.Table, sel.Where); ip != nil {
			depth := e.db.GetIndexDepth(sel.Table, ip.indexName)
			sel2 := selectivityExpr(sel.Where, sel.Table, e.db.GetTableStats(sel.Table))
			estRows := int(float64(mainN) * sel2)
			if estRows < 1 {
				estRows = 1
			}
			n := &planNode{
				label:    "Index Scan using " + ip.indexName + " on " + sel.Table,
				estTotal: float64(depth)*0.25 + float64(estRows)*costPerRow,
				estRows:  estRows,
				width:    width,
			}
			n.extras = append(n.extras, fmt.Sprintf("Index Cond: (%s.%s)", sel.Table, ip.column))
			n.extras = append(n.extras, fmt.Sprintf("Index Pages: %d", depth))
			return n
		}
	}
	n := &planNode{
		label:    "Seq Scan on " + sel.Table,
		estTotal: float64(mainN) * costPerRow,
		estRows:  mainN,
		width:    width,
	}
	n.extras = append(n.extras, fmt.Sprintf("Heap Pages: %d", pageCount(sel.Table)))
	if sel.Where != nil && len(sel.Joins) == 0 {
		n.extras = append(n.extras, "Filter: "+exprToSQL(sel.Where))
		sel2 := e.estimateSelectivity(sel.Table, sel.Where)
		n.estRows = int(float64(mainN) * sel2)
		if n.estRows < 1 {
			n.estRows = 1
		}
	}
	return n
}

// buildExplainTree builds the cost-estimate planNode tree for EXPLAIN rendering.
func (e *Executor) buildExplainTree(sel *parser.SelectStatement) *planNode {
	const costPerRow = 0.01
	const width = 64

	rowCount := func(name string) int {
		n, err := e.db.RowCount(name)
		if err != nil {
			return 100
		}
		return n
	}

	pageCount := func(name string) int {
		n, err := e.db.PageCount(name)
		if err != nil {
			return 1
		}
		if n < 1 {
			return 1
		}
		return n
	}

	mainN := rowCount(sel.Table)
	scan := e.buildScanNode(sel, mainN, pageCount, costPerRow, width)
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
			jn.extras = append(jn.extras, "Join Filter: "+exprToSQL(j.Condition))
		}
		if sel.Where != nil {
			jn.extras = append(jn.extras, "Filter: "+exprToSQL(sel.Where))
			jn.estRows = outN / 2
			if jn.estRows < 1 {
				jn.estRows = 1
			}
		}
		root = jn
	}

	if len(sel.GroupBy) > 0 || hasAggExprs(sel.Exprs) {
		inN := root.estRows
		outN := e.groupByNDistinct(sel.Table, sel.GroupBy, mainN)
		if outN > inN {
			outN = inN
		}
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
			agg.extras = append(agg.extras, "Filter: "+exprToSQL(sel.Having))
			havingSel := e.estimateSelectivity(sel.Table, sel.Having)
			agg.estRows = int(float64(outN) * havingSel)
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
		uniqN := e.distinctOutputRows(sel.Table, sel.Exprs, mainN)
		if uniqN > inN {
			uniqN = inN
		}
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

// collectBuffers walks a volcano tree and returns buffers read per table name.
func collectBuffers(node Node) map[string]int {
	result := map[string]int{}
	if node == nil {
		return result
	}
	if br, ok := node.(BufferReporter); ok {
		result[br.ScanTable()] += br.BuffersRead()
	}
	for _, child := range node.NodeChildren() {
		for k, v := range collectBuffers(child) {
			result[k] += v
		}
	}
	return result
}

// injectBuffers walks the planNode tree and adds buffer stats to matching scan nodes.
func injectBuffers(node *planNode, buffers map[string]int) {
	if node == nil {
		return
	}
	for tableName, count := range buffers {
		if strings.HasPrefix(node.label, "Seq Scan on "+tableName) ||
			strings.HasSuffix(node.label, " on "+tableName) {
			node.extras = append(node.extras, fmt.Sprintf("Buffers: shared read=%d", count))
		}
	}
	injectBuffers(node.child, buffers)
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

	// Build volcano plan to collect real buffer stats.
	if sel != nil {
		volcanoPlan, err := e.planSelect(sel)
		if err != nil {
			return nil, fmt.Errorf("EXPLAIN ANALYZE: %w", err)
		}
		if err := volcanoPlan.root.Open(); err != nil {
			return nil, fmt.Errorf("EXPLAIN ANALYZE: %w", err)
		}
		// Drain all rows.
		var actualRows int
		for {
			row, err := volcanoPlan.root.Next()
			if err != nil {
				volcanoPlan.root.Close()
				return nil, fmt.Errorf("EXPLAIN ANALYZE: %w", err)
			}
			if row == nil {
				break
			}
			actualRows++
		}
		volcanoPlan.root.Close()
		execMs := float64(time.Since(execStart).Nanoseconds()) / 1e6

		buffers := collectBuffers(volcanoPlan.root)
		if tree != nil {
			injectBuffers(tree, buffers)
		}

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

	// Non-SELECT fallback (DDL etc.)
	inner, err := e.Execute(stmt)
	execMs := float64(time.Since(execStart).Nanoseconds()) / 1e6
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN ANALYZE: %w", err)
	}
	actualRows := len(inner.Rows)
	lines := []string{fmt.Sprintf("%T  (cost=0.00..0.00 rows=0 width=0) (actual time=%.3f..%.3f rows=%d loops=1)",
		stmt, execMs*0.75, execMs, actualRows)}
	lines = append(lines, fmt.Sprintf("Planning Time: %.3f ms", planMs))
	lines = append(lines, fmt.Sprintf("Execution Time: %.3f ms", execMs))
	return planToResult(lines), nil
}
