package mediacache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// migrations are append-only: never edit a released step.
var migrations = []string{
	`CREATE TABLE mediacache_library (
		rel_path TEXT PRIMARY KEY,
		base_name TEXT NOT NULL,
		sha256 BLOB NOT NULL,
		size INTEGER NOT NULL,
		mtime_ns INTEGER NOT NULL,
		file_id TEXT NOT NULL DEFAULT '',
		file_unique_id TEXT NOT NULL DEFAULT '',
		failed_at INTEGER,
		fail_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX mediacache_library_base_name ON mediacache_library (base_name);
	CREATE INDEX mediacache_library_sha256 ON mediacache_library (sha256);
	CREATE INDEX mediacache_library_file_unique_id ON mediacache_library (file_unique_id);
	CREATE TABLE mediacache_memes (
		meme_id INTEGER PRIMARY KEY,
		file_id TEXT NOT NULL DEFAULT '',
		file_unique_id TEXT NOT NULL DEFAULT '',
		library_rel_path TEXT,
		failed_at INTEGER,
		fail_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX mediacache_memes_file_unique_id ON mediacache_memes (file_unique_id);`,
	// The change feed: its cursor, and the photo memes it reported, which
	// warmup uploads from the API when there is no library dir to scan.
	`CREATE TABLE mediacache_feed (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		cursor TEXT NOT NULL
	);
	CREATE TABLE mediacache_api_memes (
		meme_id INTEGER PRIMARY KEY,
		filename TEXT NOT NULL,
		media_type TEXT NOT NULL
	);`,
}

// libRow is one row of the library index. relPath uses forward slashes.
type libRow struct {
	relPath      string
	baseName     string
	sha256       [32]byte
	size         int64
	mtimeNS      int64
	fileID       string
	fileUniqueID string
	failed       bool
	failReason   string
}

const libCols = `rel_path, base_name, sha256, size, mtime_ns, file_id, file_unique_id, failed_at IS NOT NULL, fail_reason`

func scanLibRow(sc interface{ Scan(...any) error }) (libRow, error) {
	var r libRow
	var sum []byte
	if err := sc.Scan(&r.relPath, &r.baseName, &sum, &r.size, &r.mtimeNS, &r.fileID, &r.fileUniqueID,
		&r.failed, &r.failReason); err != nil {
		return r, err
	}
	copy(r.sha256[:], sum)
	return r, nil
}

func (c *Cache) libraryRow(ctx context.Context, rel string) (libRow, bool, error) {
	r, err := scanLibRow(c.db.QueryRowContext(ctx, `SELECT `+libCols+` FROM mediacache_library WHERE rel_path = ?`, rel))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("mediacache: read library row: %w", err)
	}
	return r, true, nil
}

