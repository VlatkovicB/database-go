package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Node is the volcano/iterator interface — each node pulls rows one at a time
// from its children. This mirrors PostgreSQL's executor node model.
// Open prepares state, Next emits one row (nil = EOF), Close releases resources.
type Node interface {
	Open() error
	Next() (storage.Row, error)
	Close()
	NodeName() string
	NodeChildren() []Node
}

// BufferReporter is implemented by scan nodes that track pages read.
type BufferReporter interface {
	BuffersRead() int
	ScanTable() string
}

// =============================================================================
// seqScan — reads all rows from one table, emits alias-prefixed rows
// =============================================================================

type seqScan struct {
	db          *storage.Database
	table       string
	alias       string
	tuples      []storage.Tuple
	pos         int
	buffersRead int
}

func newSeqScan(db *storage.Database, table, alias string) *seqScan {
	return &seqScan{db: db, table: table, alias: alias}
}

func (n *seqScan) NodeName() string     { return "Seq Scan on " + n.table }
func (n *seqScan) NodeChildren() []Node { return nil }
func (n *seqScan) BuffersRead() int     { return n.buffersRead }
func (n *seqScan) ScanTable() string    { return n.table }

func (n *seqScan) Open() error {
	tuples, _, pageCount, err := n.db.ScanPages(n.table)
	if err != nil {
		return err
	}
	pfx := n.alias + "."
	n.tuples = make([]storage.Tuple, len(tuples))
	for i, t := range tuples {
		row := make(storage.Row, len(t.Data)+1)
		for k, v := range t.Data {
			row[pfx+k] = v
		}
		row[pfx+"ctid"] = t.CTID()
		n.tuples[i] = storage.Tuple{PageNum: t.PageNum, SlotNum: t.SlotNum, Data: row}
	}
	n.buffersRead = pageCount
	n.pos = 0
	return nil
}

func (n *seqScan) Next() (storage.Row, error) {
	if n.pos >= len(n.tuples) {
		return nil, nil
	}
	row := n.tuples[n.pos].Data
	n.pos++
	return row, nil
}

func (n *seqScan) Close() { n.tuples = nil }

// =============================================================================
// indexScan — reads rows from a B+ tree index, emits alias-prefixed rows
// =============================================================================

type indexScan struct {
	db          *storage.Database
	table       string
	alias       string
	indexName   string
	column      string
	lo          interface{}
	loOp        string
	hi          interface{}
	hiOp        string
	tuples      []storage.Tuple
	pos         int
	buffersRead int // index depth pages + heap pages touched
}

func newIndexScan(db *storage.Database, table, alias, indexName, column string, lo interface{}, loOp string, hi interface{}, hiOp string) *indexScan {
	return &indexScan{
		db:        db,
		table:     table,
		alias:     alias,
		indexName: indexName,
		column:    column,
		lo:        lo,
		loOp:      loOp,
		hi:        hi,
		hiOp:      hiOp,
	}
}

func (n *indexScan) NodeName() string {
	return "Index Scan using " + n.indexName + " on " + n.table
}
func (n *indexScan) NodeChildren() []Node { return nil }
func (n *indexScan) BuffersRead() int     { return n.buffersRead }
func (n *indexScan) ScanTable() string    { return n.table }

func (n *indexScan) Open() error {
	tuples, depth, err := n.db.IndexRangeScan(n.table, n.indexName, n.lo, n.loOp, n.hi, n.hiOp)
	if err != nil {
		return err
	}
	pfx := n.alias + "."
	pagesHit := make(map[int]struct{})
	n.tuples = make([]storage.Tuple, len(tuples))
	for i, t := range tuples {
		row := make(storage.Row, len(t.Data)+1)
		for k, v := range t.Data {
			row[pfx+k] = v
		}
		row[pfx+"ctid"] = t.CTID()
		n.tuples[i] = storage.Tuple{PageNum: t.PageNum, SlotNum: t.SlotNum, Data: row}
		pagesHit[t.PageNum] = struct{}{}
	}
	n.buffersRead = depth + len(pagesHit)
	n.pos = 0
	return nil
}

func (n *indexScan) Next() (storage.Row, error) {
	if n.pos >= len(n.tuples) {
		return nil, nil
	}
	row := n.tuples[n.pos].Data
	n.pos++
	return row, nil
}

func (n *indexScan) Close() { n.tuples = nil }

// =============================================================================
// filterNode — passes rows where predicate evaluates to true
// =============================================================================

type filterNode struct {
	child Node
	pred  parser.Expression
}

