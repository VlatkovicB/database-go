package api

import (
	"database/internal/executor"
	"database/internal/lexer"
	"database/internal/parser"
	"database/internal/storage"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type QueryRequest struct {
	SQL       string `json:"sql"`
	SessionID string `json:"session_id,omitempty"`
}

type TraceToken struct {
	TypeName string `json:"typeName"`
	Literal  string `json:"literal"`
	Category string `json:"category"`
}

type QueryResponse struct {
	Columns       []string               `json:"columns,omitempty"`
	Rows          [][]interface{}        `json:"rows,omitempty"`
	Message       string                 `json:"message,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Tokens        []TraceToken           `json:"tokens,omitempty"`
	AST           interface{}            `json:"ast,omitempty"`
	ExecTrace     []string               `json:"execTrace,omitempty"`
	SessionID     string                 `json:"session_id,omitempty"`
	StepLog          []executor.StepEvent     `json:"stepLog,omitempty"`
	NodeTree         *executor.NodeTreeDesc   `json:"nodeTree,omitempty"`
	StepTruncated    bool                     `json:"stepTruncated,omitempty"`
	IndexSuggestions []executor.IndexSuggestion `json:"indexSuggestions,omitempty"`
}

// Handler holds the shared database and an in-memory session store.
// Each session maps a session_id string to a stateful Executor that has an
// open transaction (BEGIN has been called but COMMIT/ROLLBACK not yet).
type Handler struct {
	db       *storage.Database
	history  *HistoryStore
	mu       sync.Mutex
	sessions map[string]*executor.Executor
}

func NewHandler(db *storage.Database, history *HistoryStore) *Handler {
	return &Handler{
		db:       db,
		history:  history,
		sessions: make(map[string]*executor.Executor),
	}
}

// QueryHandler returns an http.HandlerFunc backed by a Handler with session support.
func QueryHandler(db *storage.Database, history *HistoryStore) http.HandlerFunc {
	h := NewHandler(db, history)
	return h.handleQuery
}

// HistoryHandler handles GET /history.
func HistoryHandler(history *HistoryStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		queries, err := history.List()
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if queries == nil {
			queries = []string{}
		}
		writeJSON(w, map[string]interface{}{"queries": queries})
	}
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
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

	// Stage 3: Resolve executor (session-aware)
	var exec *executor.Executor
	var sessionID string

	switch stmt.(type) {
	case *parser.BeginStatement:
		// BEGIN: always create a fresh executor, assign session after Begin sets CurrentTx.
		exec = executor.New(h.db)
		result, err := exec.Execute(stmt)
		if err != nil {
			writeJSON(w, QueryResponse{Error: err.Error(), Tokens: traceTokens, AST: ast})
			return
		}
		// Store session keyed by tx ID.
		sessionID = fmt.Sprintf("tx-%d", exec.CurrentTx.ID)
		h.mu.Lock()
		h.sessions[sessionID] = exec
		h.mu.Unlock()
		if h.history != nil {
			go h.history.Upsert(req.SQL)
		}
		writeJSON(w, QueryResponse{
			Message:   result.Message,
			Tokens:    traceTokens,
			AST:       ast,
			ExecTrace: result.Trace,
			SessionID: sessionID,
		})
		return

	case *parser.CommitStatement, *parser.RollbackStatement:
		// Need existing session.
		if req.SessionID == "" {
			writeJSON(w, QueryResponse{Error: "COMMIT/ROLLBACK requires a session_id", Tokens: traceTokens, AST: ast})
			return
		}
		h.mu.Lock()
		exec = h.sessions[req.SessionID]
		h.mu.Unlock()
		if exec == nil {
			writeJSON(w, QueryResponse{Error: fmt.Sprintf("session %q not found", req.SessionID), Tokens: traceTokens, AST: ast})
			return
		}
		result, err := exec.Execute(stmt)
		// Always remove the session after COMMIT/ROLLBACK.
		h.mu.Lock()
		delete(h.sessions, req.SessionID)
		h.mu.Unlock()
		if err != nil {
			writeJSON(w, QueryResponse{Error: err.Error(), Tokens: traceTokens, AST: ast})
			return
		}
		writeJSON(w, QueryResponse{
			Message:   result.Message,
			Tokens:    traceTokens,
			AST:       ast,
			ExecTrace: result.Trace,
		})
		return

	default:
		if req.SessionID != "" {
			// Run on existing session.
			h.mu.Lock()
			exec = h.sessions[req.SessionID]
			h.mu.Unlock()
			if exec == nil {
				writeJSON(w, QueryResponse{Error: fmt.Sprintf("session %q not found", req.SessionID), Tokens: traceTokens, AST: ast})
				return
			}
			sessionID = req.SessionID
		} else {
			// No session: auto-commit mode.
			exec = executor.New(h.db)
		}
	}

	// Execute the statement.
	result, err := exec.Execute(stmt)
	if err != nil {
		writeJSON(w, QueryResponse{
			Error:     err.Error(),
			Tokens:    traceTokens,
			AST:       ast,
			SessionID: sessionID,
		})
		return
	}

	if h.history != nil {
		go h.history.Upsert(req.SQL)
	}
	writeJSON(w, QueryResponse{
		Columns:          result.Columns,
		Rows:             result.Rows,
		Message:          result.Message,
		Tokens:           traceTokens,
		AST:              ast,
		ExecTrace:        result.Trace,
		SessionID:        sessionID,
		StepLog:          result.StepLog,
		NodeTree:         result.NodeTree,
		StepTruncated:    result.StepTruncated,
		IndexSuggestions: result.IndexSuggestions,
	})
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
	case *parser.BeginStatement:
		return map[string]interface{}{"type": "BeginStatement"}
	case *parser.CommitStatement:
		return map[string]interface{}{"type": "CommitStatement"}
	case *parser.RollbackStatement:
		return map[string]interface{}{"type": "RollbackStatement"}
	case *parser.VacuumStatement:
		return map[string]interface{}{"type": "VacuumStatement", "table": s.Table}
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
			Name      string `json:"name"`
			Type      string `json:"type"`
			Primary   bool   `json:"primary"`
			Indexed   bool   `json:"indexed,omitempty"`
			IndexName string `json:"indexName,omitempty"`
		}
		type TblInfo struct {
			Name     string    `json:"name"`
			Columns  []ColInfo `json:"columns"`
			RowCount int       `json:"rowCount"`
		}

		var result []TblInfo
		for _, t := range db.ListTables() {
			idxes := db.ListIndexesForTable(t.Name)
			colIdx := make(map[string]string, len(idxes))
			for _, idx := range idxes {
				colIdx[idx.Column] = idx.Name
			}
			var cols []ColInfo
			for _, c := range t.Columns {
				idxName := colIdx[c.Name]
				cols = append(cols, ColInfo{
					Name:      c.Name,
					Type:      string(c.Type),
					Primary:   c.Primary,
					Indexed:   idxName != "",
					IndexName: idxName,
				})
			}
			result = append(result, TblInfo{Name: t.Name, Columns: cols, RowCount: t.RowCount})
		}
		writeJSON(w, map[string]interface{}{"tables": result})
	}
}

// VacuumHandler handles POST /vacuum {"table": "tablename"}.
func VacuumHandler(db *storage.Database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]interface{}{"error": "only POST allowed"})
			return
		}
		var req struct {
			Table string `json:"table"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Table == "" {
			writeJSON(w, map[string]interface{}{"error": "table is required"})
			return
		}
		reclaimed, err := db.Vacuum(req.Table)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"reclaimed": reclaimed})
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	json.NewEncoder(w).Encode(v)
}
