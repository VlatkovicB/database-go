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

	mux := http.NewServeMux()
	mux.HandleFunc("/query", api.QueryHandler(db))
	mux.HandleFunc("/tables", api.TablesHandler(db))

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
