package executor

// =============================================================================
// SELECT — volcano-based execution
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"strings"
)

func hasAggExprs(exprs []parser.SelectExpr) bool {
	for _, ex := range exprs {
		if _, ok := ex.(*parser.AggSelectExpr); ok {
			return true
		}
	}
	return false
}

// indexPlan captures a WHERE predicate that can be satisfied by a B+ tree index.
type indexPlan struct {
	indexName string
	column    string
	lo        interface{}
	loOp      string
	hi        interface{}
	hiOp      string
	residual  parser.Expression // remaining WHERE conditions not covered by the index
}

// extractSingleBound checks if expr is a simple "col op literal" or "literal op col" comparison.
// Returns the column name and populated lo/hi bounds, or ok=false if not applicable.
func extractSingleBound(expr parser.Expression) (col string, lo interface{}, loOp string, hi interface{}, hiOp string, ok bool) {
	bin, isBin := expr.(*parser.BinaryExpr)
	if !isBin {
		return
	}

	var ident *parser.IdentExpr
	var lit *parser.LiteralExpr
	var op string

	if id, o1 := bin.Left.(*parser.IdentExpr); o1 {
		if l, o2 := bin.Right.(*parser.LiteralExpr); o2 {
			ident, lit, op = id, l, bin.Op
		}
	} else if id, o1 := bin.Right.(*parser.IdentExpr); o1 {
		if l, o2 := bin.Left.(*parser.LiteralExpr); o2 {
			ident, lit = id, l
			switch bin.Op {
			case ">":
				op = "<"
			case ">=":
				op = "<="
			case "<":
				op = ">"
			case "<=":
				op = ">="
			default:
				op = bin.Op
			}
		}
	}
	if ident == nil {
		return
	}
	col = ident.Name
	switch op {
	case "=":
		return col, lit.Value, "=", nil, "", true
	case ">":
		return col, lit.Value, ">", nil, "", true
	case ">=":
		return col, lit.Value, ">=", nil, "", true
	case "<":
		return col, nil, "", lit.Value, "<", true
	case "<=":
		return col, nil, "", lit.Value, "<=", true
	}
	return
}

// extractRangeBounds tries to extract index-compatible bounds from a WHERE expression.
// Handles: single predicate, or two predicates on the same column joined by AND.
func extractRangeBounds(where parser.Expression) (col string, lo interface{}, loOp string, hi interface{}, hiOp string, residual parser.Expression, ok bool) {
	col, lo, loOp, hi, hiOp, ok = extractSingleBound(where)
	if ok {
		return col, lo, loOp, hi, hiOp, nil, true
	}
	bin, isBin := where.(*parser.BinaryExpr)
	if !isBin || bin.Op != "AND" {
		return
	}
	col2, lo2, loOp2, hi2, hiOp2, ok2 := extractSingleBound(bin.Left)
	col3, lo3, loOp3, hi3, hiOp3, ok3 := extractSingleBound(bin.Right)

	if ok2 && ok3 && col2 == col3 {
		lo, loOp = lo2, loOp2
		hi, hiOp = hi2, hiOp2
		if lo == nil {
			lo, loOp = lo3, loOp3
		}
		if hi == nil {
			hi, hiOp = hi3, hiOp3
		}
		return col2, lo, loOp, hi, hiOp, nil, true
	}
	if ok2 {
		return col2, lo2, loOp2, hi2, hiOp2, bin.Right, true
	}
	if ok3 {
		return col3, lo3, loOp3, hi3, hiOp3, bin.Left, true
	}
	return
}

// findIndexPlan returns an *indexPlan if the WHERE clause can use an index on tableName.
func (e *Executor) findIndexPlan(tableName string, where parser.Expression) *indexPlan {
	if where == nil {
		return nil
	}
	col, lo, loOp, hi, hiOp, residual, ok := extractRangeBounds(where)
	if !ok {
		return nil
	}
	indexName, found := e.db.FindIndexForColumn(tableName, col)
	if !found {
		return nil
	}
	return &indexPlan{
		indexName: indexName,
		column:    col,
		lo:        lo,
		loOp:      loOp,
		hi:        hi,
		hiOp:      hiOp,
		residual:  residual,
	}
}

