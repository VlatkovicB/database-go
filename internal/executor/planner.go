package executor

// =============================================================================
// Cost-based query planner (Phase 7)
// =============================================================================
//
// Separates planning from execution: the planner produces a physRelation tree
// that captures the cheapest physical operators, then physRelToVolcano converts
// it into a live volcano node tree. This mirrors PostgreSQL's plan/execute split.
//
// Cost model (PG-style constants):
//   Seq Scan:   pages * seqPageCost + rows * cpuTupleCost
//   Index Scan: depth * randPageCost + matchRows * (randPageCost + cpuIndexCost)
//   Nested Loop: left.total + left.rows * right.total + output * cpuTupleCost
//   Hash Join:  (right.total + right.rows * cpuOpCost)   ← build phase
//             + (left.total  + left.rows  * cpuOpCost)   ← probe phase
//             + output * cpuTupleCost

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"math"
	"runtime"
)

// PG-style cost constants.
const (
	seqPageCost      = 1.0
	randPageCost     = 4.0
	cpuTupleCost     = 0.01
	cpuOperatorCost  = 0.0025
	cpuIndexCost     = 0.005
	planWidth        = 64
	parallelSetupCost = 10.0 // fixed overhead for Gather (goroutine startup, not PG process fork)
	parallelMinRows   = 1000.0 // minimum estimated rows before parallel scan is considered
	maxParallelWorkers = 4     // cap on parallel worker goroutines
)

type physScanType int

const (
	physSeqScan  physScanType = iota
	physIndexScan
)

type physJoinAlg int

const (
	physNestedLoop physJoinAlg = iota
	physHashJoin
)

// physRelation is one node in the physical plan tree.
// Leaf nodes hold a single-table scan; non-leaf nodes hold a join of two relations.
type physRelation struct {
	// Leaf (scan) fields — set when left == nil.
	table    string
	alias    string
	scanType physScanType
	idxPlan  *indexPlan       // non-nil for physIndexScan
	filter   parser.Expression // residual WHERE after the index predicate

	// Join fields — set when left != nil.
	joinAlg  physJoinAlg
	joinType parser.JoinType
	joinCond parser.Expression
	left     *physRelation
	right    *physRelation

	// Cost estimates (PG convention: startup + total cost).
	startupCost     float64
	totalCost       float64
	estRows         float64
	width           int
	parallelWorkers int // >0 means Gather + Parallel Seq Scan
}

func (r *physRelation) isJoin() bool { return r.left != nil }

// tableRef describes one table participating in a query.
type tableRef struct {
	table    string
	alias    string
	joinType parser.JoinType
	cond     parser.Expression // ON condition; nil for the driving (base) table
}

// qplanner holds state for one planning session.
type qplanner struct {
	e    *Executor
	db   *storage.Database
	ctes map[string]*cteEntry
}

func newQPlanner(e *Executor, ctes map[string]*cteEntry) *qplanner {
	return &qplanner{e: e, db: e.db, ctes: ctes}
}

