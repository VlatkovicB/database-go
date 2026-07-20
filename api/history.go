package api

import (
	"database/sql"
	"sync"

	_ "modernc.org/sqlite"
)

type HistoryStore struct {
	mu sync.Mutex
	db *sql.DB
}

func NewHistoryStore(path string) (*HistoryStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS query_history (
		sql       TEXT PRIMARY KEY,
		last_used DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return nil, err
	}
	return &HistoryStore{db: db}, nil
}

func (h *HistoryStore) Upsert(query string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.db.Exec(
		`INSERT INTO query_history(sql, last_used) VALUES(?, CURRENT_TIMESTAMP)
		 ON CONFLICT(sql) DO UPDATE SET last_used = CURRENT_TIMESTAMP`,
		query,
	)
}

func (h *HistoryStore) List() ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, err := h.db.Query(`SELECT sql FROM query_history ORDER BY last_used DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}
