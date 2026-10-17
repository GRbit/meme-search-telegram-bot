// Package store opens the bot's SQLite database and applies per-module migrations.
//
// Modules receive a plain *sql.DB and own their tables, which carry the module
// name as a prefix (mediacache_, access_). A host embedding the modules can pass
// its own SQLite handle instead of using Open.
package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver, no cgo
)

// Open opens (creating if needed, including parent directories) the SQLite file at path.
func Open(ctx context.Context, path string, log *slog.Logger) (*sql.DB, error) {
	return nil, errors.New("store: not implemented")
}

// Migrate applies steps[n:] for module, where n is the number of steps already
// applied to this database. Steps are append-only: never edit a released step.
func Migrate(ctx context.Context, db *sql.DB, module string, steps []string) error {
	return errors.New("store: not implemented")
}
