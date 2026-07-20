package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"strings"
	"time"
)

type IndexSuggestion struct {
	Reason string `json:"reason"`
	SQL    string `json:"sql"`
}

type Result struct {
	Columns          []string
	Rows             [][]interface{}
	Message          string
	Trace            []string
	StepLog          []StepEvent
	NodeTree         *NodeTreeDesc
	StepTruncated    bool
	IndexSuggestions []IndexSuggestion
}

type NodeTreeDesc struct {
	ID       int             `json:"id"`
	NodeType string          `json:"nodeType"`
	Children []*NodeTreeDesc `json:"children,omitempty"`
}

func buildNodeTree(node Node) *NodeTreeDesc {
	if node == nil {
		return nil
	}
	desc := &NodeTreeDesc{ID: node.NodeID(), NodeType: node.NodeName()}
	for _, child := range node.NodeChildren() {
		desc.Children = append(desc.Children, buildNodeTree(child))
	}
	return desc
}

type Executor struct {
	db        *storage.Database
	CurrentTx *storage.Transaction
}

func New(db *storage.Database) *Executor {
	return &Executor{db: db}
}

// currentXID returns the active transaction ID, or 0 for auto-commit mode.
func (e *Executor) currentXID() uint64 {
	if e.CurrentTx != nil {
		return e.CurrentTx.ID
	}
	return 0
}

// currentSnapshot returns a pointer to the active transaction's snapshot,
// or nil for auto-commit mode.
func (e *Executor) currentSnapshot() *storage.Snapshot {
	if e.CurrentTx != nil {
		return &e.CurrentTx.Snapshot
	}
	return nil
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
	case *parser.CreateIndexStatement:
		return e.execCreateIndex(s)
	case *parser.DropIndexStatement:
		return e.execDropIndex(s)
	case *parser.AnalyzeStatement:
		return e.execAnalyze(s)
	case *parser.BeginStatement:
		return e.execBegin()
	case *parser.CommitStatement:
		return e.execCommit()
	case *parser.RollbackStatement:
		return e.execRollback()
	case *parser.VacuumStatement:
		return e.execVacuum(s)
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

	xid := e.currentXID()
	if err := e.db.Insert(s.Table, row, xid); err != nil {
		return nil, err
	}
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
	count, err := e.db.UpdateRows(s.Table,
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
	count, err := e.db.DeleteRows(s.Table, func(row storage.Row) bool {
		if s.Where == nil {
			return true
		}
		match, _ := evalExpr(s.Where, row)
		return boolVal(match)
	}, xid)
	if err != nil {
		return nil, err
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
	if len(s.ForeignKeys) > 0 {
		var fks []storage.FKConstraint
		for _, fk := range s.ForeignKeys {
			fks = append(fks, storage.FKConstraint{
				Column:    fk.Column,
				RefTable:  fk.RefTable,
				RefColumn: fk.RefColumn,
			})
		}
		if err := e.db.SetForeignKeys(s.Table, fks); err != nil {
			return nil, err
		}
	}
	trace := []string{
		fmt.Sprintf("Validate %d column definition(s)", len(cols)),
		fmt.Sprintf("Register %q in database catalog", s.Table),
		"Initialize empty row storage",
	}
	if len(s.ForeignKeys) > 0 {
		trace = append(trace, fmt.Sprintf("Register %d foreign key constraint(s)", len(s.ForeignKeys)))
	}
	return &Result{
		Message: fmt.Sprintf("table %q created", s.Table),
		Trace:   trace,
	}, nil
}

func (e *Executor) execCreateIndex(s *parser.CreateIndexStatement) (*Result, error) {
	if err := e.db.CreateIndex(s.Name, s.Table, s.Column); err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("index %q created on %q (%s)", s.Name, s.Table, s.Column),
		Trace: []string{
			fmt.Sprintf("Scan all pages of %q", s.Table),
			fmt.Sprintf("Build B+ tree on column %q", s.Column),
			fmt.Sprintf("Register index %q in table catalog", s.Name),
		},
	}, nil
}

func (e *Executor) execDropIndex(s *parser.DropIndexStatement) (*Result, error) {
	if err := e.db.DropIndexByName(s.Name, s.IfExists); err != nil {
		return nil, err
	}
	if s.IfExists {
		return &Result{Message: fmt.Sprintf("index %q dropped (or did not exist)", s.Name)}, nil
	}
	return &Result{
		Message: fmt.Sprintf("index %q dropped", s.Name),
		Trace: []string{
			fmt.Sprintf("Remove index %q from table catalog", s.Name),
		},
	}, nil
}

func (e *Executor) execAnalyze(s *parser.AnalyzeStatement) (*Result, error) {
	lines, err := e.db.AnalyzeTable(s.Table)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("ANALYZE %q — statistics updated", s.Table),
		Trace:   lines,
	}, nil
}

