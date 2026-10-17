package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.db")
	db, err := Open(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func version(t *testing.T, db *sql.DB, module string) int {
	t.Helper()
	var v int
	err := db.QueryRowContext(t.Context(), `SELECT version FROM store_migrations WHERE module = ?`, module).Scan(&v)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestOpenCreatesNestedDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b c", "bot.db")
	db, err := Open(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE x (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file: %v", err)
	}
}

func TestOpenRelativePathWithURIChars(t *testing.T) {
	// '?' and '#' would cut a naively built file: URI and open a different file.
	dir := t.TempDir()
	t.Chdir(dir)
	db, err := Open(t.Context(), "data/we?ird#name%20.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE x (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "we?ird#name%20.db")); err != nil {
		t.Fatalf("db file: %v", err)
	}
}

func TestPragmasOnEveryConnection(t *testing.T) {
	db, _ := openTemp(t)
	ctx := t.Context()
	// Hold several connections at once so the pool must open new ones.
	conns := make([]*sql.Conn, 4)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	for i, c := range conns {
		var mode string
		var fk, busy int
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || fk != 1 || busy != busyTimeoutMS {
			t.Fatalf("conn %d: journal_mode=%s foreign_keys=%d busy_timeout=%d", i, mode, fk, busy)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestMigrateAppliesOnce(t *testing.T) {
	db, _ := openTemp(t)
	steps := []string{
		`CREATE TABLE m_a (id INTEGER PRIMARY KEY)`,
		`INSERT INTO m_a (id) VALUES (1)`,
	}
	for range 3 {
		if err := Migrate(t.Context(), db, "m", steps); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM m_a`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || version(t, db, "m") != 2 {
		t.Fatalf("rows=%d version=%d", n, version(t, db, "m"))
	}
}

func TestMigrateIncremental(t *testing.T) {
	db, _ := openTemp(t)
	steps := []string{`CREATE TABLE m_a (id INTEGER)`}
	if err := Migrate(t.Context(), db, "m", steps); err != nil {
		t.Fatal(err)
	}
	steps = append(steps, `ALTER TABLE m_a ADD COLUMN name TEXT`, `CREATE TABLE m_b (id INTEGER)`)
	if err := Migrate(t.Context(), db, "m", steps); err != nil {
		t.Fatal(err)
	}
	if version(t, db, "m") != 3 || !tableExists(t, db, "m_b") {
		t.Fatalf("version=%d", version(t, db, "m"))
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO m_a (id, name) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateFailingStepRollsBack(t *testing.T) {
	db, _ := openTemp(t)
	steps := []string{
		`CREATE TABLE m_a (id INTEGER)`,
		// Multi-statement step: the first statement must be undone together with the failure.
		`CREATE TABLE m_b (id INTEGER); INSERT INTO no_such_table VALUES (1)`,
	}
	err := Migrate(t.Context(), db, "m", steps)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), `"m"`) || !strings.Contains(err.Error(), "step 2") {
		t.Fatalf("error lacks module or step: %v", err)
	}
	if version(t, db, "m") != 1 {
		t.Fatalf("version=%d, want 1", version(t, db, "m"))
	}
	if !tableExists(t, db, "m_a") || tableExists(t, db, "m_b") {
		t.Fatal("step 2 not rolled back or step 1 lost")
	}

	steps[1] = `CREATE TABLE m_b (id INTEGER)`
	if err := Migrate(t.Context(), db, "m", steps); err != nil {
		t.Fatal(err)
	}
	if version(t, db, "m") != 2 {
		t.Fatalf("version=%d after fix", version(t, db, "m"))
	}
}

func TestMigrateDowngrade(t *testing.T) {
	db, _ := openTemp(t)
	steps := []string{`CREATE TABLE m_a (id INTEGER)`, `CREATE TABLE m_b (id INTEGER)`}
	if err := Migrate(t.Context(), db, "m", steps); err != nil {
		t.Fatal(err)
	}
	err := Migrate(t.Context(), db, "m", steps[:1])
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
	if version(t, db, "m") != 2 {
		t.Fatal("version changed")
	}
}

func TestMigrateRejectsEmptyModule(t *testing.T) {
	db, _ := openTemp(t)
	if err := Migrate(t.Context(), db, "", []string{`SELECT 1`}); err == nil {
		t.Fatal("no error")
	}
}

func TestMigrateModulesIndependent(t *testing.T) {
	db, _ := openTemp(t)
	if err := Migrate(t.Context(), db, "a", []string{`CREATE TABLE a_t (id INTEGER)`}); err != nil {
		t.Fatal(err)
	}
	bSteps := []string{`CREATE TABLE b_t (id INTEGER)`, `CREATE TABLE b_u (id INTEGER)`, `CREATE TABLE b_v (id INTEGER)`}
	if err := Migrate(t.Context(), db, "b", bSteps); err != nil {
		t.Fatal(err)
	}
	if version(t, db, "a") != 1 || version(t, db, "b") != 3 {
		t.Fatalf("a=%d b=%d", version(t, db, "a"), version(t, db, "b"))
	}
	// "a" with one step must not see "b"'s three as a downgrade.
	if err := Migrate(t.Context(), db, "a", []string{`CREATE TABLE a_t (id INTEGER)`}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	_, path := openTemp(t)
	// Separate *sql.DB handles behave like separate processes sharing the file,
	// which is the hardest case for SQLite locking.
	const modules = 6
	var wg sync.WaitGroup
	errs := make(chan error, modules*2)
	for i := range modules * 2 {
		wg.Go(func() {
			db, err := Open(context.Background(), path, nil)
			if err != nil {
				errs <- err
				return
			}
			defer db.Close()
			mod := fmt.Sprintf("mod%d", i%modules) // every module is migrated twice at once
			steps := []string{
				fmt.Sprintf(`CREATE TABLE %s_a (id INTEGER)`, mod),
				fmt.Sprintf(`CREATE TABLE %s_b (id INTEGER)`, mod),
				fmt.Sprintf(`INSERT INTO %s_a VALUES (1)`, mod),
			}
			errs <- Migrate(context.Background(), db, mod, steps)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range modules {
		mod := fmt.Sprintf("mod%d", i)
		var n int
		if err := db.QueryRowContext(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s_a`, mod)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 || version(t, db, mod) != 3 {
			t.Fatalf("%s: rows=%d version=%d", mod, n, version(t, db, mod))
		}
	}
}
