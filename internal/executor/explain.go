package executor

// =============================================================================
// EXPLAIN / EXPLAIN ANALYZE
// =============================================================================

import (
	"database/internal/parser"
	"fmt"
	"math"
	"strings"
	"time"
)

// planNode is one node in the cost-estimate tree used by EXPLAIN (no ANALYZE).
// children holds ordered child nodes; joins have two (left then right).
type planNode struct {
	label      string
	estStartup float64
	estTotal   float64
	estRows    int
	width      int
	extras     []string
	children   []*planNode
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

// buildExplainTree builds the cost-estimate planNode tree for EXPLAIN rendering.
// It delegates scan and join selection to the cost-based qplanner so that EXPLAIN
// shows the same operators that will actually execute.
func (e *Executor) buildExplainTree(sel *parser.SelectStatement) *planNode {
	p := newQPlanner(e, nil)
	refs := buildTableRefs(sel)

	// Pass WHERE only for single-table queries; multi-table WHERE is a filter above the join.
	singleWhere := parser.Expression(nil)
	if len(sel.Joins) == 0 {
		singleWhere = sel.Where
	}

	root := physRelToPlanNode(p.planRelations(refs, singleWhere), e.db, nil)

	mainN, _ := e.db.RowCount(sel.Table)

	// WHERE filter above the join tree (multi-table only).
	if sel.Where != nil && len(sel.Joins) > 0 {
		sel2 := e.estimateSelectivity(sel.Table, sel.Where)
		filterRows := int(math.Max(1, float64(root.estRows)*sel2))
		root = &planNode{
			label:    "Filter",
			estTotal: root.estTotal + float64(root.estRows)*cpuTupleCost,
			estRows:  filterRows,
			width:    root.width,
			extras:   []string{"Filter: " + exprToSQL(sel.Where)},
			children: []*planNode{root},
		}
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
			estTotal: root.estTotal + float64(inN)*cpuTupleCost,
			estRows:  outN,
			width:    root.width,
			children: []*planNode{root},
		}
		if len(sel.GroupBy) > 0 {
			agg.extras = append(agg.extras, "Group Key: "+joinStrings(sel.GroupBy))
		}
		if sel.Having != nil {
			agg.extras = append(agg.extras, "Filter: "+exprToSQL(sel.Having))
			havingSel := e.estimateSelectivity(sel.Table, sel.Having)
			agg.estRows = int(math.Max(1, float64(outN)*havingSel))
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
		root = &planNode{
			label:      "Sort",
			estStartup: root.estTotal + sortCost,
			estTotal:   root.estTotal + sortCost,
			estRows:    inN,
			width:      root.width,
			extras:     []string{"Sort Key: " + joinStrings(cols)},
			children:   []*planNode{root},
		}
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
			estTotal: root.estTotal + float64(inN)*cpuTupleCost,
			estRows:  uniqN,
			width:    root.width,
			children: []*planNode{root},
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
			width:    root.width,
			children: []*planNode{root},
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

	for _, child := range node.children {
		lines = append(lines, renderPlanTree(child, depth+1, analyze, actualRows, totalMs)...)
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
	for _, child := range node.children {
		injectBuffers(child, buffers)
	}
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
