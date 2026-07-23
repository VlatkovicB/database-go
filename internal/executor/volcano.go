package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	NodeID() int
}

// BufferReporter is implemented by scan nodes that track pages read.
type BufferReporter interface {
	BuffersRead() int
	BufferHits() int
	BufferMisses() int
	ScanTable() string
}

const maxStepEvents = 500

type StepEvent struct {
	NodeID   int                    `json:"nodeId"`
	NodeType string                 `json:"nodeType"`
	Action   string                 `json:"action"`
	Row      map[string]interface{} `json:"row,omitempty"`
}

type ExecLogger struct {
	Events    []StepEvent
	Truncated bool
	nextID    int
}

func newExecLogger() *ExecLogger { return &ExecLogger{} }

func (l *ExecLogger) NextID() int {
	id := l.nextID
	l.nextID++
	return id
}

func (l *ExecLogger) Log(nodeID int, nodeType, action string, row storage.Row) {
	if len(l.Events) >= maxStepEvents {
		l.Truncated = true
		return
	}
	var r map[string]interface{}
	if row != nil {
		r = make(map[string]interface{}, len(row))
		for k, v := range row {
			if idx := strings.Index(k, "."); idx >= 0 {
				k = k[idx+1:]
			}
			if k == "ctid" {
				continue
			}
			r[k] = v
		}
	}
	l.Events = append(l.Events, StepEvent{NodeID: nodeID, NodeType: nodeType, Action: action, Row: r})
}

type nodeBase struct {
	id  int
	log *ExecLogger
}

func (b *nodeBase) NodeID() int { return b.id }

func (b *nodeBase) setLog(log *ExecLogger, id int) {
	b.log = log
	b.id = id
}

type loggable interface {
	setLog(log *ExecLogger, id int)
}

// =============================================================================
// seqScan — reads all rows from one table, emits alias-prefixed rows
// =============================================================================

type seqScan struct {
	nodeBase
	db        *storage.Database
	table     string
	alias     string
	snap      *storage.Snapshot
	xid       uint64
	lockMode  storage.LockMode
	lockMgr   *storage.LockManager
	tuples    []storage.Tuple
	pos       int
	bufHits   int
	bufMisses int
}

func newSeqScan(db *storage.Database, table, alias string, snap *storage.Snapshot, xid uint64, lockMode storage.LockMode, lockMgr *storage.LockManager) *seqScan {
	return &seqScan{db: db, table: table, alias: alias, snap: snap, xid: xid, lockMode: lockMode, lockMgr: lockMgr}
}

func (n *seqScan) NodeName() string     { return "Seq Scan on " + n.table }
func (n *seqScan) NodeChildren() []Node { return nil }
func (n *seqScan) BuffersRead() int     { return n.bufHits + n.bufMisses }
func (n *seqScan) BufferHits() int      { return n.bufHits }
func (n *seqScan) BufferMisses() int    { return n.bufMisses }
func (n *seqScan) ScanTable() string    { return n.table }

