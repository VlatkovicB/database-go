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

// subqueryProj holds a scalar subquery expression to be evaluated per row during projection.
type subqueryProj struct {
	colIdx int
	expr   parser.Expression
}

// execPlan holds a volcano node tree and projection info for one SELECT.
type execPlan struct {
	root          Node
	cols          []string // output column names
	keys          []string // row map keys corresponding to each output column
	logger        *ExecLogger
	ctes          map[string]*cteEntry
	subqueryExprs []subqueryProj
}

// containsSubquery returns true if expr contains any subquery expression node.
func containsSubquery(expr parser.Expression) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *parser.SubqueryExpr, *parser.InSubqueryExpr, *parser.ExistsExpr:
		return true
	case *parser.BinaryExpr:
		return containsSubquery(e.Left) || containsSubquery(e.Right)
	}
	return false
}

// planSelect materializes CTEs and then builds the volcano execution tree.
func (e *Executor) planSelect(sel *parser.SelectStatement) (*execPlan, error) {
	var ctes map[string]*cteEntry
	if len(sel.With) > 0 {
		ctes = make(map[string]*cteEntry)
		for _, cte := range sel.With {
			sub, err := e.planSelectWithCTEs(cte.Query, nil)
			if err != nil {
				return nil, fmt.Errorf("CTE %q: %w", cte.Name, err)
			}
			if err := sub.root.Open(); err != nil {
				return nil, fmt.Errorf("CTE %q open: %w", cte.Name, err)
			}
			var cteRows []storage.Row
			for {
				row, err := sub.root.Next()
				if err != nil {
					sub.root.Close()
					return nil, err
				}
				if row == nil {
					break
				}
				// Store CTE rows with bare column names (strip any alias prefix)
				// so that the outer query can re-prefix with whatever alias it uses.
				projected := make(storage.Row)
				for i, key := range sub.keys {
					val := row[key]
					colName := sub.cols[i] // bare column name (e.g. "username")
					projected[colName] = val
				}
				cteRows = append(cteRows, projected)
			}
			sub.root.Close()
			// Keys stored with bare names; outer query's cteSeqScan will re-prefix.
			ctes[cte.Name] = &cteEntry{rows: cteRows, cols: sub.cols, keys: sub.cols}
		}
	}
	return e.planSelectWithCTEs(sel, ctes)
}