// =============================================================================
// Transaction control (MVCC Phase 5)
// =============================================================================

func (e *Executor) execBegin() (*Result, error) {
	if e.CurrentTx != nil {
		return nil, fmt.Errorf("there is already an open transaction (xid=%d)", e.CurrentTx.ID)
	}
	e.CurrentTx = e.db.TxManager.Begin()
	return &Result{
		Message: "BEGIN",
		Trace: []string{
			fmt.Sprintf("Assigned xid=%d", e.CurrentTx.ID),
			fmt.Sprintf("Snapshot: xmin=%d xmax=%d active=%v",
				e.CurrentTx.Snapshot.Xmin,
				e.CurrentTx.Snapshot.Xmax,
				e.CurrentTx.Snapshot.Active),
		},
	}, nil
}

func (e *Executor) execCommit() (*Result, error) {
	if e.CurrentTx == nil {
		return nil, fmt.Errorf("there is no open transaction")
	}
	xid := e.CurrentTx.ID
	if err := e.db.TxManager.Commit(xid); err != nil {
		return nil, err
	}
	e.CurrentTx = nil
	return &Result{
		Message: "COMMIT",
		Trace: []string{
			fmt.Sprintf("xid=%d marked COMMITTED", xid),
			"Tuple versions written by this tx are now visible to other transactions",
		},
	}, nil
}

func (e *Executor) execRollback() (*Result, error) {
	if e.CurrentTx == nil {
		return nil, fmt.Errorf("there is no open transaction")
	}
	xid := e.CurrentTx.ID
	if err := e.db.TxManager.Abort(xid); err != nil {
		return nil, err
	}
	e.CurrentTx = nil
	return &Result{
		Message: "ROLLBACK",
		Trace: []string{
			fmt.Sprintf("xid=%d marked ABORTED", xid),
			"Tuple versions written by this tx are invisible (xmin not committed)",
			"Dead tuples will be reclaimed by VACUUM",
		},
	}, nil
}

