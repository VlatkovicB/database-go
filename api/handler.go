package api

import (
	"database/internal/executor"
	"database/internal/lexer"
	"database/internal/parser"
	"database/internal/storage"
	"encoding/json"
	"net/http"
)

type QueryRequest struct {
	SQL string `json:"sql"`
}

type QueryResponse struct {
	Columns []string        `json:"columns,omitempty"`
	Rows    [][]interface{} `json:"rows,omitempty"`
	Message string          `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
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

		tokens := lexer.New(req.SQL).Tokenize()
		stmt, err := parser.New(tokens).Parse()
		if err != nil {
			writeJSON(w, QueryResponse{Error: "parse error: " + err.Error()})
			return
		}

		result, err := exec.Execute(stmt)
		if err != nil {
			writeJSON(w, QueryResponse{Error: err.Error()})
			return
		}

		writeJSON(w, QueryResponse{
			Columns: result.Columns,
			Rows:    result.Rows,
			Message: result.Message,
		})
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	json.NewEncoder(w).Encode(v)
}