func newFilterNode(child Node, pred parser.Expression) *filterNode {
	return &filterNode{child: child, pred: pred}
}

func (n *filterNode) NodeName() string     { return "Filter" }
func (n *filterNode) NodeChildren() []Node { return []Node{n.child} }
func (n *filterNode) Open() error          { return n.child.Open() }
func (n *filterNode) Close()               { n.child.Close() }

func (n *filterNode) Next() (storage.Row, error) {
	for {
		row, err := n.child.Next()
		if err != nil || row == nil {
			return row, err
		}
		pass, err := evalExpr(n.pred, row)
		if err != nil {
			return nil, err
		}
		if boolVal(pass) {
			return row, nil
		}
	}
}

// =============================================================================
// nestedLoopJoin — materialises the inner side, probes once per outer row
// =============================================================================

type nestedLoopJoin struct {
	outer        Node
	inner        Node
	cond         parser.Expression
	joinType     parser.JoinType
	innerRows    []storage.Row
	innerKeys    map[string]bool
	outerRow     storage.Row
	innerPos     int
	emittedMatch bool
}

func newNestedLoopJoin(outer, inner Node, cond parser.Expression, joinType parser.JoinType) *nestedLoopJoin {
	return &nestedLoopJoin{outer: outer, inner: inner, cond: cond, joinType: joinType}
}

func (n *nestedLoopJoin) NodeName() string {
	if n.joinType == parser.LeftJoin {
		return "Nested Loop Left Join"
	}
	return "Nested Loop"
}
func (n *nestedLoopJoin) NodeChildren() []Node { return []Node{n.outer, n.inner} }

func (n *nestedLoopJoin) Open() error {
	// Materialise inner side (build phase).
	if err := n.inner.Open(); err != nil {
		return err
	}
	n.innerKeys = map[string]bool{}
	for {
		row, err := n.inner.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		for k := range row {
			n.innerKeys[k] = true
		}
		n.innerRows = append(n.innerRows, row)
	}
	n.inner.Close()

	if err := n.outer.Open(); err != nil {
		return err
	}
	n.outerRow = nil
	n.innerPos = len(n.innerRows) // trigger outer advance on first Next()
	return nil
}

func (n *nestedLoopJoin) Close() {
	n.outer.Close()
	n.innerRows = nil
	n.innerKeys = nil
}

func (n *nestedLoopJoin) Next() (storage.Row, error) {
	for {
		if n.innerPos >= len(n.innerRows) {
			// LEFT JOIN: emit null-padded row for previous unmatched outer row.
			if n.outerRow != nil && !n.emittedMatch && n.joinType == parser.LeftJoin {
				nr := make(storage.Row, len(n.outerRow)+len(n.innerKeys))
				for k, v := range n.outerRow {
					nr[k] = v
				}
				for k := range n.innerKeys {
					nr[k] = nil
				}
				n.outerRow = nil
				return nr, nil
			}
			var err error
			n.outerRow, err = n.outer.Next()
			if err != nil {
				return nil, err
			}
			if n.outerRow == nil {
				return nil, nil // outer EOF
			}
			n.innerPos = 0
			n.emittedMatch = false
		}

		if n.innerPos >= len(n.innerRows) {
			n.outerRow = nil
			continue
		}

		innerRow := n.innerRows[n.innerPos]
		n.innerPos++

		combined := make(storage.Row, len(n.outerRow)+len(innerRow))
		for k, v := range n.outerRow {
			combined[k] = v
		}
		for k, v := range innerRow {
			combined[k] = v
		}

		if n.cond != nil {
			ok, err := evalExpr(n.cond, combined)
			if err != nil {
				return nil, err
			}
			if !boolVal(ok) {
				continue
			}
		}
		n.emittedMatch = true
		return combined, nil
	}
}

// =============================================================================
// hashAggregate — groups rows and computes aggregate functions
// =============================================================================

type aggGroup struct {
	keyVals map[string]interface{}
	rows    []storage.Row
}

type hashAggregate struct {
	child       Node
	groupBy     []string
	selectExprs []parser.SelectExpr
	having      parser.Expression
	output      []storage.Row
	pos         int
	built       bool
}

func newHashAggregate(child Node, groupBy []string, exprs []parser.SelectExpr, having parser.Expression) *hashAggregate {
	return &hashAggregate{child: child, groupBy: groupBy, selectExprs: exprs, having: having}
}

func (n *hashAggregate) NodeName() string     { return "HashAggregate" }
func (n *hashAggregate) NodeChildren() []Node { return []Node{n.child} }
func (n *hashAggregate) Open() error          { return n.child.Open() }
func (n *hashAggregate) Close() {
	n.child.Close()
	n.output = nil
}