// planScan returns the cheapest physical access path for a single table.
// where is non-nil only for single-table queries; the planner uses it to
// select an index scan when one exists and is cheaper than a seq scan.
func (p *qplanner) planScan(table, alias string, where parser.Expression) *physRelation {
	// CTE table reference — return a lightweight estimate.
	if p.ctes != nil {
		if entry, ok := p.ctes[table]; ok {
			estRows := math.Max(1, float64(len(entry.rows)))
			return &physRelation{
				table:     table,
				alias:     alias,
				scanType:  physSeqScan,
				filter:    where,
				estRows:   estRows,
				totalCost: estRows * cpuTupleCost,
				width:     planWidth,
			}
		}
	}

	rows, _ := p.db.RowCount(table)
	pages, _ := p.db.PageCount(table)
	if pages < 1 {
		pages = 1
	}
	stats := p.db.GetTableStats(table)

	sel := 1.0
	if where != nil {
		sel = selectivityExpr(where, table, stats)
	}
	estRows := math.Max(1, float64(rows)*sel)
	seqCost := float64(pages)*seqPageCost + float64(rows)*cpuTupleCost

	// Default: Seq Scan with a filter node on top if there's a WHERE predicate.
	best := &physRelation{
		table:     table,
		alias:     alias,
		scanType:  physSeqScan,
		filter:    where,
		estRows:   estRows,
		totalCost: seqCost,
		width:     planWidth,
	}

	// Index Scan: only considered when the WHERE clause is indexable.
	// Cost model for in-memory B-tree (no disk I/O, tuples stored in index directly):
	//   startup:  depth * seqPageCost * 0.1  (pointer chasing down B-tree levels)
	//   pages:    proportional fraction of pages containing matching tuples
	//   per-row:  cpuIndexCost per key comparison
	if where != nil {
		if ip := p.e.findIndexPlan(table, where); ip != nil {
			depth := p.db.GetIndexDepth(table, ip.indexName)
			idxSel := selectivityExpr(where, table, stats)
			idxRows := math.Max(1, float64(rows)*idxSel)
			selectivityRatio := idxRows / math.Max(1, float64(rows))
			idxCost := float64(depth)*seqPageCost*0.1 +
				float64(pages)*selectivityRatio*seqPageCost +
				idxRows*cpuIndexCost
			if idxCost < best.totalCost {
				best = &physRelation{
					table:     table,
					alias:     alias,
					scanType:  physIndexScan,
					idxPlan:   ip,
					filter:    ip.residual, // residual after the index predicate
					estRows:   idxRows,
					totalCost: idxCost,
					width:     planWidth,
				}
			}
		}
	}

	// Parallel SeqScan: cheaper than serial when table is large enough.
	// Cost model: startup=parallelSetupCost, total=parallelSetupCost + seqCost/nWorkers
	// Mirrors PG's parallel_setup_cost + (seqCost / max_parallel_workers_per_gather).
	if best.scanType == physSeqScan && best.estRows > parallelMinRows {
		nWorkers := runtime.NumCPU()
		if nWorkers > maxParallelWorkers {
			nWorkers = maxParallelWorkers
		}
		parallelTotal := parallelSetupCost + seqCost/float64(nWorkers)
		if parallelTotal < best.totalCost {
			best = &physRelation{
				table:           best.table,
				alias:           best.alias,
				scanType:        physSeqScan,
				filter:          best.filter,
				estRows:         best.estRows,
				startupCost:     parallelSetupCost,
				totalCost:       parallelTotal,
				width:           best.width,
				parallelWorkers: nWorkers,
			}
		}
	}

	return best
}

// planJoinPair returns the cheaper of Nested Loop Join and Hash Join for two
// relations. For INNER JOINs it also tries the reversed order (right ⋈ left)
// and returns the overall cheapest plan.
func (p *qplanner) planJoinPair(left, right *physRelation, joinType parser.JoinType, cond parser.Expression) *physRelation {
	// PG default join selectivity when no column stats span both tables.
	const joinSel = 0.1
	outRows := math.Max(1, left.estRows*right.estRows*joinSel)

	// Nested Loop: for each outer row, rescan the materialised inner.
	nlTotal := left.totalCost + left.estRows*right.totalCost + outRows*cpuTupleCost
	nl := &physRelation{
		joinAlg:     physNestedLoop,
		joinType:    joinType,
		joinCond:    cond,
		left:        left,
		right:       right,
		startupCost: left.startupCost,
		totalCost:   nlTotal,
		estRows:     outRows,
		width:       left.width + right.width,
	}

	// Hash Join: build a hash table from the inner side, then probe with outer.
	buildCost := right.totalCost + right.estRows*cpuOperatorCost
	probeCost := left.totalCost + left.estRows*cpuOperatorCost
	hjTotal := buildCost + probeCost + outRows*cpuTupleCost
	hj := &physRelation{
		joinAlg:     physHashJoin,
		joinType:    joinType,
		joinCond:    cond,
		left:        left,
		right:       right,
		startupCost: buildCost,
		totalCost:   hjTotal,
		estRows:     outRows,
		width:       left.width + right.width,
	}

	if hj.totalCost < nl.totalCost {
		return hj
	}
	return nl
}

