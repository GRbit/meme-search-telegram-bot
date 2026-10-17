package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

// openForeign opens a handle the way a host might: busy_timeout set, but no
// _txlock=immediate, so plain BeginTx starts DEFERRED.
func openForeign(t *testing.T, path string, pragmas string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestWriteTxSerializesReadModifyWrite runs read-then-write increments from
// many goroutines on a DEFERRED-locking handle. With a deferred BEGIN two
// readers both try to upgrade to a write lock and one fails with SQLITE_BUSY
// without waiting; WriteTx must take the lock at BEGIN so every increment lands.
func TestWriteTxSerializesReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	db := openForeign(t, path, "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE n (v INTEGER NOT NULL); INSERT INTO n VALUES (0)`); err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				errs <- WriteTx(ctx, db, func(tx *sql.Tx) error {
					var v int
					if err := tx.QueryRowContext(ctx, `SELECT v FROM n`).Scan(&v); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `UPDATE n SET v = ?`, v+1)
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WriteTx: %v", err)
		}
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT v FROM n`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != workers*each {
		t.Fatalf("counter = %d, want %d", v, workers*each)
	}
}

func TestWriteTxRollsBackOnError(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE r (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	boom := sql.ErrTxDone
	err := WriteTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO r VALUES (1)`); err != nil {
			return err
		}
		return boom
	})
	if err != boom { //nolint:errorlint // must be returned unwrapped
		t.Fatalf("err = %v, want %v", err, boom)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM r`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0 after rollback", n)
	}
}

func TestCheckAcceptsOpen(t *testing.T) {
	db, _ := openTemp(t)
	if err := Check(context.Background(), db); err != nil {
		t.Fatalf("Check on Open handle: %v", err)
	}
}

// ncruces defaults a bare handle to a 1 minute busy timeout and foreign keys
// on, so the bad handle has to switch them off explicitly.
func TestCheckRejectsHandleWithoutPragmas(t *testing.T) {
	db := openForeign(t, filepath.Join(t.TempDir(), "bare.db"), "?_pragma=busy_timeout(0)&_pragma=foreign_keys(off)")
	if err := Check(context.Background(), db); err == nil {
		t.Fatal("Check accepted a handle without busy_timeout and foreign_keys")
	}
}
