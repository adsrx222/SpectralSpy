package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	"github.com/adsrx222/SpectralSpy/src"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run main.go <sqlite_db_path>")
	}
	dbPath := os.Args[1]

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	
	if err := SpectralSpy.RunDP(ctx, db); err != nil {
		log.Fatalf("Data processing failed: %v", err)
	}

	log.Println("Successfully completed audio processing.")
}