func (n *seqScan) Open() error {
	tuples, _, pageCount, bpStats, err := n.db.ScanPages(n.table, n.snap, n.xid)
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
		n.tuples[i] = storage.Tuple{PageNum: t.PageNum, SlotNum: t.SlotNum, Data: row, Xmin: t.Xmin, Xmax: t.Xmax}
	}
	n.bufHits = int(bpStats.Hits)
	n.bufMisses = int(bpStats.Misses)
	_ = pageCount // kept for reference; total = bufHits + bufMisses
	n.pos = 0

	// SELECT FOR UPDATE / FOR SHARE: acquire row locks on all visible tuples
	if n.lockMode != storage.NoLock && n.lockMgr != nil && n.xid != 0 {
		for _, t := range tuples {
			rowID := storage.RowLockID{Table: n.table, PageNum: t.PageNum, SlotNum: t.SlotNum}
			if err := n.lockMgr.Acquire(n.xid, rowID, n.lockMode); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *seqScan) Next() (storage.Row, error) {
	if n.pos >= len(n.tuples) {
		return nil, nil
	}
	row := n.tuples[n.pos].Data
	n.pos++
	if n.log != nil {
		n.log.Log(n.id, "Seq Scan", "scan", row)
	}
	return row, nil
}

func (n *seqScan) Close() { n.tuples = nil }

// =============================================================================
// parallelSeqScan — Gather node: splits heap pages across N worker goroutines,
// merges results. Mirrors PG's Parallel Seq Scan + Gather plan shape.
// Row locks (FOR UPDATE) are not supported in parallel mode.
// =============================================================================

type parallelSeqScan struct {
	nodeBase
	db         *storage.Database
	table      string
	alias      string
	snap       *storage.Snapshot
	xid        uint64
	numWorkers int
	tuples     []storage.Tuple
	pos        int
	bufHits    int
	bufMisses  int
}

func newParallelSeqScan(db *storage.Database, table, alias string, snap *storage.Snapshot, xid uint64, numWorkers int) *parallelSeqScan {
	return &parallelSeqScan{db: db, table: table, alias: alias, snap: snap, xid: xid, numWorkers: numWorkers}
}

func (n *parallelSeqScan) NodeName() string     { return "Parallel Seq Scan on " + n.table }
func (n *parallelSeqScan) NodeChildren() []Node { return nil }
func (n *parallelSeqScan) BuffersRead() int     { return n.bufHits + n.bufMisses }
func (n *parallelSeqScan) BufferHits() int      { return n.bufHits }
func (n *parallelSeqScan) BufferMisses() int    { return n.bufMisses }
func (n *parallelSeqScan) ScanTable() string    { return n.table }

func (n *parallelSeqScan) Open() error {
	pageCount, err := n.db.PageCount(n.table)
	if err != nil {
		return err
	}
	if pageCount == 0 {
		n.pos = 0
		return nil
	}

	workers := n.numWorkers
	if workers > pageCount {
		workers = pageCount
	}
	chunkSize := (pageCount + workers - 1) / workers

	type workerResult struct {
		tuples []storage.Tuple
		hits   int64
		misses int64
	}
	ch := make(chan workerResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if start >= pageCount {
			break
		}
		wg.Add(1)
		go func(startPage, endPage int) {
			defer wg.Done()
			tuples, stats, _ := n.db.ScanPagesRange(n.table, startPage, endPage, n.snap, n.xid)
			ch <- workerResult{tuples: tuples, hits: stats.Hits, misses: stats.Misses}
		}(start, end)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	pfx := n.alias + "."
	for res := range ch {
		n.bufHits += int(res.hits)
		n.bufMisses += int(res.misses)
		for _, t := range res.tuples {
			row := make(storage.Row, len(t.Data)+1)
			for k, v := range t.Data {
				row[pfx+k] = v
			}
			row[pfx+"ctid"] = t.CTID()
			n.tuples = append(n.tuples, storage.Tuple{
				PageNum: t.PageNum, SlotNum: t.SlotNum, Data: row, Xmin: t.Xmin, Xmax: t.Xmax,
			})
		}
	}
	n.pos = 0
	return nil
}

func (n *parallelSeqScan) Next() (storage.Row, error) {
	if n.pos >= len(n.tuples) {
		return nil, nil
	}
	row := n.tuples[n.pos].Data
	n.pos++
	if n.log != nil {
		n.log.Log(n.id, "Parallel Seq Scan", "scan", row)
	}
	return row, nil
}

func (n *parallelSeqScan) Close() { n.tuples = nil }

// =============================================================================
// indexScan — reads rows from a B+ tree index, emits alias-prefixed rows
// =============================================================================

type indexScan struct {
	nodeBase
	db          *storage.Database
	table       string
	alias       string
	indexName   string
	column      string
	lo          interface{}
	loOp        string
	hi          interface{}
	hiOp        string
	snap        *storage.Snapshot
	xid         uint64
	tuples      []storage.Tuple
	pos         int
	buffersRead int // index depth pages + heap pages touched
}

func newIndexScan(db *storage.Database, table, alias, indexName, column string, lo interface{}, loOp string, hi interface{}, hiOp string, snap *storage.Snapshot, xid uint64) *indexScan {
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
		snap:      snap,
		xid:       xid,
	}
}

func (n *indexScan) NodeName() string {
	return "Index Scan using " + n.indexName + " on " + n.table
}
func (n *indexScan) NodeChildren() []Node { return nil }
func (n *indexScan) BuffersRead() int     { return n.buffersRead }
func (n *indexScan) BufferHits() int      { return 0 } // index pages not tracked via BPM
func (n *indexScan) BufferMisses() int    { return n.buffersRead }
func (n *indexScan) ScanTable() string    { return n.table }

func (n *indexScan) Open() error {
	tuples, depth, err := n.db.IndexRangeScan(n.table, n.indexName, n.lo, n.loOp, n.hi, n.hiOp)
	if err != nil {
		return err
	}
	pfx := n.alias + "."
	pagesHit := make(map[int]struct{})
	n.tuples = nil
	for _, t := range tuples {
		// Apply MVCC visibility filter
		if !n.db.TupleVisible(t, n.snap, n.xid) {
			continue
		}
		row := make(storage.Row, len(t.Data)+1)
		for k, v := range t.Data {
			row[pfx+k] = v
		}
		row[pfx+"ctid"] = t.CTID()
		n.tuples = append(n.tuples, storage.Tuple{PageNum: t.PageNum, SlotNum: t.SlotNum, Data: row, Xmin: t.Xmin, Xmax: t.Xmax})
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
	if n.log != nil {
		n.log.Log(n.id, "Index Scan", "scan", row)
	}
	return row, nil
}

func (n *indexScan) Close() { n.tuples = nil }

// =============================================================================
// filterNode — passes rows where predicate evaluates to true
// =============================================================================

type filterNode struct {
	nodeBase
	child Node
	pred  parser.Expression
	ctx   *EvalCtx
}

func newFilterNode(child Node, pred parser.Expression) *filterNode {
	return &filterNode{child: child, pred: pred}
}

func newFilterNodeWithCtx(child Node, pred parser.Expression, ctx *EvalCtx) *filterNode {
	return &filterNode{child: child, pred: pred, ctx: ctx}
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
		pass, err := evalExpr(n.pred, row, n.ctx)
		if err != nil {
			return nil, err
		}
		if boolVal(pass) {
			if n.log != nil {
				n.log.Log(n.id, "Filter", "pass", row)
			}
			return row, nil
		}
		if n.log != nil {
			n.log.Log(n.id, "Filter", "reject", row)
		}
	}
}

// =============================================================================
// nestedLoopJoin — materialises the inner side, probes once per outer row
// =============================================================================

type nestedLoopJoin struct {
	nodeBase
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
			ok, err := evalExpr(n.cond, combined, nil)
			if err != nil {
				return nil, err
			}
			if !boolVal(ok) {
				continue
			}
		}
		n.emittedMatch = true
		if n.log != nil {
			n.log.Log(n.id, "Nested Loop", "match", combined)
		}
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
	nodeBase
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
			val, _ := evalExpr(&parser.IdentExpr{Name: col}, row, nil)
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
				val, _ := evalExpr(&parser.IdentExpr{Name: col}, row, nil)
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
			pass, err := evalExpr(n.having, sr, nil)
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
	if n.log != nil {
		n.log.Log(n.id, "HashAggregate", "emit", row)
	}
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
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row, nil)
			if val != nil {
				count++
			}
		}
		return count, nil
	case "SUM":
		var sum float64
		for _, row := range rows {
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row, nil)
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
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row, nil)
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
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row, nil)
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
			val, _ := evalExpr(&parser.IdentExpr{Name: arg}, row, nil)
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
	nodeBase
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
	if n.log != nil {
		n.log.Log(n.id, "Sort", "emit", row)
	}
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
	nodeBase
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
			if n.log != nil {
				n.log.Log(n.id, "Limit", "skip", row)
			}
			continue
		}
		n.emitted++
		if n.log != nil {
			n.log.Log(n.id, "Limit", "pass", row)
		}
		return row, nil
	}
}