// planRelations chooses the cheapest join order and algorithm for a list of tables.
// Two-table INNER JOINs try both orderings; larger sets use a greedy left-deep
// strategy (the same approach PG uses for small relation counts).
func (p *qplanner) planRelations(refs []tableRef, singleTableWhere parser.Expression) *physRelation {
	if len(refs) == 0 {
		return nil
	}

	scans := make([]*physRelation, len(refs))
	for i, ref := range refs {
		// Pass WHERE only for single-table queries; the planner uses it for
		// index selection. In multi-table queries the WHERE is applied as a
		// filter node above the join tree (matching current executor behaviour).
		w := parser.Expression(nil)
		if i == 0 && len(refs) == 1 {
			w = singleTableWhere
		}
		scans[i] = p.planScan(ref.table, ref.alias, w)
	}

	if len(refs) == 1 {
		return scans[0]
	}

	result := scans[0]
	for i := 1; i < len(refs); i++ {
		ref := refs[i]
		right := scans[i]

		opt := p.planJoinPair(result, right, ref.joinType, ref.cond)

		// For INNER JOINs, also price the reversed order and take the winner.
		if ref.joinType == parser.InnerJoin {
			if swap := p.planJoinPair(right, result, ref.joinType, ref.cond); swap.totalCost < opt.totalCost {
				opt = swap
			}
		}
		result = opt
	}
	return result
}

// buildTableRefs converts a SelectStatement's table + joins into a flat tableRef slice.
// LATERAL joins are excluded — they're handled separately as lateralJoin volcano nodes.
func buildTableRefs(sel *parser.SelectStatement) []tableRef {
	alias := sel.Alias
	if alias == "" {
		alias = sel.Table
	}
	refs := []tableRef{{table: sel.Table, alias: alias}}
	for _, j := range sel.Joins {
		if j.Lateral {
			continue
		}
		ja := j.Alias
		if ja == "" {
			ja = j.Table
		}
		refs = append(refs, tableRef{
			table:    j.Table,
			alias:    ja,
			joinType: j.Type,
			cond:     j.Condition,
		})
	}
	return refs
}

// physRelToVolcano converts a physRelation tree into a live volcano node tree.
func physRelToVolcano(rel *physRelation, db *storage.Database, snap *storage.Snapshot, xid uint64, logger *ExecLogger, ctes map[string]*cteEntry, lockMode storage.LockMode, lockMgr *storage.LockManager) Node {
	assign := func(n Node) Node {
		if l, ok := n.(loggable); ok {
			l.setLog(logger, logger.NextID())
		}
		return n
	}

	if !rel.isJoin() {
		// CTE / derived table check.
		if ctes != nil {
			if entry, ok := ctes[rel.table]; ok {
				var n Node
				if entry.derived {
					n = assign(newSubqueryScan(entry.rows, rel.alias))
				} else {
					n = assign(newCTESeqScan(entry.rows, rel.alias))
				}
				if rel.filter != nil {
					n = assign(newFilterNode(n, rel.filter))
				}
				return n
			}
		}
		var n Node
		if rel.scanType == physIndexScan {
			ip := rel.idxPlan
			n = assign(newIndexScan(db, rel.table, rel.alias, ip.indexName, ip.column, ip.lo, ip.loOp, ip.hi, ip.hiOp, snap, xid))
		} else if rel.parallelWorkers > 0 {
			n = assign(newParallelSeqScan(db, rel.table, rel.alias, snap, xid, rel.parallelWorkers))
		} else {
			n = assign(newSeqScan(db, rel.table, rel.alias, snap, xid, lockMode, lockMgr))
		}
		if rel.filter != nil {
			n = assign(newFilterNode(n, rel.filter))
		}
		return n
	}

	left := physRelToVolcano(rel.left, db, snap, xid, logger, ctes, lockMode, lockMgr)
	right := physRelToVolcano(rel.right, db, snap, xid, logger, ctes, lockMode, lockMgr)

	// Provide the right side's alias for hash key extraction (only valid when
	// the right side is itself a leaf scan, not a compound join).
	innerAlias := ""
	if !rel.right.isJoin() {
		innerAlias = rel.right.alias
	}

	var n Node
	if rel.joinAlg == physHashJoin {
		n = assign(newHashJoin(left, right, innerAlias, rel.joinCond, rel.joinType))
	} else {
		n = assign(newNestedLoopJoin(left, right, rel.joinCond, rel.joinType))
	}
	return n
}