// planSelectWithCTEs builds the volcano execution tree from a SelectStatement, using pre-materialized CTEs.
func (e *Executor) planSelectWithCTEs(sel *parser.SelectStatement, ctes map[string]*cteEntry) (*execPlan, error) {
	// Materialize any derived tables (FROM/JOIN subqueries) into the CTEs map.
	if sel.FromSubquery != nil {
		if ctes == nil {
			ctes = make(map[string]*cteEntry)
		}
		entry, err := e.materializeDerivedTable(sel.FromSubquery, ctes)
		if err != nil {
			return nil, fmt.Errorf("derived table %q: %w", sel.Alias, err)
		}
		ctes[sel.Table] = entry
	}
	for _, j := range sel.Joins {
		if j.JoinSubquery != nil && !j.Lateral {
			if ctes == nil {
				ctes = make(map[string]*cteEntry)
			}
			entry, err := e.materializeDerivedTable(j.JoinSubquery, ctes)
			if err != nil {
				return nil, fmt.Errorf("derived table %q: %w", j.Alias, err)
			}
			ctes[j.Table] = entry
		}
	}

	alias := sel.Alias
	if alias == "" {
		alias = sel.Table
	}

	// Resolve table schema for projection resolution.
	type aliasInfo struct {
		alias string
		cols  []storage.Column
	}

	// Base table columns — check CTEs first (covers derived tables).
	var baseTableCols []storage.Column
	if ctes != nil {
		if entry, ok := ctes[sel.Table]; ok {
			for _, col := range entry.cols {
				baseTableCols = append(baseTableCols, storage.Column{Name: col})
			}
		}
	}
	if baseTableCols == nil {
		baseTable, err := e.db.GetTable(sel.Table)
		if err != nil {
			return nil, err
		}
		baseTableCols = baseTable.Columns
	}
	aliasOrder := []aliasInfo{{alias, baseTableCols}}
	for _, j := range sel.Joins {
		ja := j.Alias
		if ja == "" {
			ja = j.Table
		}
		// LATERAL: derive column list from subquery AST (not pre-materialized).
		if j.Lateral && j.JoinSubquery != nil {
			var latCols []storage.Column
			for _, col := range lateralColsFromAST(j.JoinSubquery) {
				latCols = append(latCols, storage.Column{Name: col})
			}
			aliasOrder = append(aliasOrder, aliasInfo{ja, latCols})
			continue
		}
		// Check CTEs first — covers derived table joins.
		if ctes != nil {
			if entry, ok := ctes[j.Table]; ok {
				var jCols []storage.Column
				for _, col := range entry.cols {
					jCols = append(jCols, storage.Column{Name: col})
				}
				aliasOrder = append(aliasOrder, aliasInfo{ja, jCols})
				continue
			}
		}
		jt, err := e.db.GetTable(j.Table)
		if err != nil {
			return nil, err
		}
		aliasOrder = append(aliasOrder, aliasInfo{ja, jt.Columns})
	}

	snap := e.currentSnapshot()
	xid := e.currentXID()
	logger := newExecLogger()
	assignLog := func(node Node) Node {
		if l, ok := node.(loggable); ok {
			l.setLog(logger, logger.NextID())
		}
		return node
	}

	// Cost-based planner: choose scan types and join algorithms.
	p := newQPlanner(e, ctes)
	refs := buildTableRefs(sel) // lateral joins excluded by buildTableRefs

	// Determine whether any non-lateral joins exist.
	hasNonLateralJoins := false
	hasLateralJoins := false
	for _, j := range sel.Joins {
		if j.Lateral {
			hasLateralJoins = true
		} else {
			hasNonLateralJoins = true
		}
	}

	// For single-table queries without subqueries in WHERE, pass WHERE to planner for index selection.
	// For queries with subqueries in WHERE or lateral joins, we'll add a ctx-aware filter after planning.
	singleWhere := parser.Expression(nil)
	if !hasNonLateralJoins && !hasLateralJoins && !containsSubquery(sel.Where) {
		singleWhere = sel.Where
	}
	physRel := p.planRelations(refs, singleWhere)

	lockMode := storage.NoLock
	if sel.ForLock == "FOR UPDATE" {
		lockMode = storage.ExclusiveLock
	} else if sel.ForLock == "FOR SHARE" {
		lockMode = storage.ShareLock
	}

	root := physRelToVolcano(physRel, e.db, snap, xid, logger, ctes, lockMode, e.db.LockMgr)

	// Wrap with LATERAL join nodes (must be before WHERE filter so lateral cols are visible).
	for _, j := range sel.Joins {
		if !j.Lateral || j.JoinSubquery == nil {
			continue
		}
		root = assignLog(newLateralJoin(root, j.JoinSubquery, j.Alias, j.Type, j.Condition, e, ctes))
	}

	// Apply WHERE filter:
	// - Multi-table or subquery-in-WHERE: add filter above join/scan with subquery ctx
	// - Single-table without subqueries: already applied by planner inside physRelToVolcano
	if sel.Where != nil && (hasNonLateralJoins || hasLateralJoins || containsSubquery(sel.Where)) {
		sqCtx := &EvalCtx{exec: e, ctes: ctes}
		root = assignLog(newFilterNodeWithCtx(root, sel.Where, sqCtx))
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

	plan := &execPlan{root: root, logger: logger, ctes: ctes}

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
				switch ex := expr.(type) {
				case *parser.ColSelectExpr:
					col := ex.Col
					if idx := strings.Index(col, "."); idx >= 0 {
						// Qualified ref (e.g. t.username) — strip the alias prefix.
						plan.cols = append(plan.cols, col[idx+1:])
						plan.keys = append(plan.keys, col)
					} else {
						plan.cols = append(plan.cols, col)
						plan.keys = append(plan.keys, alias+"."+col)
					}
				case *parser.ExprSelectExpr:
					colAlias := ex.Alias
					if colAlias == "" {
						colAlias = "subquery"
					}
					syntheticKey := "__sqexpr__" + colAlias
					plan.cols = append(plan.cols, colAlias)
					plan.keys = append(plan.keys, syntheticKey)
					plan.subqueryExprs = append(plan.subqueryExprs, subqueryProj{
						colIdx: len(plan.keys) - 1,
						expr:   ex.Expr,
					})
				}
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
				switch ex := expr.(type) {
				case *parser.ColSelectExpr:
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
				case *parser.ExprSelectExpr:
					colAlias := ex.Alias
					if colAlias == "" {
						colAlias = "subquery"
					}
					syntheticKey := "__sqexpr__" + colAlias
					plan.cols = append(plan.cols, colAlias)
					plan.keys = append(plan.keys, syntheticKey)
					plan.subqueryExprs = append(plan.subqueryExprs, subqueryProj{
						colIdx: len(plan.keys) - 1,
						expr:   ex.Expr,
					})
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
		// Evaluate scalar subquery projections (e.g. SELECT (SELECT COUNT(*) ...) AS bc).
		if len(plan.subqueryExprs) > 0 {
			sqCtx := &EvalCtx{exec: e, outer: row, ctes: plan.ctes}
			for _, sp := range plan.subqueryExprs {
				val, _ := evalExpr(sp.expr, row, sqCtx)
				r[sp.colIdx] = val
			}
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