func (c *Cache) libraryByName(ctx context.Context, base string) ([]libRow, error) {
	if base == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT `+libCols+` FROM mediacache_library WHERE base_name = ? ORDER BY rel_path`, base)
	if err != nil {
		return nil, fmt.Errorf("mediacache: library by name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []libRow
	for rows.Next() {
		r, err := scanLibRow(rows)
		if err != nil {
			return nil, fmt.Errorf("mediacache: library by name: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fileMeta is what a rescan compares to decide whether to hash a file again.
type fileMeta struct {
	size, mtimeNS int64
}

func (c *Cache) libraryMeta(ctx context.Context) (map[string]fileMeta, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT rel_path, size, mtime_ns FROM mediacache_library`)
	if err != nil {
		return nil, fmt.Errorf("mediacache: read library index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]fileMeta{}
	for rows.Next() {
		var rel string
		var m fileMeta
		if err := rows.Scan(&rel, &m.size, &m.mtimeNS); err != nil {
			return nil, fmt.Errorf("mediacache: read library index: %w", err)
		}
		out[rel] = m
	}
	return out, rows.Err()
}

// upsertLibrary stores a new or changed file. A changed file keeps its
// file_id and failure only if its content hash is unchanged: a touched file is
// the same upload, an edited one must be uploaded (or rejected) anew.
//
// A row without a file_id of its own takes one from any row with the same
// content: file_ids depend on content, not path, so a renamed or moved folder
// (whose old rows are deleted only after the scan's walk) costs a rehash, not
// a re-upload. Failures are not inherited: one more attempt is cheap.
//
// hasFileID reports whether the stored row has a file_id afterwards.
func (c *Cache) upsertLibrary(ctx context.Context, r libRow) (hasFileID bool, err error) {
	err = store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		// In an SQLite UPDATE every SET expression sees the old row, so sha256
		// on the right is the previous hash. Both subqueries pick the same
		// row, so file_id and file_unique_id stay a pair.
		err := tx.QueryRowContext(ctx, `INSERT INTO mediacache_library
				(rel_path, base_name, sha256, size, mtime_ns, file_id, file_unique_id)
			VALUES (?1, ?2, ?3, ?4, ?5,
				COALESCE((SELECT file_id FROM mediacache_library
					WHERE sha256 = ?3 AND file_id != '' ORDER BY rel_path LIMIT 1), ''),
				COALESCE((SELECT file_unique_id FROM mediacache_library
					WHERE sha256 = ?3 AND file_id != '' ORDER BY rel_path LIMIT 1), ''))
			ON CONFLICT (rel_path) DO UPDATE SET
				base_name = excluded.base_name,
				sha256 = excluded.sha256,
				size = excluded.size,
				mtime_ns = excluded.mtime_ns,
				file_id = CASE WHEN sha256 = excluded.sha256 AND file_id != '' THEN file_id ELSE excluded.file_id END,
				file_unique_id = CASE WHEN sha256 = excluded.sha256 AND file_id != ''
					THEN file_unique_id ELSE excluded.file_unique_id END,
				failed_at = CASE WHEN sha256 = excluded.sha256 AND excluded.file_id = '' THEN failed_at ELSE NULL END,
				fail_reason = CASE WHEN sha256 = excluded.sha256 AND excluded.file_id = '' THEN fail_reason ELSE '' END
			RETURNING file_id != ''`,
			r.relPath, r.baseName, r.sha256[:], r.size, r.mtimeNS).Scan(&hasFileID)
		if err != nil {
			return fmt.Errorf("mediacache: upsert library row %s: %w", r.relPath, err)
		}
		return nil
	})
	return hasFileID, err
}

func (c *Cache) deleteLibrary(ctx context.Context, rels []string) error {
	if len(rels) == 0 {
		return nil
	}
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		for _, rel := range rels {
			if _, err := tx.ExecContext(ctx, `DELETE FROM mediacache_library WHERE rel_path = ?`, rel); err != nil {
				return fmt.Errorf("mediacache: delete library row %s: %w", rel, err)
			}
		}
		return nil
	})
}

func (c *Cache) setLibraryFileID(ctx context.Context, rel, fileID, uniqueID string) error {
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE mediacache_library
			SET file_id = ?, file_unique_id = ?, failed_at = NULL, fail_reason = '' WHERE rel_path = ?`,
			fileID, uniqueID, rel)
		if err != nil {
			return fmt.Errorf("mediacache: store file_id for %s: %w", rel, err)
		}
		return nil
	})
}

func (c *Cache) markLibraryFailed(ctx context.Context, rel, reason string) error {
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE mediacache_library SET failed_at = ?, fail_reason = ? WHERE rel_path = ?`,
			c.now().Unix(), reason, rel)
		if err != nil {
			return fmt.Errorf("mediacache: record failure for %s: %w", rel, err)
		}
		return nil
	})
}

