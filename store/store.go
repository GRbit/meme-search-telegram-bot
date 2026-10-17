// Package store opens the bot's SQLite database and applies per-module migrations.
//
// Modules receive a plain *sql.DB and own their tables, which carry the module
// name as a prefix (mediacache_, access_). A host embedding the modules can pass
// its own SQLite handle instead of using Open, as long as Check accepts it;
// modules write only through WriteTx so locking does not depend on the DSN.
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
func migrateStep(ctx context.Context, db *sql.DB, module string, steps []string) (done bool, err error) {
	err = WriteTx(ctx, db, func(tx *sql.Tx) error {
		var v int
		err := tx.QueryRowContext(ctx, `SELECT version FROM store_migrations WHERE module = ?`, module).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read version: %w", err)
		}
		switch {
		case v > len(steps):
			return fmt.Errorf("database has %d steps applied, code knows %d: database is newer than this binary",
				v, len(steps))
		case v == len(steps):
			done = true
			return nil
		}
		if _, err := tx.ExecContext(ctx, steps[v]); err != nil {
			return fmt.Errorf("step %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO store_migrations (module, version) VALUES (?, ?)
			ON CONFLICT (module) DO UPDATE SET version = excluded.version`, module, v+1); err != nil {
			return fmt.Errorf("step %d: bump version: %w", v+1, err)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: migrate %q: %w", module, err)
	}
	return done, nil
}

// WriteTx runs fn in a write transaction and commits it, or rolls back and
// returns fn's error unchanged. Every module write goes through it.
//
// The transaction begins IMMEDIATE whatever the handle's DSN says: with a
// deferred BEGIN, two concurrent read-then-write transactions both hold read
// locks and one fails at upgrade with SQLITE_BUSY without waiting out
// busy_timeout. ncruces maps LevelSerializable to BEGIN IMMEDIATE.
func WriteTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Check verifies that a handle has the pragmas modules rely on: busy_timeout
// (concurrent writers wait instead of failing) and foreign_keys. Module
// constructors call it so a host-opened handle fails at startup, not under load.
//
// It checks one pooled connection; a DSN-level pragma applies to all of them,
// an Exec-level one does not, which is why Open puts them in the DSN.
func Check(ctx context.Context, db *sql.DB) error {
	var busy, fk int
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		return fmt.Errorf("store: check busy_timeout: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return fmt.Errorf("store: check foreign_keys: %w", err)
	}
	var problems []error
	if busy <= 0 {
		problems = append(problems, errors.New("busy_timeout is 0, concurrent writes would fail with SQLITE_BUSY"))
	}
	if fk != 1 {
		problems = append(problems, errors.New("foreign_keys is off"))
	}
	if len(problems) > 0 {
		return fmt.Errorf("store: handle not usable, open it with store.Open: %w", errors.Join(problems...))
	}
	return nil
}