// =============================================================================
// distinctNode — deduplicates rows by value fingerprint
// =============================================================================

type distinctNode struct {
	nodeBase
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
			if n.log != nil {
				n.log.Log(n.id, "Unique", "pass", row)
			}
			return row, nil
		}
		if n.log != nil {
			n.log.Log(n.id, "Unique", "dedup", row)
		}
	}
}

// =============================================================================
// hashJoin — builds a hash table on the inner (right) side, probes per outer row
//
// This is faster than Nested Loop when both sides are large and no index exists
// on the inner join key. Build cost: O(inner). Probe cost: O(outer).
// =============================================================================

// extractHashJoinKeys splits an equality ON condition into outer and inner key
// expressions, identified by which side references innerAlias.
func extractHashJoinKeys(cond parser.Expression, innerAlias string) (outerExpr, innerExpr parser.Expression, ok bool) {
	if innerAlias == "" {
		return nil, nil, false
	}
	bin, isBin := cond.(*parser.BinaryExpr)
	if !isBin || bin.Op != "=" {
		return nil, nil, false
	}
	leftId, leftOk := bin.Left.(*parser.IdentExpr)
	rightId, rightOk := bin.Right.(*parser.IdentExpr)
	if !leftOk || !rightOk {
		return nil, nil, false
	}
	if leftId.Table == innerAlias {
		return bin.Right, bin.Left, true
	}
	if rightId.Table == innerAlias {
		return bin.Left, bin.Right, true
	}
	return nil, nil, false
}