// physRelToPlanNode converts a physRelation tree into a planNode tree for EXPLAIN.
func physRelToPlanNode(rel *physRelation, db *storage.Database, ctes map[string]*cteEntry) *planNode {
	if !rel.isJoin() {
		// CTE / derived table label for EXPLAIN.
		if ctes != nil {
			if entry, ok := ctes[rel.table]; ok {
				label := "CTE Scan on " + rel.table
				if entry.derived {
					label = "Subquery Scan on " + rel.table
				}
				return &planNode{
					label:    label,
					estTotal: rel.totalCost,
					estRows:  int(math.Max(1, rel.estRows)),
					width:    rel.width,
				}
			}
		}
		label := "Seq Scan on " + rel.table
		var extras []string
		if rel.scanType == physIndexScan {
			label = fmt.Sprintf("Index Scan using %s on %s", rel.idxPlan.indexName, rel.table)
			extras = append(extras, fmt.Sprintf("Index Cond: (%s.%s)", rel.table, rel.idxPlan.column))
			if rel.filter != nil {
				extras = append(extras, "Filter: "+exprToSQL(rel.filter))
			}
		} else {
			pc, _ := db.PageCount(rel.table)
			if pc < 1 {
				pc = 1
			}
			if rel.parallelWorkers > 0 {
				// Two-level plan: Gather → Parallel Seq Scan (mirrors PG output).
				var innerExtras []string
				innerExtras = append(innerExtras, fmt.Sprintf("Heap Pages: %d", pc))
				if rel.filter != nil {
					innerExtras = append(innerExtras, "Filter: "+exprToSQL(rel.filter))
				}
				innerNode := &planNode{
					label:    "Parallel Seq Scan on " + rel.table,
					estTotal: (rel.totalCost - parallelSetupCost) * float64(rel.parallelWorkers),
					estRows:  int(math.Max(1, rel.estRows)),
					width:    rel.width,
					extras:   innerExtras,
				}
				return &planNode{
					label:      "Gather",
					estStartup: parallelSetupCost,
					estTotal:   rel.totalCost,
					estRows:    int(math.Max(1, rel.estRows)),
					width:      rel.width,
					extras:     []string{fmt.Sprintf("Workers Planned: %d", rel.parallelWorkers)},
					children:   []*planNode{innerNode},
				}
			}
			extras = append(extras, fmt.Sprintf("Heap Pages: %d", pc))
			if rel.filter != nil {
				extras = append(extras, "Filter: "+exprToSQL(rel.filter))
			}
		}
		return &planNode{
			label:    label,
			estTotal: rel.totalCost,
			estRows:  int(math.Max(1, rel.estRows)),
			width:    rel.width,
			extras:   extras,
		}
	}

	leftNode := physRelToPlanNode(rel.left, db, ctes)
	rightNode := physRelToPlanNode(rel.right, db, ctes)

	algLabel := "Nested Loop"
	if rel.joinAlg == physHashJoin {
		algLabel = "Hash Join"
	}
	if rel.joinType == parser.LeftJoin {
		algLabel += " Left Join"
	}

	condKey := "Join Filter"
	if rel.joinAlg == physHashJoin {
		condKey = "Hash Cond"
	}

	var extras []string
	if rel.joinCond != nil {
		extras = append(extras, condKey+": "+exprToSQL(rel.joinCond))
	}

	return &planNode{
		label:       algLabel,
		estStartup:  rel.startupCost,
		estTotal:    rel.totalCost,
		estRows:     int(math.Max(1, rel.estRows)),
		width:       rel.width,
		extras:      extras,
		children:    []*planNode{leftNode, rightNode},
	}
}