func (n *hashAggregate) build() error {
	var groups []*aggGroup
	groupIdx := map[string]int{}

	for {
		row, err := n.child.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}

		var keyParts []string
		for _, col := range n.groupBy {
			val, _ := evalExpr(&parser.IdentExpr{Name: col}, row)
			keyParts = append(keyParts, fmt.Sprintf("%v", val))
		}
		if len(n.groupBy) == 0 {
			keyParts = []string{"__all__"}
		}
		key := strings.Join(keyParts, "\x00")

		if idx, ok := groupIdx[key]; ok {
			groups[idx].rows = append(groups[idx].rows, row)
		} else {
			kv := map[string]interface{}{}
			for _, col := range n.groupBy {
				val, _ := evalExpr(&parser.IdentExpr{Name: col}, row)
				kv[col] = val
			}
			groupIdx[key] = len(groups)
			groups = append(groups, &aggGroup{keyVals: kv, rows: []storage.Row{row}})
		}
	}

	// No GROUP BY with zero input still emits one row (e.g. COUNT(*) = 0).
	if len(n.groupBy) == 0 && len(groups) == 0 {
		groups = []*aggGroup{{keyVals: map[string]interface{}{}, rows: nil}}
	}

	// Collect aggregate specs from SELECT list and HAVING.
	needed := map[string]aggSpec{}
	for _, expr := range n.selectExprs {
		if agg, ok := expr.(*parser.AggSelectExpr); ok {
			k := agg.Func + "(" + agg.Arg + ")"
			needed[k] = aggSpec{agg.Func, agg.Arg}
		}
	}
	collectAggFuncs(n.having, needed)

	// Build synthetic row per group, apply HAVING.
	for _, g := range groups {
		sr := storage.Row{}
		for k, v := range g.keyVals {
			sr[k] = v
		}
		for key, spec := range needed {
			val, err := computeAggFromRows(spec.fn, spec.arg, g.rows)
			if err != nil {
				return err
			}
			sr[key] = val
		}

		if n.having != nil {
			pass, err := evalExpr(n.having, sr)
			if err != nil {
				return err
			}
			if !boolVal(pass) {
				continue
			}
		}
		n.output = append(n.output, sr)
	}
	n.built = true
	return nil
}

func (n *hashAggregate) Next() (storage.Row, error) {
	if !n.built {
		if err := n.build(); err != nil {
			return nil, err
		}
	}
	if n.pos >= len(n.output) {
		return nil, nil
	}
	row := n.output[n.pos]
	n.pos++
	return row, nil
}

// computeAggFromRows resolves column values via evalExpr (handles alias.col keys).
func computeAggFromRows(fn, arg string, rows []storage.Row) (interface{}, error) {
	switch fn {
	case "COUNT":
		if arg == "*" {
			return int64(len(rows)), nil
		}
		var count int64
		for _, row := range rows {
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row)
			if val != nil {
				count++
			}
		}
		return count, nil
	case "SUM":
		var sum float64
		for _, row := range rows {
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row)
			f, ok := toFloat(val)
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
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row)
			f, ok := toFloat(val)
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
		var minVal interface{}
		for _, row := range rows {
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row)
			if minVal == nil {
				minVal = val
				continue
			}
			lf, lok := toFloat(minVal)
			rf, rok := toFloat(val)
			if lok && rok {
				if rf < lf {
					minVal = val
				}
			} else if fmt.Sprintf("%v", val) < fmt.Sprintf("%v", minVal) {
				minVal = val
			}
		}
		return minVal, nil
	case "MAX":
		if len(rows) == 0 {
			return nil, nil
		}
		var maxVal interface{}
		for _, row := range rows {
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row)
			if maxVal == nil {
				maxVal = val
				continue
			}
			lf, lok := toFloat(maxVal)
			rf, rok := toFloat(val)
			if lok && rok {
				if rf > lf {
					maxVal = val
				}
			} else if fmt.Sprintf("%v", val) > fmt.Sprintf("%v", maxVal) {
				maxVal = val
			}
		}
		return maxVal, nil
	}
	return nil, fmt.Errorf("unknown aggregate function %q", fn)
}

// =============================================================================
// sortNode — materialises child output, then emits sorted rows
// =============================================================================

type sortNode struct {
	child   Node
	orderBy []parser.OrderByExpr
	rows    []storage.Row
	pos     int
	built   bool
}

