package executor

// =============================================================================
// Selectivity estimation
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
)

// Selectivity defaults mirror PostgreSQL's hardcoded fractions (used when no ANALYZE stats exist).
const (
	defaultEqSel    = 0.005
	defaultNeqSel   = 0.995
	defaultRangeSel = 1.0 / 3.0
)

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
	return defaultRangeSel // default: no stats, no match
}

func selectivityCmp(b *parser.BinaryExpr, tableName string, ts *storage.TableStats) float64 {
	ident, lit, op := extractIdentLit(b)
	if ident == nil || lit == nil {
		return defaultRangeSel
	}

	if ts == nil {
		// No stats — use PostgreSQL-style defaults.
		switch op {
		case "=":
			return defaultEqSel
		case "!=":
			return defaultNeqSel
		default:
			return defaultRangeSel
		}
	}

	colName := ident.Name
	cs := ts.Columns[colName]
	if cs == nil {
		switch op {
		case "=":
			return defaultEqSel
		case "!=":
			return defaultNeqSel
		default:
			return defaultRangeSel
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
		return defaultRangeSel
	}
	return defaultRangeSel
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