// pendingWarmup lists library files that still need an upload, in no
// particular order: queueWarmup sorts them by name date.
func (c *Cache) pendingWarmup(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT rel_path FROM mediacache_library
		WHERE file_id = '' AND failed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("mediacache: list pending uploads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, fmt.Errorf("mediacache: list pending uploads: %w", err)
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

type memeRow struct {
	fileID   string
	failedAt time.Time // zero when not failed
}

func (c *Cache) memeRow(ctx context.Context, id int64) (memeRow, bool, error) {
	var r memeRow
	var failed sql.NullInt64
	err := c.db.QueryRowContext(ctx, `SELECT file_id, failed_at FROM mediacache_memes WHERE meme_id = ?`, id).
		Scan(&r.fileID, &failed)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("mediacache: read meme %d: %w", id, err)
	}
	if failed.Valid {
		r.failedAt = time.Unix(failed.Int64, 0)
	}
	return r, true, nil
}

// rememberMeme upserts a meme's file_id; an empty rel keeps the stored link
// to a library file, if any.
func (c *Cache) rememberMeme(ctx context.Context, id int64, fileID, uniqueID, rel string) error {
	var relArg any
	if rel != "" {
		relArg = rel
	}
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO mediacache_memes (meme_id, file_id, file_unique_id, library_rel_path)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (meme_id) DO UPDATE SET
				file_id = excluded.file_id,
				file_unique_id = excluded.file_unique_id,
				library_rel_path = COALESCE(excluded.library_rel_path, library_rel_path),
				failed_at = NULL,
				fail_reason = ''`, id, fileID, uniqueID, relArg)
		if err != nil {
			return fmt.Errorf("mediacache: remember meme %d: %w", id, err)
		}
		return nil
	})
}

func (c *Cache) markMemeFailed(ctx context.Context, id int64, reason string) error {
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO mediacache_memes (meme_id, failed_at, fail_reason) VALUES (?, ?, ?)
			ON CONFLICT (meme_id) DO UPDATE SET failed_at = excluded.failed_at, fail_reason = excluded.fail_reason`,
			id, c.now().Unix(), reason)
		if err != nil {
			return fmt.Errorf("mediacache: record failure for meme %d: %w", id, err)
		}
		return nil
	})
}

func (c *Cache) knownUniqueID(ctx context.Context, id string) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM mediacache_memes WHERE file_unique_id = ?1) OR
		EXISTS (SELECT 1 FROM mediacache_library WHERE file_unique_id = ?1)`, id).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("mediacache: known file_unique_id: %w", err)
	}
	return n == 1, nil
}

func (c *Cache) knownSHA256(ctx context.Context, sum [32]byte) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mediacache_library WHERE sha256 = ?)`, sum[:]).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("mediacache: known sha256: %w", err)
	}
	return n == 1, nil
}

func (c *Cache) feedCursor(ctx context.Context) (string, error) {
	var cur string
	err := c.db.QueryRowContext(ctx, `SELECT cursor FROM mediacache_feed WHERE id = 1`).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("mediacache: read feed cursor: %w", err)
	}
	return cur, nil
}

func (c *Cache) setFeedCursor(ctx context.Context, cur string) error {
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO mediacache_feed (id, cursor) VALUES (1, ?)
			ON CONFLICT (id) DO UPDATE SET cursor = excluded.cursor`, cur)
		if err != nil {
			return fmt.Errorf("mediacache: store feed cursor: %w", err)
		}
		return nil
	})
}

func (c *Cache) upsertAPIMemes(ctx context.Context, ms []memesearch.Meme) error {
	if len(ms) == 0 {
		return nil
	}
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		for _, m := range ms {
			_, err := tx.ExecContext(ctx, `INSERT INTO mediacache_api_memes (meme_id, filename, media_type) VALUES (?, ?, ?)
				ON CONFLICT (meme_id) DO UPDATE SET filename = excluded.filename, media_type = excluded.media_type`,
				m.ID, m.Filename, m.MediaType)
			if err != nil {
				return fmt.Errorf("mediacache: store meme %d: %w", m.ID, err)
			}
		}
		return nil
	})
}

func (c *Cache) deleteAPIMeme(ctx context.Context, id int64) error {
	return store.WriteTx(ctx, c.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM mediacache_api_memes WHERE meme_id = ?`, id); err != nil {
			return fmt.Errorf("mediacache: forget meme %d: %w", id, err)
		}
		return nil
	})
}

// pendingAPIWarmup lists feed memes without a file_id that did not fail.
func (c *Cache) pendingAPIWarmup(ctx context.Context) ([]memesearch.Meme, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT a.meme_id, a.filename, a.media_type
		FROM mediacache_api_memes a LEFT JOIN mediacache_memes m ON m.meme_id = a.meme_id
		WHERE m.meme_id IS NULL OR (m.file_id = '' AND m.failed_at IS NULL)`)
	if err != nil {
		return nil, fmt.Errorf("mediacache: list pending API uploads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []memesearch.Meme
	for rows.Next() {
		var m memesearch.Meme
		if err := rows.Scan(&m.ID, &m.Filename, &m.MediaType); err != nil {
			return nil, fmt.Errorf("mediacache: list pending API uploads: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