func newSortNode(child Node, orderBy []parser.OrderByExpr) *sortNode {
	return &sortNode{child: child, orderBy: orderBy}
}

func (n *sortNode) NodeName() string     { return "Sort" }
func (n *sortNode) NodeChildren() []Node { return []Node{n.child} }
func (n *sortNode) Open() error          { return n.child.Open() }
func (n *sortNode) Close() {
	n.child.Close()
	n.rows = nil
}

func (n *sortNode) Next() (storage.Row, error) {
	if !n.built {
		for {
			row, err := n.child.Next()
			if err != nil {
				return nil, err
			}
			if row == nil {
				break
			}
			n.rows = append(n.rows, row)
		}
		sort.SliceStable(n.rows, func(i, j int) bool {
			for _, ob := range n.orderBy {
				vi := resolveCol(n.rows[i], ob.Col)
				vj := resolveCol(n.rows[j], ob.Col)
				cmp := compareVals(vi, vj)
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
		n.built = true
	}
	if n.pos >= len(n.rows) {
		return nil, nil
	}
	row := n.rows[n.pos]
	n.pos++
	return row, nil
}

// resolveCol looks up a column value handling both bare and alias.col key formats.
func resolveCol(row storage.Row, col string) interface{} {
	if v, ok := row[col]; ok {
		return v
	}
	sfx := "." + col
	for k, v := range row {
		if strings.HasSuffix(k, sfx) {
			return v
		}
	}
	return nil
}

// =============================================================================
// limitNode — emits at most N rows after skipping offset rows
// =============================================================================

type limitNode struct {
	child   Node
	limit   int64
	offset  int64
	seen    int64
	emitted int64
}

func newLimitNode(child Node, limit, offset *int64) *limitNode {
	n := &limitNode{child: child, limit: -1}
	if limit != nil {
		n.limit = *limit
	}
	if offset != nil {
		n.offset = *offset
	}
	return n
}

func (n *limitNode) NodeName() string     { return "Limit" }
func (n *limitNode) NodeChildren() []Node { return []Node{n.child} }
func (n *limitNode) Open() error          { return n.child.Open() }
func (n *limitNode) Close()               { n.child.Close() }

func (n *limitNode) Next() (storage.Row, error) {
	for {
		if n.limit >= 0 && n.emitted >= n.limit {
			return nil, nil
		}
		row, err := n.child.Next()
		if err != nil || row == nil {
			return row, err
		}
		n.seen++
		if n.seen <= n.offset {
			continue
		}
		n.emitted++
		return row, nil
	}
}

// =============================================================================
// distinctNode — deduplicates rows by value fingerprint
// =============================================================================

type distinctNode struct {
	child Node
	seen  map[string]bool
}

func newDistinctNode(child Node) *distinctNode {
	return &distinctNode{child: child}
}

func (n *distinctNode) NodeName() string     { return "Unique" }
func (n *distinctNode) NodeChildren() []Node { return []Node{n.child} }

func (n *distinctNode) Open() error {
	n.seen = map[string]bool{}
	return n.child.Open()
}
func (n *distinctNode) Close() { n.child.Close() }

func (n *distinctNode) Next() (storage.Row, error) {
	for {
		row, err := n.child.Next()
		if err != nil || row == nil {
			return row, err
		}
		key := fmt.Sprintf("%v", row)
		if !n.seen[key] {
			n.seen[key] = true
			return row, nil
		}
	}
}

// =============================================================================
// instrumentedNode — wraps any Node to collect per-node timing and row counts
// =============================================================================

type instrumentedNode struct {
	child      Node
	totalNs    int64
	actualRows int
}

func newInstrumentedNode(child Node) *instrumentedNode {
	return &instrumentedNode{child: child}
}

func (n *instrumentedNode) NodeName() string     { return n.child.NodeName() }
func (n *instrumentedNode) NodeChildren() []Node { return n.child.NodeChildren() }

func (n *instrumentedNode) Open() error {
	t := time.Now()
	err := n.child.Open()
	n.totalNs += time.Since(t).Nanoseconds()
	return err
}

func (n *instrumentedNode) Next() (storage.Row, error) {
	t := time.Now()
	row, err := n.child.Next()
	n.totalNs += time.Since(t).Nanoseconds()
	if row != nil {
		n.actualRows++
	}
	return row, err
}

func (n *instrumentedNode) Close() {
	t := time.Now()
	n.child.Close()
	n.totalNs += time.Since(t).Nanoseconds()
}

func (n *instrumentedNode) ms() float64 {
	return float64(n.totalNs) / 1e6
}