// execPlan holds a volcano node tree and projection info for one SELECT.
type execPlan struct {
	root   Node
	cols   []string // output column names
	keys   []string // row map keys corresponding to each output column
	logger *ExecLogger
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

	// Choose scan strategy: Index Scan (single-table with index) or Seq Scan.
	var root Node
	whereHandled := false
	snap := e.currentSnapshot()
	xid := e.currentXID()

	logger := newExecLogger()
	assignLog := func(node Node) Node {
		if l, ok := node.(loggable); ok {
			l.setLog(logger, logger.NextID())
		}
		return node
	}

	if len(sel.Joins) == 0 {
		if ip := e.findIndexPlan(sel.Table, sel.Where); ip != nil {
			root = assignLog(newIndexScan(e.db, sel.Table, alias, ip.indexName, ip.column, ip.lo, ip.loOp, ip.hi, ip.hiOp, snap, xid))
			if ip.residual != nil {
				root = assignLog(newFilterNode(root, ip.residual))
			}
			whereHandled = true
		}
	}
	if root == nil {
		root = assignLog(newSeqScan(e.db, sel.Table, alias, snap, xid))
	}

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
		inner := assignLog(newSeqScan(e.db, j.Table, ja, snap, xid))
		root = assignLog(newNestedLoopJoin(root, inner, j.Condition, j.Type))
	}

	// WHERE filter (skip if already handled by index scan).
	if sel.Where != nil && !whereHandled {
		root = assignLog(newFilterNode(root, sel.Where))
	}

	isAgg := len(sel.GroupBy) > 0 || hasAggExprs(sel.Exprs)

	// HashAggregate (absorbs GROUP BY, HAVING, and aggregate functions).
	if isAgg {
		root = assignLog(newHashAggregate(root, sel.GroupBy, sel.Exprs, sel.Having))
	}

	// Sort.
	if len(sel.OrderBy) > 0 {
		root = assignLog(newSortNode(root, sel.OrderBy))
	}

	// Limit / Offset.
	if sel.Limit != nil || sel.Offset != nil {
		root = assignLog(newLimitNode(root, sel.Limit, sel.Offset))
	}

	plan := &execPlan{root: root, logger: logger}

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

// collectIndexableCols walks an expression tree and returns all column names
// that appear in indexable comparisons (col op literal).
func collectIndexableCols(expr parser.Expression) []string {
	if expr == nil {
		return nil
	}
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		return nil
	}
	if bin.Op == "AND" || bin.Op == "OR" {
		return append(collectIndexableCols(bin.Left), collectIndexableCols(bin.Right)...)
	}
	col, _, _, _, _, ok2 := extractSingleBound(expr)
	if ok2 {
		return []string{col}
	}
	return nil
}

// collectJoinInnerCols returns columns from the inner table referenced in an ON condition.
func collectJoinInnerCols(expr parser.Expression, innerTable, innerAlias string) []string {
	if expr == nil {
		return nil
	}
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		return nil
	}
	if bin.Op == "AND" || bin.Op == "OR" {
		return append(collectJoinInnerCols(bin.Left, innerTable, innerAlias),
			collectJoinInnerCols(bin.Right, innerTable, innerAlias)...)
	}
	if bin.Op != "=" {
		return nil
	}
	var cols []string
	check := func(id *parser.IdentExpr) {
		if id.Table == innerAlias || id.Table == innerTable || id.Table == "" {
			cols = append(cols, id.Name)
		}
	}
	if id, ok := bin.Left.(*parser.IdentExpr); ok {
		check(id)
	}
	if id, ok := bin.Right.(*parser.IdentExpr); ok {
		check(id)
	}
	return cols
}

func (e *Executor) suggestIndexes(sel *parser.SelectStatement) []IndexSuggestion {
	var suggestions []IndexSuggestion
	seen := map[string]bool{}

	if sel.Where != nil && len(sel.Joins) == 0 {
		if e.findIndexPlan(sel.Table, sel.Where) == nil {
			for _, col := range collectIndexableCols(sel.Where) {
				key := sel.Table + "." + col
				if seen[key] {
					continue
				}
				seen[key] = true
				if _, found := e.db.FindIndexForColumn(sel.Table, col); !found {
					suggestions = append(suggestions, IndexSuggestion{
						Reason: fmt.Sprintf("SeqScan on %s with predicate on %s — index enables Index Scan", sel.Table, col),
						SQL:    fmt.Sprintf("CREATE INDEX idx_%s_%s ON %s(%s);", sel.Table, col, sel.Table, col),
					})
				}
			}
		}
	}

	for _, j := range sel.Joins {
		innerAlias := j.Alias
		if innerAlias == "" {
			innerAlias = j.Table
		}
		for _, col := range collectJoinInnerCols(j.Condition, j.Table, innerAlias) {
			key := j.Table + "." + col
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, found := e.db.FindIndexForColumn(j.Table, col); !found {
				suggestions = append(suggestions, IndexSuggestion{
					Reason: fmt.Sprintf("Nested Loop Join probes %s on %s — index speeds inner lookups", j.Table, col),
					SQL:    fmt.Sprintf("CREATE INDEX idx_%s_%s ON %s(%s);", j.Table, col, j.Table, col),
				})
			}
		}
	}

	return suggestions
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
	var seen map[string]bool
	if s.Distinct {
		seen = make(map[string]bool)
	}
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
		if seen != nil {
			key := fmt.Sprintf("%v", r)
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		result.Rows = append(result.Rows, r)
	}
	result.Trace = nodeTrace(plan.root)
	result.NodeTree = buildNodeTree(plan.root)
	result.StepLog = plan.logger.Events
	result.StepTruncated = plan.logger.Truncated
	result.IndexSuggestions = e.suggestIndexes(s)
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
