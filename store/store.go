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
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver, no cgo
)

const busyTimeoutMS = 5000

// Open opens (creating if needed, including parent directories) the SQLite file at path.
//
// Pragmas go into the DSN rather than one Exec because database/sql pools
// connections and every new connection must get them. _txlock=immediate makes
// write transactions take the lock at BEGIN, so concurrent writers wait out
// busy_timeout instead of failing with SQLITE_BUSY on lock upgrade; pass
// sql.TxOptions{ReadOnly: true} for read-only transactions.
func Open(ctx context.Context, path string, log *slog.Logger) (*sql.DB, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	db, err := sql.Open("sqlite3", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	log.Info("database opened", "path", abs)
	return db, nil
}

// dsn builds a file: URI; url.URL escapes '?', '#' and '%' in the path, which
// would otherwise be read as URI syntax and open a different file.
func dsn(abs string) string {
	q := url.Values{}
	q.Set("_txlock", "immediate")
	// busy_timeout goes first: journal_mode itself may need to wait for a lock.
	q["_pragma"] = []string{
		"busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")",
		"journal_mode(wal)",
		"foreign_keys(on)",
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	return u.String()
}

// Migrate applies steps[n:] for module, where n is the number of steps already
// applied to this database. Steps are append-only: never edit a released step.
//
// Each step runs in its own transaction together with its version bump, so a
// failed step leaves the earlier ones applied and is retried on the next run.
// The version is re-read inside every transaction, which makes concurrent
// calls safe even for the same module.
func Migrate(ctx context.Context, db *sql.DB, module string, steps []string) error {
	if module == "" {
		return errors.New("store: migrate: empty module name")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS store_migrations (
		module TEXT PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create store_migrations: %w", err)
	}
	for {
		done, err := migrateStep(ctx, db, module, steps)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// migrateStep applies the next pending step, reporting done when none is left.
func migrateStep(ctx context.Context, db *sql.DB, module string, steps []string) (bool, error) {
	// ncruces maps LevelSerializable to BEGIN IMMEDIATE regardless of the DSN, so
	// this holds for a handle a host opened itself.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("store: migrate %q: begin: %w", module, err)
	}
	defer func() { _ = tx.Rollback() }()

	var v int
	err = tx.QueryRowContext(ctx, `SELECT version FROM store_migrations WHERE module = ?`, module).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: migrate %q: read version: %w", module, err)
	}
	switch {
	case v > len(steps):
		return false, fmt.Errorf("store: migrate %q: database has %d steps applied, code knows %d: database is newer than this binary",
			module, v, len(steps))
	case v == len(steps):
		return true, nil
	}
	if _, err := tx.ExecContext(ctx, steps[v]); err != nil {
		return false, fmt.Errorf("store: migrate %q: step %d: %w", module, v+1, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO store_migrations (module, version) VALUES (?, ?)
		ON CONFLICT (module) DO UPDATE SET version = excluded.version`, module, v+1); err != nil {
		return false, fmt.Errorf("store: migrate %q: bump version: %w", module, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: migrate %q: step %d: commit: %w", module, v+1, err)
	}
	return false, nil
}
