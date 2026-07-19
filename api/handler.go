package api

import (
	"database/internal/executor"
	"database/internal/lexer"
	"database/internal/parser"
	"database/internal/storage"
	"encoding/json"
	"fmt"
	"net/http"
)

type QueryRequest struct {
	SQL string `json:"sql"`
}

type TraceToken struct {
	TypeName string `json:"typeName"`
	Literal  string `json:"literal"`
	Category string `json:"category"`
}

type QueryResponse struct {
	Columns   []string        `json:"columns,omitempty"`
	Rows      [][]interface{} `json:"rows,omitempty"`
	Message   string          `json:"message,omitempty"`
	Error     string          `json:"error,omitempty"`
	Tokens    []TraceToken    `json:"tokens,omitempty"`
	AST       interface{}     `json:"ast,omitempty"`
	ExecTrace []string        `json:"execTrace,omitempty"`
}

func QueryHandler(db *storage.Database) http.HandlerFunc {
	exec := executor.New(db)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, QueryResponse{Error: "only POST allowed"})
			return
		}

		var req QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, QueryResponse{Error: "invalid request body"})
			return
		}
		if req.SQL == "" {
			writeJSON(w, QueryResponse{Error: "sql is required"})
			return
		}

		// Stage 1: Lex
		tokens := lexer.New(req.SQL).Tokenize()
		traceTokens := serializeTokens(tokens)

		// Stage 2: Parse
		stmt, err := parser.New(tokens).Parse()
		if err != nil {
			writeJSON(w, QueryResponse{
				Error:  "parse error: " + err.Error(),
				Tokens: traceTokens,
			})
			return
		}
		ast := stmtToTrace(stmt)

		// Stage 3: Execute
		result, err := exec.Execute(stmt)
		if err != nil {
			writeJSON(w, QueryResponse{
				Error:  err.Error(),
				Tokens: traceTokens,
				AST:    ast,
			})
			return
		}

		writeJSON(w, QueryResponse{
			Columns:   result.Columns,
			Rows:      result.Rows,
			Message:   result.Message,
			Tokens:    traceTokens,
			AST:       ast,
			ExecTrace: result.Trace,
		})
	}
}

func serializeTokens(tokens []lexer.Token) []TraceToken {
	var out []TraceToken
	for _, t := range tokens {
		if t.Type == lexer.EOF || t.Type == lexer.SEMICOLON {
			continue
		}
		lit := t.Literal
		if t.Type == lexer.STRING_LIT {
			lit = fmt.Sprintf("'%s'", t.Literal)
		}
		out = append(out, TraceToken{
			TypeName: t.Type.Name(),
			Literal:  lit,
			Category: t.Type.Category(),
		})
	}
	return out
}