type hashJoin struct {
	nodeBase
	outer      Node
	inner      Node
	innerAlias string
	cond       parser.Expression
	joinType   parser.JoinType

	// build phase
	hashTab   map[string][]storage.Row // keyed by inner join-column value
	fallback  []storage.Row            // used when condition is not a simple equality
	innerKeys map[string]bool          // all column keys seen in inner rows
	outerExpr parser.Expression        // outer side of the join equality
	innerExpr parser.Expression        // inner side of the join equality
	canHash   bool

	// probe state
	outerRow     storage.Row
	probeList    []storage.Row
	probePos     int
	emittedMatch bool
}

func newHashJoin(outer, inner Node, innerAlias string, cond parser.Expression, joinType parser.JoinType) *hashJoin {
	return &hashJoin{outer: outer, inner: inner, innerAlias: innerAlias, cond: cond, joinType: joinType}
}

func (n *hashJoin) NodeName() string {
	if n.joinType == parser.LeftJoin {
		return "Hash Join Left Join"
	}
	return "Hash Join"
}
func (n *hashJoin) NodeChildren() []Node { return []Node{n.outer, n.inner} }

func (n *hashJoin) Open() error {
	if err := n.inner.Open(); err != nil {
		return err
	}

	outerExpr, innerExpr, canHash := extractHashJoinKeys(n.cond, n.innerAlias)
	n.canHash = canHash
	n.outerExpr = outerExpr
	n.innerExpr = innerExpr
	n.hashTab = make(map[string][]storage.Row)
	n.innerKeys = map[string]bool{}

	// Build phase: scan inner once and hash on the join key.
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
		if n.canHash {
			keyVal, err := evalExpr(n.innerExpr, row, nil)
			if err == nil && keyVal != nil {
				key := fmt.Sprintf("%v", keyVal)
				n.hashTab[key] = append(n.hashTab[key], row)
				continue
			}
		}
		n.fallback = append(n.fallback, row)
	}
	n.inner.Close()

	if err := n.outer.Open(); err != nil {
		return err
	}
	n.outerRow = nil
	n.probeList = nil
	n.probePos = 0
	return nil
}

func (n *hashJoin) Close() {
	n.outer.Close()
	n.hashTab = nil
	n.fallback = nil
	n.innerKeys = nil
}

