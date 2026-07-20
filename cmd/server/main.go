package main

import (
	"database/api"
	"database/internal/storage"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
)

//go:embed web
var webFiles embed.FS

func main() {
	db := storage.New()

	historyPath := "minidb.history.db"
	history, err := api.NewHistoryStore(historyPath)
	if err != nil {
		log.Printf("history store unavailable: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/query", api.QueryHandler(db, history))
	mux.HandleFunc("/history", api.HistoryHandler(history))
	mux.HandleFunc("/tables", api.TablesHandler(db))
	mux.HandleFunc("/seed", api.SeedHandler(db))
	mux.HandleFunc("/vacuum", api.VacuumHandler(db))
	mux.HandleFunc("/wal", api.WALHandler(db))
	mux.HandleFunc("/wal/checkpoint", api.WALCheckpointHandler(db))
	mux.HandleFunc("/wal/crash", api.WALCrashHandler(db))
	mux.HandleFunc("/wal/recover", api.WALRecoverHandler(db))

	// Serve frontend from embedded web/ directory.
	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(webFS)))

	addr := ":8080"
	fmt.Printf("MiniDB running at http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
