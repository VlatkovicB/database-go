package executor

import (
	"database/internal/parser"
	"database/internal/storage"
)

// cteEntry holds materialized data for one CTE or derived table.
// derived=true for FROM/JOIN subqueries; false for WITH CTEs.
type cteEntry struct {
	rows    []storage.Row
	cols    []string
	keys    []string
	derived bool
}

// materializeSubquery executes subquery q and returns matching rows.
// ctx.outer resolves correlated column references from the outer query.
// ctx.ctes provides materialized CTE data for CTE table references.
//
// For simple subqueries (no aggregates, no joins), uses a fast correlated scan.
// For aggregate subqueries, scans the inner table with correlated WHERE, then
// computes aggregates on the filtered rows — this correctly handles correlated
// references in WHERE (e.g. WHERE attacker_id = players.id).
func (e *Executor) materializeSubquery(q *parser.SelectStatement, ctx *EvalCtx) ([]storage.Row, error) {
	hasAgg := hasAggExprs(q.Exprs) || len(q.GroupBy) > 0
	hasJoins := len(q.Joins) > 0

	if !hasAgg && !hasJoins {
		return e.materializeSubquerySimple(q, ctx)
	}

	// For aggregate subqueries: scan inner table with correlated filter,
	// then compute aggregates on the filtered rows.
	// This correctly handles correlated WHERE like: WHERE attacker_id = players.id.
	if hasAgg && !hasJoins {
		return e.materializeAggSubquery(q, ctx)
	}

	// For join subqueries (non-aggregate): use full pipeline.
	// Correlated join subqueries are uncommon in practice; we build the plan
	// and inject a top-level correlated filter for the WHERE if needed.
	plan, err := e.planSelectWithCTEs(q, ctesFrom(ctx))
	if err != nil {
		return nil, err
	}

	// Wrap root with a correlated filter if there's a WHERE and an outer row.
	root := plan.root
	if q.Where != nil && ctx != nil && ctx.outer != nil {
		root = newFilterNodeWithCtx(root, q.Where, ctx)
	}

	if err := root.Open(); err != nil {
		return nil, err
	}
	defer root.Close()

	var results []storage.Row
	for {
		row, err := root.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		out := make(storage.Row)
		for _, key := range plan.keys {
			out[key] = row[key]
		}
		results = append(results, out)
	}
	return results, nil
}

// materializeAggSubquery handles aggregate subqueries with correlated WHERE.
// It scans the inner table, applies the correlated WHERE, then computes aggregates.
func (e *Executor) materializeAggSubquery(q *parser.SelectStatement, ctx *EvalCtx) ([]storage.Row, error) {
	snap := e.currentSnapshot()
	xid := e.currentXID()

	alias := q.Alias
	if alias == "" {
		alias = q.Table
	}

	// Collect all filtered rows.
	var filteredRows []storage.Row

	// CTE table reference?
	if ctx != nil && ctx.ctes != nil {
		if entry, ok := ctx.ctes[q.Table]; ok {
			for _, row := range entry.rows {
				if q.Where != nil {
					ok2, err := evalExpr(q.Where, row, ctx)
					if err != nil || !boolVal(ok2) {
						continue
					}
				}
				filteredRows = append(filteredRows, row)
			}
		}
	} else {
		scan := newSeqScan(e.db, q.Table, alias, snap, xid, storage.NoLock, nil)
		if err := scan.Open(); err != nil {
			return nil, err
		}
		for {
			row, err := scan.Next()
			if err != nil {
				scan.Close()
				return nil, err
			}
			if row == nil {
				break
			}
			if q.Where != nil {
				ok, err := evalExpr(q.Where, row, ctx)
				if err != nil || !boolVal(ok) {
					continue
				}
			}
			filteredRows = append(filteredRows, row)
		}
		scan.Close()
	}

	// Compute aggregates on the filtered rows.
	sr := storage.Row{}

	// Collect needed aggregate specs.
	needed := map[string]aggSpec{}
	for _, expr := range q.Exprs {
		if agg, ok := expr.(*parser.AggSelectExpr); ok {
			k := agg.Func + "(" + agg.Arg + ")"
			needed[k] = aggSpec{agg.Func, agg.Arg}
		}
	}

	for key, spec := range needed {
		val, err := computeAggFromRows(spec.fn, spec.arg, filteredRows)
		if err != nil {
			return nil, err
		}
		sr[key] = val
	}

	// Also handle ExprSelectExpr literals (like SELECT 1 — just return 1 row).
	hasExprExprs := false
	for _, expr := range q.Exprs {
		if _, ok := expr.(*parser.ExprSelectExpr); ok {
			hasExprExprs = true
		}
	}
	if hasExprExprs && len(needed) == 0 {
		// SELECT 1 with aggregation context — just return the literal.
		for _, expr := range q.Exprs {
			if ex, ok := expr.(*parser.ExprSelectExpr); ok {
				val, err := evalExpr(ex.Expr, storage.Row{}, ctx)
				if err == nil {
					sr[ex.Alias] = val
				}
			}
		}
	}

	if len(sr) == 0 {
		return nil, nil
	}
	return []storage.Row{sr}, nil
}

// materializeSubquerySimple handles simple (non-aggregate, non-join) subqueries,
// including correlated WHERE clauses.
func (e *Executor) materializeSubquerySimple(q *parser.SelectStatement, ctx *EvalCtx) ([]storage.Row, error) {
	snap := e.currentSnapshot()
	xid := e.currentXID()

	alias := q.Alias
	if alias == "" {
		alias = q.Table
	}

	// CTE table reference?
	if ctx != nil && ctx.ctes != nil {
		if entry, ok := ctx.ctes[q.Table]; ok {
			var results []storage.Row
			for _, row := range entry.rows {
				if q.Where != nil {
					ok2, err := evalExpr(q.Where, row, ctx)
					if err != nil || !boolVal(ok2) {
						continue
					}
				}
				results = append(results, row)
			}
			return results, nil
		}
	}

	// Regular table scan.
	scan := newSeqScan(e.db, q.Table, alias, snap, xid, storage.NoLock, nil)
	if err := scan.Open(); err != nil {
		return nil, err
	}
	defer scan.Close()

	var results []storage.Row
	for {
		row, err := scan.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		if q.Where != nil {
			ok, err := evalExpr(q.Where, row, ctx)
			if err != nil || !boolVal(ok) {
				continue
			}
		}
		results = append(results, row)
	}
	return results, nil
}

// ctesFrom extracts the ctes map from an EvalCtx (nil-safe).
func ctesFrom(ctx *EvalCtx) map[string]*cteEntry {
	if ctx == nil {
		return nil
	}
	return ctx.ctes
}

// materializeDerivedTable executes subq and returns a cteEntry for use as a derived table.
// outerCTEs are passed through so the subquery can reference CTEs from the outer query.
func (e *Executor) materializeDerivedTable(subq *parser.SelectStatement, outerCTEs map[string]*cteEntry) (*cteEntry, error) {
	sub, err := e.planSelectWithCTEs(subq, outerCTEs)
	if err != nil {
		return nil, err
	}
	if err := sub.root.Open(); err != nil {
		return nil, err
	}
	defer sub.root.Close()

	var rows []storage.Row
	for {
		row, err := sub.root.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		projected := make(storage.Row)
		for i, key := range sub.keys {
			projected[sub.cols[i]] = row[key]
		}
		rows = append(rows, projected)
	}
	return &cteEntry{rows: rows, cols: sub.cols, keys: sub.cols, derived: true}, nil
}
