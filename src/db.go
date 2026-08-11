package SpectralSpy

import (
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// InitSchema applies the common database schema onto the provided database connection.
func InitSchema(db *sql.DB) error {
	if schemaSQL == "" {
		return fmt.Errorf("schemaSQL is empty; ensure schema.sql exists in db/ package directory")
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}
	return nil
}