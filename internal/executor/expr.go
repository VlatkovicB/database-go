package executor

// =============================================================================
// Expression evaluation
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"strings"
)

// aggSpec names an aggregate function and its argument column.
type aggSpec struct{ fn, arg string }

// EvalCtx carries subquery execution context (executor, outer row for correlated queries, CTEs).
type EvalCtx struct {
	exec  *Executor
	outer storage.Row
	ctes  map[string]*cteEntry
}

// evalExpr evaluates an expression against a row and returns the result value.
// ctx is nil for simple (non-subquery) expressions and is safe to pass as nil.
func evalExpr(expr parser.Expression, row storage.Row, ctx *EvalCtx) (interface{}, error) {
	switch e := expr.(type) {
	case *parser.IdentExpr:
		if e.Table != "" {
			key := e.Table + "." + e.Name
			if val, ok := row[key]; ok {
				return val, nil
			}
			// Correlated: fall back to outer row
			if ctx != nil && ctx.outer != nil {
				if val, ok := ctx.outer[key]; ok {
					return val, nil
				}
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
		// Correlated: fall back to outer row
		if ctx != nil && ctx.outer != nil {
			if val, ok := ctx.outer[e.Name]; ok {
				return val, nil
			}
			for k, v := range ctx.outer {
				if strings.HasSuffix(k, suffix) {
					return v, nil
				}
			}
		}
		return nil, fmt.Errorf("column %q not found", e.Name)
	case *parser.LiteralExpr:
		return e.Value, nil
	case *parser.BinaryExpr:
		return evalBinary(e, row, ctx)
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

	case *parser.SubqueryExpr:
		if ctx == nil || ctx.exec == nil {
			return nil, fmt.Errorf("scalar subquery requires execution context")
		}
		subCtx := &EvalCtx{exec: ctx.exec, outer: row, ctes: ctx.ctes}
		rows, err := ctx.exec.materializeSubquery(e.Query, subCtx)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		if len(rows) > 1 {
			return nil, fmt.Errorf("scalar subquery returned more than one row")
		}
		for _, v := range rows[0] {
			return v, nil
		}
		return nil, nil

	case *parser.InSubqueryExpr:
		if ctx == nil || ctx.exec == nil {
			return nil, fmt.Errorf("IN subquery requires execution context")
		}
		leftVal, err := evalExpr(e.Left, row, ctx)
		if err != nil {
			return nil, err
		}
		var matches bool
		if e.Query != nil {
			subCtx := &EvalCtx{exec: ctx.exec, outer: row, ctes: ctx.ctes}
			rows, err := ctx.exec.materializeSubquery(e.Query, subCtx)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				for _, v := range r {
					eq, _ := compare(leftVal, "=", v)
					if eq {
						matches = true
						break
					}
				}
				if matches {
					break
				}
			}
		} else {
			for _, valExpr := range e.Values {
				v, err := evalExpr(valExpr, row, ctx)
				if err != nil {
					return nil, err
				}
				eq, _ := compare(leftVal, "=", v)
				if eq {
					matches = true
					break
				}
			}
		}
		if e.Not {
			return !matches, nil
		}
		return matches, nil

	case *parser.ExistsExpr:
		if ctx == nil || ctx.exec == nil {
			return nil, fmt.Errorf("EXISTS requires execution context")
		}
		subCtx := &EvalCtx{exec: ctx.exec, outer: row, ctes: ctx.ctes}
		rows, err := ctx.exec.materializeSubquery(e.Query, subCtx)
		if err != nil {
			return nil, err
		}
		exists := len(rows) > 0
		if e.Not {
			return !exists, nil
		}
		return exists, nil
	}
	return nil, fmt.Errorf("unknown expression type %T", expr)
}

func evalBinary(e *parser.BinaryExpr, row storage.Row, ctx *EvalCtx) (interface{}, error) {
	if e.Op == "AND" {
		left, err := evalExpr(e.Left, row, ctx)
		if err != nil {
			return nil, err
		}
		if !boolVal(left) {
			return false, nil
		}
		return evalExpr(e.Right, row, ctx)
	}
	if e.Op == "OR" {
		left, err := evalExpr(e.Left, row, ctx)
		if err != nil {
			return nil, err
		}
		if boolVal(left) {
			return true, nil
		}
		return evalExpr(e.Right, row, ctx)
	}

	left, err := evalExpr(e.Left, row, ctx)
	if err != nil {
		return nil, err
	}
	right, err := evalExpr(e.Right, row, ctx)
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