func (n *hashJoin) Next() (storage.Row, error) {
	for {
		// Drain remaining probe matches from the current outer row.
		for n.probePos < len(n.probeList) {
			innerRow := n.probeList[n.probePos]
			n.probePos++

			combined := make(storage.Row, len(n.outerRow)+len(innerRow))
			for k, v := range n.outerRow {
				combined[k] = v
			}
			for k, v := range innerRow {
				combined[k] = v
			}

			if n.cond != nil {
				ok, err := evalExpr(n.cond, combined, nil)
				if err != nil {
					return nil, err
				}
				if !boolVal(ok) {
					continue
				}
			}
			n.emittedMatch = true
			if n.log != nil {
				n.log.Log(n.id, "Hash Join", "match", combined)
			}
			return combined, nil
		}

		// LEFT JOIN: emit a null-padded row for an unmatched outer row.
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

		// Advance to next outer row.
		var err error
		n.outerRow, err = n.outer.Next()
		if err != nil {
			return nil, err
		}
		if n.outerRow == nil {
			return nil, nil
		}
		n.emittedMatch = false

		// Probe phase: look up hash table or fall back to linear scan.
		if n.canHash {
			keyVal, err := evalExpr(n.outerExpr, n.outerRow, nil)
			if err == nil && keyVal != nil {
				key := fmt.Sprintf("%v", keyVal)
				n.probeList = n.hashTab[key]
				n.probePos = 0
				continue
			}
		}
		n.probeList = n.fallback
		n.probePos = 0
	}
}

// =============================================================================
// cteSeqScan — iterates over a pre-materialized CTE result set
// =============================================================================

type cteSeqScan struct {
	nodeBase
	rows  []storage.Row
	alias string
	pos   int
}

func newCTESeqScan(rows []storage.Row, alias string) *cteSeqScan {
	return &cteSeqScan{rows: rows, alias: alias}
}

func (n *cteSeqScan) Open() error          { n.pos = 0; return nil }
func (n *cteSeqScan) Close()               {}
func (n *cteSeqScan) NodeName() string     { return "CTE Scan" }
func (n *cteSeqScan) NodeChildren() []Node { return nil }

func (n *cteSeqScan) Next() (storage.Row, error) {
	if n.pos >= len(n.rows) {
		return nil, nil
	}
	src := n.rows[n.pos]
	n.pos++
	if n.alias == "" {
		return src, nil
	}
	// Re-prefix bare column names with the outer alias (e.g. "username" -> "t.username").
	pfx := n.alias + "."
	out := make(storage.Row, len(src))
	for k, v := range src {
		if strings.Contains(k, ".") {
			out[k] = v // already prefixed (shouldn't happen but be safe)
		} else {
			out[pfx+k] = v
		}
	}
	return out, nil
}

// =============================================================================
// lateralJoin — re-evaluates a LATERAL subquery for each outer row
// =============================================================================

type lateralJoin struct {
	nodeBase
	outer     Node
	subq      *parser.SelectStatement
	alias     string
	joinType  parser.JoinType
	onCond    parser.Expression
	exec      *Executor
	ctes      map[string]*cteEntry
	innerCols []string // column names from AST for LEFT JOIN null-padding

	outerRow     storage.Row
	innerRows    []storage.Row
	innerIdx     int
	emittedMatch bool
	colsKnown    bool
}

func newLateralJoin(outer Node, subq *parser.SelectStatement, alias string, joinType parser.JoinType, onCond parser.Expression, exec *Executor, ctes map[string]*cteEntry) *lateralJoin {
	return &lateralJoin{
		outer:     outer,
		subq:      subq,
		alias:     alias,
		joinType:  joinType,
		onCond:    onCond,
		exec:      exec,
		ctes:      ctes,
		innerCols: lateralColsFromAST(subq),
	}
}

func (n *lateralJoin) NodeName() string {
	if n.joinType == parser.LeftJoin {
		return "Nested Loop Left Join (LATERAL)"
	}
	return "Nested Loop (LATERAL)"
}
func (n *lateralJoin) NodeChildren() []Node { return []Node{n.outer} }
func (n *lateralJoin) Open() error          { return n.outer.Open() }
func (n *lateralJoin) Close()               { n.outer.Close() }

