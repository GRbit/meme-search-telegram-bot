package mediacache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/grbit/meme-search-telegram-bot/mediacache/internal/limiter"
)

// GIF is left out on purpose: Telegram turns an uploaded GIF into an
// animation, which has no photo sizes and cannot be an inline photo result.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}

func indexable(name string) bool {
	return !strings.HasPrefix(name, ".") && imageExts[strings.ToLower(filepath.Ext(name))]
}

type scanStats struct {
	enabled bool
	files   int // indexable files found
	added   int
	changed int
	removed int
	pending int // files without file_id that are not failed
}

// libraryDir returns the absolute library dir, or "" with a reason when the
// index is off.
func (c *Cache) libraryDir() (dir, reason string) {
	if c.cfg.LibraryDir == "" {
		return "", "LibraryDir is empty"
	}
	abs, err := filepath.Abs(c.cfg.LibraryDir)
	if err != nil {
		return "", err.Error()
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err.Error()
	}
	if !info.IsDir() {
		return "", "not a directory"
	}
	return abs, ""
}

// scan syncs the library index with the disk and queues warmup uploads.
//
// A missing directory leaves the index untouched instead of emptying it: an
// unmounted volume would otherwise cost a full rehash once it is back.
func (c *Cache) scan(ctx context.Context) (scanStats, error) {
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	start := time.Now()
	var st scanStats
	dir, reason := c.libraryDir()
	if dir == "" {
		if c.indexOn == nil || *c.indexOn {
			c.log.InfoContext(ctx, "library index disabled", "library_dir", c.cfg.LibraryDir, "reason", reason)
		}
		c.indexOn = new(false)
		c.scans.Add(1)
		return st, nil
	}
	if c.indexOn == nil || !*c.indexOn {
		c.log.InfoContext(ctx, "library index enabled", "library_dir", dir)
	}
	c.indexOn = new(true)
	st.enabled = true

	known, err := c.libraryMeta(ctx)
	if err != nil {
		return st, err
	}
	seen := make(map[string]bool, len(known))
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if p == dir {
				return err
			}
			c.log.WarnContext(ctx, "library scan: skip unreadable entry", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == dir {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !indexable(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Stat, not d.Info: a symlinked file counts as the file it points to.
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			c.log.DebugContext(ctx, "library scan: skip non-regular file", "rel_path", rel, "err", err)
			return nil
		}
		seen[rel] = true
		st.files++
		old, existed := known[rel]
		if existed && old.size == info.Size() && old.mtimeNS == info.ModTime().UnixNano() {
			return nil
		}
		if err := c.indexFile(ctx, p, rel, info); err != nil {
			c.log.WarnContext(ctx, "library scan: index file", "rel_path", rel, "err", err)
			return nil
		}
		if existed {
			st.changed++
			c.log.DebugContext(ctx, "library scan: changed file", "rel_path", rel, "size", info.Size())
		} else {
			st.added++
			c.log.DebugContext(ctx, "library scan: new file", "rel_path", rel, "size", info.Size())
		}
		return nil
	})
	if walkErr != nil {
		return st, fmt.Errorf("mediacache: scan %s: %w", dir, walkErr)
	}
	var gone []string
	for rel := range known {
		if !seen[rel] {
			gone = append(gone, rel)
			c.log.DebugContext(ctx, "library scan: removed file", "rel_path", rel)
		}
	}
	if err := c.deleteLibrary(ctx, gone); err != nil {
		return st, err
	}
	st.removed = len(gone)
	if c.cfg.CacheChatID != 0 {
		if st.pending, err = c.queueWarmup(ctx); err != nil {
			return st, err
		}
	}
	c.scans.Add(1)
	c.log.InfoContext(ctx, "library scan done", "files", st.files, "new", st.added, "changed", st.changed,
		"removed", st.removed, "pending_uploads", st.pending, "duration", time.Since(start))
	return st, nil
}

func (c *Cache) indexFile(ctx context.Context, abs, rel string, info fs.FileInfo) error {
	sum, err := hashFile(abs)
	if err != nil {
		return err
	}
	return c.upsertLibrary(ctx, libRow{
		relPath:  rel,
		baseName: path.Base(rel),
		sha256:   sum,
		size:     info.Size(),
		mtimeNS:  info.ModTime().UnixNano(),
	})
}

func hashFile(p string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, fmt.Errorf("hash %s: %w", p, err)
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// queueWarmup queues every library file still lacking a file_id and returns
// how many there are. Files already queued are deduplicated by the queue.
func (c *Cache) queueWarmup(ctx context.Context) (int, error) {
	rels, err := c.pendingWarmup(ctx)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, rel := range rels {
		ok, err := c.pushWarmup(rel)
		if errors.Is(err, limiter.ErrFull) {
			// The rest is picked up by a later scan once the queue drains.
			c.log.DebugContext(ctx, "warmup queue full, rest waits for the next scan", "queued", queued, "pending", len(rels))
			break
		}
		if err != nil {
			// The queue is closed: Run is ending and nothing will upload these.
			break
		}
		if ok {
			queued++
		}
	}
	c.log.DebugContext(ctx, "warmup queued", "new_jobs", queued, "pending", len(rels))
	return len(rels), nil
}

func (c *Cache) pushWarmup(rel string) (bool, error) {
	return c.queue.Push(limiter.Background, "lib:"+rel, &job{relPath: rel, caption: rel, filename: path.Base(rel)})
}

// AddLibraryFile indexes a file that was just written inside LibraryDir and
// queues its warmup upload, so new saves do not wait for the next rescan.
//
// A path outside LibraryDir (or with the index disabled) is an error, not a
// silent skip: the saver writes under SAVE_DIR, which a host may set apart
// from LibraryDir, and that misconfiguration must show up in its logs. Files
// the index skips anyway (dot files, non-image extensions) are ignored without
// an error. Symlinks are not resolved when checking that path is inside
// LibraryDir.
func (c *Cache) AddLibraryFile(ctx context.Context, p string) error {
	dir, reason := c.libraryDir()
	if dir == "" {
		return fmt.Errorf("mediacache: library index disabled: %s", reason)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("mediacache: add %s: %w", p, err)
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("mediacache: %s is outside the library dir %s", p, dir)
	}
	rel = filepath.ToSlash(rel)
	for _, part := range strings.Split(path.Dir(rel), "/") {
		if strings.HasPrefix(part, ".") && part != "." {
			c.log.DebugContext(ctx, "add library file: hidden dir, not indexed", "rel_path", rel)
			return nil
		}
	}
	if !indexable(path.Base(rel)) {
		c.log.DebugContext(ctx, "add library file: not an indexable image", "rel_path", rel)
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("mediacache: add %s: %w", rel, err)
	}
	if err := c.indexFile(ctx, abs, rel, info); err != nil {
		return fmt.Errorf("mediacache: add %s: %w", rel, err)
	}
	c.log.DebugContext(ctx, "add library file: indexed", "rel_path", rel, "size", info.Size())
	if c.cfg.CacheChatID == 0 {
		return nil
	}
	if _, err := c.pushWarmup(rel); err != nil {
		// Indexed already; the next scan queues it again.
		c.log.DebugContext(ctx, "add library file: upload not queued", "rel_path", rel, "err", err)
	}
	return nil
}