// stmtToTrace converts an AST statement into a plain map for JSON serialization.
func stmtToTrace(stmt parser.Statement) map[string]interface{} {
	switch s := stmt.(type) {
	case *parser.SelectStatement:
		var exprList interface{} = "*"
		if s.Exprs != nil {
			var items []interface{}
			for _, ex := range s.Exprs {
				switch e := ex.(type) {
				case *parser.ColSelectExpr:
					items = append(items, e.Col)
				case *parser.AggSelectExpr:
					items = append(items, e.Func+"("+e.Arg+")")
				}
			}
			exprList = items
		}
		var joins []map[string]interface{}
		for _, j := range s.Joins {
			joins = append(joins, map[string]interface{}{
				"type":  string(j.Type),
				"table": j.Table,
				"alias": j.Alias,
				"on":    exprToTrace(j.Condition),
			})
		}
		var orderBy []map[string]interface{}
		for _, ob := range s.OrderBy {
			dir := "ASC"
			if ob.Desc {
				dir = "DESC"
			}
			orderBy = append(orderBy, map[string]interface{}{"col": ob.Col, "dir": dir})
		}
		return map[string]interface{}{
			"type":     "SelectStatement",
			"distinct": s.Distinct,
			"table":    s.Table,
			"alias":    s.Alias,
			"joins":    joins,
			"columns":  exprList,
			"where":    exprToTrace(s.Where),
			"groupBy":  s.GroupBy,
			"having":   exprToTrace(s.Having),
			"orderBy":  orderBy,
			"limit":    s.Limit,
			"offset":   s.Offset,
		}
	case *parser.InsertStatement:
		cols := interface{}("<positional>")
		if len(s.Columns) > 0 {
			cols = s.Columns
		}
		return map[string]interface{}{
			"type":    "InsertStatement",
			"table":   s.Table,
			"columns": cols,
			"values":  s.Values,
		}
	case *parser.UpdateStatement:
		return map[string]interface{}{
			"type":        "UpdateStatement",
			"table":       s.Table,
			"assignments": s.Assignments,
			"where":       exprToTrace(s.Where),
		}
	case *parser.DeleteStatement:
		return map[string]interface{}{
			"type":  "DeleteStatement",
			"table": s.Table,
			"where": exprToTrace(s.Where),
		}
	case *parser.CreateTableStatement:
		var cols []map[string]interface{}
		for _, c := range s.Columns {
			cols = append(cols, map[string]interface{}{
				"name":    c.Name,
				"type":    c.Type,
				"primary": c.Primary,
			})
		}
		return map[string]interface{}{
			"type":    "CreateTableStatement",
			"table":   s.Table,
			"columns": cols,
		}
	case *parser.DropTableStatement:
		return map[string]interface{}{
			"type":  "DropTableStatement",
			"table": s.Table,
		}
	case *parser.ExplainStatement:
		mode := "EXPLAIN"
		if s.Analyze {
			mode = "EXPLAIN ANALYZE"
		}
		return map[string]interface{}{
			"type":  "ExplainStatement",
			"mode":  mode,
			"inner": stmtToTrace(s.Stmt),
		}
	case *parser.CreateIndexStatement:
		return map[string]interface{}{
			"type":   "CreateIndex",
			"name":   s.Name,
			"table":  s.Table,
			"column": s.Column,
		}
	case *parser.DropIndexStatement:
		return map[string]interface{}{
			"type":     "DropIndex",
			"name":     s.Name,
			"ifExists": s.IfExists,
		}
	case *parser.AnalyzeStatement:
		return map[string]interface{}{
			"type":  "AnalyzeStatement",
			"table": s.Table,
		}
	}
	return map[string]interface{}{"type": "Unknown"}
}

func exprToTrace(expr parser.Expression) interface{} {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *parser.BinaryExpr:
		return map[string]interface{}{
			"type":  "BinaryExpr",
			"op":    e.Op,
			"left":  exprToTrace(e.Left),
			"right": exprToTrace(e.Right),
		}
	case *parser.IdentExpr:
		m := map[string]interface{}{"type": "IdentExpr", "name": e.Name}
		if e.Table != "" {
			m["table"] = e.Table
		}
		return m
	case *parser.LiteralExpr:
		return map[string]interface{}{
			"type":  "LiteralExpr",
			"value": e.Value,
		}
	case *parser.AggFuncExpr:
		arg := interface{}("*")
		if e.Arg != nil {
			arg = exprToTrace(e.Arg)
		}
		return map[string]interface{}{
			"type": "AggFuncExpr",
			"func": e.Func,
			"arg":  arg,
		}
	}
	return nil
}

func TablesHandler(db *storage.Database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		type ColInfo struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Primary bool   `json:"primary"`
		}
		type TblInfo struct {
			Name     string    `json:"name"`
			Columns  []ColInfo `json:"columns"`
			RowCount int       `json:"rowCount"`
		}

		var result []TblInfo
		for _, t := range db.ListTables() {
			var cols []ColInfo
			for _, c := range t.Columns {
				cols = append(cols, ColInfo{Name: c.Name, Type: string(c.Type), Primary: c.Primary})
			}
			result = append(result, TblInfo{Name: t.Name, Columns: cols, RowCount: t.RowCount})
		}
		writeJSON(w, map[string]interface{}{"tables": result})
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	json.NewEncoder(w).Encode(v)
}