func (n *lateralJoin) Next() (storage.Row, error) {
	for {
		if n.innerIdx >= len(n.innerRows) {
			// LEFT JOIN: emit null-padded outer for unmatched row.
			if n.outerRow != nil && !n.emittedMatch && n.joinType == parser.LeftJoin {
				nr := make(storage.Row, len(n.outerRow)+len(n.innerCols))
				for k, v := range n.outerRow {
					nr[k] = v
				}
				for _, col := range n.innerCols {
					nr[n.alias+"."+col] = nil
				}
				n.outerRow = nil
				return nr, nil
			}
			// Advance outer.
			outerRow, err := n.outer.Next()
			if err != nil {
				return nil, err
			}
			if outerRow == nil {
				return nil, nil
			}
			n.outerRow = outerRow
			ctx := &EvalCtx{exec: n.exec, outer: outerRow, ctes: n.ctes}
			n.innerRows, err = n.exec.materializeSubquery(n.subq, ctx)
			if err != nil {
				return nil, err
			}
			n.innerIdx = 0
			n.emittedMatch = false
		}

		if n.innerIdx >= len(n.innerRows) {
			continue
		}

		innerRow := n.innerRows[n.innerIdx]
		n.innerIdx++

		// Merge outer + re-keyed inner rows.
		merged := make(storage.Row, len(n.outerRow)+len(innerRow))
		for k, v := range n.outerRow {
			merged[k] = v
		}
		if !n.colsKnown {
			n.innerCols = nil
		}
		for k, v := range innerRow {
			col := k
			if idx := strings.LastIndex(k, "."); idx >= 0 {
				col = k[idx+1:]
			}
			merged[n.alias+"."+col] = v
			if !n.colsKnown {
				n.innerCols = append(n.innerCols, col)
			}
		}
		n.colsKnown = true

		// Apply ON condition.
		if n.onCond != nil {
			ctx := &EvalCtx{exec: n.exec, outer: n.outerRow, ctes: n.ctes}
			ok, err := evalExpr(n.onCond, merged, ctx)
			if err != nil {
				return nil, err
			}
			if !boolVal(ok) {
				continue
			}
		}
		n.emittedMatch = true
		return merged, nil
	}
}

// lateralColsFromAST derives projected column names from a subquery's SELECT list.
// Used for LEFT LATERAL JOIN null-padding when the lateral subquery returns no rows.
func lateralColsFromAST(subq *parser.SelectStatement) []string {
	if subq == nil || subq.Exprs == nil {
		return nil
	}
	var cols []string
	for _, expr := range subq.Exprs {
		switch ex := expr.(type) {
		case *parser.ColSelectExpr:
			col := ex.Col
			if idx := strings.LastIndex(col, "."); idx >= 0 {
				col = col[idx+1:]
			}
			cols = append(cols, col)
		case *parser.AggSelectExpr:
			cols = append(cols, ex.Func+"("+ex.Arg+")")
		case *parser.ExprSelectExpr:
			if ex.Alias != "" {
				cols = append(cols, ex.Alias)
			}
		}
	}
	return cols
}

// =============================================================================
// subqueryScan — iterates over a derived table's materialized rows
// Same row-emission logic as cteSeqScan but shows "Subquery Scan" in EXPLAIN.
// =============================================================================

type subqueryScan struct {
	nodeBase
	rows  []storage.Row
	alias string
	pos   int
}

func newSubqueryScan(rows []storage.Row, alias string) *subqueryScan {
	return &subqueryScan{rows: rows, alias: alias}
}

func (n *subqueryScan) Open() error          { n.pos = 0; return nil }
func (n *subqueryScan) Close()               {}
func (n *subqueryScan) NodeName() string     { return "Subquery Scan on " + n.alias }
func (n *subqueryScan) NodeChildren() []Node { return nil }

func (n *subqueryScan) Next() (storage.Row, error) {
	if n.pos >= len(n.rows) {
		return nil, nil
	}
	src := n.rows[n.pos]
	n.pos++
	pfx := n.alias + "."
	out := make(storage.Row, len(src))
	for k, v := range src {
		if strings.Contains(k, ".") {
			out[k] = v
		} else {
			out[pfx+k] = v
		}
	}
	return out, nil
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
func (n *instrumentedNode) NodeID() int          { return n.child.NodeID() }

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