func (e *Executor) execVacuum(s *parser.VacuumStatement) (*Result, error) {
	reclaimed, err := e.db.Vacuum(s.Table)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("VACUUM %q — %d dead tuple(s) reclaimed", s.Table, reclaimed),
		Trace: []string{
			fmt.Sprintf("Scan %q for tuples where xmax != 0 AND xmax is committed", s.Table),
			fmt.Sprintf("Reclaimed %d dead tuple(s)", reclaimed),
			"Rebuilt indexes",
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

// estimateSelectivity returns the fraction of rows expected to pass the WHERE clause
// for tableName, using stored column statistics when available.
// Falls back to hardcoded PostgreSQL-style defaults when no ANALYZE has been run.
func (e *Executor) estimateSelectivity(tableName string, where parser.Expression) float64 {
	if where == nil {
		return 1.0
	}
	ts := e.db.GetTableStats(tableName)
	return selectivityExpr(where, tableName, ts)
}

func selectivityExpr(expr parser.Expression, tableName string, ts *storage.TableStats) float64 {
	switch e := expr.(type) {
	case *parser.BinaryExpr:
		switch e.Op {
		case "AND":
			s1 := selectivityExpr(e.Left, tableName, ts)
			s2 := selectivityExpr(e.Right, tableName, ts)
			return s1 * s2
		case "OR":
			s1 := selectivityExpr(e.Left, tableName, ts)
			s2 := selectivityExpr(e.Right, tableName, ts)
			return 1.0 - (1.0-s1)*(1.0-s2)
		case "=", "!=", "<", "<=", ">", ">=":
			return selectivityCmp(e, tableName, ts)
		}
	}
	return 1.0 / 3.0 // default: no stats, no match
}

func selectivityCmp(b *parser.BinaryExpr, tableName string, ts *storage.TableStats) float64 {
	ident, lit, op := extractIdentLit(b)
	if ident == nil || lit == nil {
		return 1.0 / 3.0
	}

	if ts == nil {
		// No stats — use PostgreSQL-style defaults.
		switch op {
		case "=":
			return 0.005
		case "!=":
			return 0.995
		default:
			return 1.0 / 3.0
		}
	}

	colName := ident.Name
	cs := ts.Columns[colName]
	if cs == nil {
		switch op {
		case "=":
			return 0.005
		case "!=":
			return 0.995
		default:
			return 1.0 / 3.0
		}
	}

	switch op {
	case "=":
		return cs.EqualitySelectivity(lit.Value)
	case "!=":
		return 1.0 - cs.EqualitySelectivity(lit.Value)
	case "<", "<=", ">", ">=":
		if sel, ok := cs.HistogramSelectivity(op, lit.Value); ok {
			return sel
		}
		return 1.0 / 3.0
	}
	return 1.0 / 3.0
}

// extractIdentLit returns the column identifier and literal from a simple comparison,
// normalizing flipped forms like "10 < level" into "level > 10".
func extractIdentLit(b *parser.BinaryExpr) (ident *parser.IdentExpr, lit *parser.LiteralExpr, op string) {
	if id, ok := b.Left.(*parser.IdentExpr); ok {
		if l, ok2 := b.Right.(*parser.LiteralExpr); ok2 {
			return id, l, b.Op
		}
	}
	if id, ok := b.Right.(*parser.IdentExpr); ok {
		if l, ok2 := b.Left.(*parser.LiteralExpr); ok2 {
			flipped := map[string]string{">": "<", ">=": "<=", "<": ">", "<=": ">=", "=": "=", "!=": "!="}
			return id, l, flipped[b.Op]
		}
	}
	return nil, nil, ""
}

// groupByNDistinct returns the estimated number of distinct values for the first GROUP BY column.
func (e *Executor) groupByNDistinct(tableName string, groupByCols []string, nRows int) int {
	if len(groupByCols) == 0 {
		return 1
	}
	ts := e.db.GetTableStats(tableName)
	if ts == nil {
		return max(nRows/5, 1)
	}
	total := 1.0
	for _, col := range groupByCols {
		cs := ts.Columns[col]
		if cs == nil {
			total *= float64(max(nRows/5, 1))
			continue
		}
		nd := cs.NDistinct
		if nd < 0 {
			// all distinct — cap at row count to avoid explosion
			nd = float64(nRows)
		}
		total *= nd
	}
	n := int(total)
	if n < 1 {
		n = 1
	}
	if n > nRows {
		n = nRows
	}
	return n
}

// distinctOutputRows estimates SELECT DISTINCT output rows using column stats.
func (e *Executor) distinctOutputRows(tableName string, exprs []parser.SelectExpr, nRows int) int {
	ts := e.db.GetTableStats(tableName)
	if ts == nil {
		return max(nRows/2, 1)
	}
	// Use n_distinct of the first plain column expression.
	for _, ex := range exprs {
		if col, ok := ex.(*parser.ColSelectExpr); ok && col.Col != "*" {
			colName := col.Col
			if cs := ts.Columns[colName]; cs != nil {
				nd := cs.NDistinct
				if nd < 0 {
					nd = float64(nRows)
				}
				n := int(nd)
				if n < 1 {
					n = 1
				}
				if n > nRows {
					n = nRows
				}
				return n
			}
		}
	}
	return max(nRows/2, 1)
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
		n.extras = append(n.extras, "Filter: "+exprStr(sel.Where))
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
			agg.extras = append(agg.extras, "Filter: "+exprStr(sel.Having))
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
