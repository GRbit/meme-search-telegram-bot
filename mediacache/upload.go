package mediacache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/mediacache/internal/limiter"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// memeRetryAfter is how long a meme whose API bytes Telegram rejected is not
// tried again. Inline mode enqueues misses on every keystroke, so without it
// one bad meme would eat the realtime upload rate.
const memeRetryAfter = 24 * time.Hour

// job is one upload. Jobs carry no bytes while queued: warmup and library
// jobs read the file and API jobs download the meme only when the worker runs
// them, so API downloads are bounded by the upload rate (they share the
// per-token budget with user searches) and a long queue holds no images.
type job struct {
	memeID   int64           // 0 for warmup
	meme     memesearch.Meme // realtime: downloaded from the API unless relPath is readable
	relPath  string          // library file the bytes come from or belong to, if any
	caption  string
	filename string
	data     []byte      // set once downloaded, kept for a retry after 429
	done     chan result // nil for warmup
}

type result struct {
	fileID, uniqueID string
	err              error
}

func (j *job) key() string {
	if j.done == nil {
		return "lib:" + j.relPath
	}
	return ""
}

func (j *job) finish(r result) {
	if j.done != nil {
		j.done <- r // buffered, read at most once
	}
}

// call is one in-flight resolve shared by every Resolve and Enqueue of a meme.
type call struct {
	done     chan struct{}
	fileID   string
	err      error
	enqueued bool // counts against maxEnqueued
}

// join returns the in-flight resolve of m, starting it if needed. With bounded
// set it refuses to start a new one past maxEnqueued and returns nil.
func (c *Cache) join(m memesearch.Meme, bounded bool) *call {
	c.mu.Lock()
	if cl, ok := c.calls[m.ID]; ok {
		c.mu.Unlock()
		return cl
	}
	if bounded && c.enqueued >= c.maxEnqueued {
		c.mu.Unlock()
		return nil
	}
	cl := &call{done: make(chan struct{}), enqueued: bounded}
	c.calls[m.ID] = cl
	if bounded {
		c.enqueued++
	}
	c.mu.Unlock()

	go func() {
		// The cache's own context, not a caller's: the first caller giving up
		// must not fail everyone else sharing the upload.
		fileID, err := c.resolveMeme(c.life, m)
		c.mu.Lock()
		delete(c.calls, m.ID)
		if cl.enqueued {
			c.enqueued--
		}
		c.mu.Unlock()
		cl.fileID, cl.err = fileID, err
		close(cl.done)
	}()
	return cl
}

// resolveMeme is the slow path of Resolve. It only consults the database;
// reading or downloading the bytes waits for the job's turn in the queue.
func (c *Cache) resolveMeme(ctx context.Context, m memesearch.Meme) (string, error) {
	if err := notPhoto(m); err != nil {
		return "", err
	}
	mr, ok, err := c.memeRow(ctx, m.ID)
	if err != nil {
		return "", err
	}
	if ok && mr.fileID != "" {
		return mr.fileID, nil
	}
	if ok && !mr.failedAt.IsZero() && c.now().Sub(mr.failedAt) < memeRetryAfter {
		c.log.DebugContext(ctx, "resolve: meme rejected recently, not retrying", "meme_id", m.ID, "failed_at", mr.failedAt)
		return "", fmt.Errorf("%w: meme %d", ErrRejected, m.ID)
	}
	rows, err := c.libraryByName(ctx, m.Filename)
	if err != nil {
		return "", err
	}
	j := &job{memeID: m.ID, meme: m, caption: fmt.Sprintf("meme %d", m.ID), filename: photoName(m)}
	if len(rows) == 1 {
		r := rows[0]
		if fid, done, err := c.useRow(ctx, m.ID, r); done {
			return fid, err
		}
		j.relPath, j.caption, j.filename = r.relPath, r.relPath, path.Base(r.relPath)
	}
	c.log.DebugContext(ctx, "resolve: queueing upload", "meme_id", m.ID, "rel_path", j.relPath,
		"library_candidates", len(rows))
	return c.upload(ctx, j)
}

// matchRow picks the library row whose content is data.
func matchRow(rows []libRow, data []byte) (libRow, bool) {
	sum := sha256.Sum256(data)
	for _, r := range rows {
		if r.sha256 == sum {
			return r, true
		}
	}
	return libRow{}, false
}

// useRow settles m from a library row that needs no upload: it has a file_id
// (remembered by meme id) or it failed before. done is false when the row
// still has to be uploaded.
func (c *Cache) useRow(ctx context.Context, memeID int64, r libRow) (fileID string, done bool, err error) {
	switch {
	case r.fileID != "":
		if err := c.rememberMeme(ctx, memeID, r.fileID, r.fileUniqueID, r.relPath); err != nil {
			c.log.WarnContext(ctx, "resolve: remember meme", "meme_id", memeID, "err", err)
		}
		return r.fileID, true, nil
	case r.failed:
		return "", true, fmt.Errorf("%w: %s: %s", ErrRejected, r.relPath, r.failReason)
	}
	return "", false, nil
}

// upload queues a realtime job and waits for the worker.
func (c *Cache) upload(ctx context.Context, j *job) (string, error) {
	j.done = make(chan result, 1)
	if _, err := c.queue.Push(limiter.Realtime, "", j); err != nil {
		if errors.Is(err, limiter.ErrFull) {
			return "", ErrQueueFull
		}
		return "", ErrStopped
	}
	select {
	case r := <-j.done:
		return r.fileID, r.err
	case <-ctx.Done():
		return "", ErrStopped
	}
}

func (c *Cache) absPath(rel string) string {
	return filepath.Join(c.cfg.LibraryDir, filepath.FromSlash(rel))
}

// work serves the upload queue until ctx ends or the queue closes.
func (c *Cache) work(ctx context.Context) {
	for {
		j, p, err := c.queue.Next(ctx)
		if err != nil {
			return
		}
		c.runJob(ctx, j, p)
	}
}

func (c *Cache) runJob(ctx context.Context, j *job, p limiter.Priority) {
	data, ok := c.jobData(ctx, j)
	if !ok {
		return
	}

	start := time.Now()
	msg, err := c.sender.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:              c.cfg.CacheChatID,
		Photo:               &models.InputFileUpload{Filename: j.filename, Data: bytes.NewReader(data)},
		Caption:             j.caption,
		DisableNotification: true,
	})
	var ps models.PhotoSize
	if err == nil {
		var ok bool
		if ps, ok = largest(msg); !ok {
			err = errNoPhoto
		}
	}
	log := c.log.With("method", "sendPhoto", "priority", p.String(), "caption", j.caption, "bytes", len(data),
		"duration", time.Since(start))
	if err == nil {
		log.DebugContext(ctx, "upload done", "file_unique_id", ps.FileUniqueID)
		c.stored(ctx, j, ps)
		j.finish(result{fileID: ps.FileID, uniqueID: ps.FileUniqueID})
		return
	}

	var tooMany *bot.TooManyRequestsError
	switch kind := classify(err); {
	case errors.As(err, &tooMany):
		wait := time.Duration(max(tooMany.RetryAfter, 1)) * time.Second
		log.WarnContext(ctx, "upload: telegram rate limit, pausing the queue", "retry_after", wait)
		c.queue.Pause(wait)
		if err := c.queue.PushFront(p, j.key(), j); err != nil {
			j.finish(result{err: ErrStopped})
		}
	case kind == failFile:
		reason := err.Error()
		log.WarnContext(ctx, "upload: telegram rejected the file, not retrying until it changes", "err", err)
		var recErr error
		if j.relPath != "" {
			recErr = c.markLibraryFailed(ctx, j.relPath, reason)
		} else if j.memeID != 0 {
			recErr = c.markMemeFailed(ctx, j.memeID, reason)
		}
		if recErr != nil {
			c.log.WarnContext(ctx, "upload: record failure", "err", recErr)
		}
		j.finish(result{err: fmt.Errorf("%w: %w", ErrRejected, err)})
	case kind == failChat:
		dropped := c.queue.DropBackground()
		log.ErrorContext(ctx, "upload: cache chat refused the upload, check CacheChatID and the bot's admin rights",
			"err", err, "dropped_warmup_jobs", dropped)
		j.finish(result{err: fmt.Errorf("mediacache: upload: %w", err)})
	default:
		log.WarnContext(ctx, "upload failed", "err", err)
		j.finish(result{err: fmt.Errorf("mediacache: upload: %w", err)})
	}
}

// jobData reads or downloads the bytes of j. ok is false when the job is
// settled without an upload; j is finished then.
func (c *Cache) jobData(ctx context.Context, j *job) (data []byte, ok bool) {
	if j.data != nil {
		return j.data, true
	}
	if j.relPath != "" {
		if c.settledRow(ctx, j) {
			return nil, false
		}
		data, err := os.ReadFile(c.absPath(j.relPath))
		if err == nil {
			return data, true
		}
		if j.done == nil {
			c.log.DebugContext(ctx, "upload: cannot read library file, skipped", "rel_path", j.relPath, "err", err)
			return nil, false
		}
		// The next scan drops the row; the API still has the meme.
		c.log.DebugContext(ctx, "upload: library file unreadable, using the API", "meme_id", j.memeID,
			"rel_path", j.relPath, "err", err)
		j.relPath, j.caption, j.filename = "", fmt.Sprintf("meme %d", j.memeID), photoName(j.meme)
	}
	ct, err := fetchPhoto(ctx, c.api, c.log, j.meme)
	if err != nil {
		j.finish(result{err: err})
		return nil, false
	}
	j.data = ct.Data
	rows, err := c.libraryByName(ctx, j.meme.Filename)
	if err != nil {
		c.log.WarnContext(ctx, "upload: library lookup", "meme_id", j.memeID, "err", err)
	}
	if len(rows) > 1 {
		r, found := matchRow(rows, ct.Data)
		if !found {
			c.log.DebugContext(ctx, "upload: no library file matches the hash", "meme_id", j.memeID, "candidates", len(rows))
			return j.data, true
		}
		c.log.DebugContext(ctx, "upload: name collision settled by hash", "meme_id", j.memeID,
			"rel_path", r.relPath, "candidates", len(rows))
		if fid, done, err := c.useRow(ctx, j.memeID, r); done {
			j.finish(result{fileID: fid, uniqueID: r.fileUniqueID, err: err})
			return nil, false
		}
		j.relPath, j.caption, j.filename = r.relPath, r.relPath, path.Base(r.relPath)
	}
	return j.data, true
}

// settledRow reports whether j's library file needs no upload any more:
// another job uploaded it since this one was queued, it failed, or (for
// warmup) it is gone. A realtime job is finished from the row.
func (c *Cache) settledRow(ctx context.Context, j *job) bool {
	r, ok, err := c.libraryRow(ctx, j.relPath)
	switch {
	case err != nil:
		c.log.WarnContext(ctx, "upload: read library row", "rel_path", j.relPath, "err", err)
	case !ok && j.done == nil:
		c.log.DebugContext(ctx, "upload: library file gone, skipped", "rel_path", j.relPath)
		return true
	case ok && (r.fileID != "" || r.failed):
		c.log.DebugContext(ctx, "upload: already settled, skipped", "rel_path", j.relPath, "failed", r.failed)
		if j.memeID != 0 {
			fid, _, err := c.useRow(ctx, j.memeID, r)
			j.finish(result{fileID: fid, uniqueID: r.fileUniqueID, err: err})
		}
		return true
	}
	return false
}

// stored records a successful upload on the library row and the meme.
func (c *Cache) stored(ctx context.Context, j *job, ps models.PhotoSize) {
	if j.relPath != "" {
		if err := c.setLibraryFileID(ctx, j.relPath, ps.FileID, ps.FileUniqueID); err != nil {
			c.log.WarnContext(ctx, "upload: store library file_id", "rel_path", j.relPath, "err", err)
		}
	}
	if j.memeID != 0 {
		if err := c.rememberMeme(ctx, j.memeID, ps.FileID, ps.FileUniqueID, j.relPath); err != nil {
			c.log.WarnContext(ctx, "upload: remember meme", "meme_id", j.memeID, "err", err)
		}
	}
}

var errNoPhoto = errors.New("telegram returned no photo sizes")

func largest(msg *models.Message) (models.PhotoSize, bool) {
	if msg == nil || len(msg.Photo) == 0 {
		return models.PhotoSize{}, false
	}
	best := msg.Photo[0]
	for _, p := range msg.Photo[1:] {
		if p.Width*p.Height > best.Width*best.Height {
			best = p
		}
	}
	return best, best.FileID != ""
}

type failKind int

const (
	failTransient failKind = iota
	failFile               // this file will never be accepted
	failChat               // the cache chat is misconfigured; not the file's fault
)

// classify sorts a SendPhoto error. go-telegram/bot exposes only the status
// sentinel and the description text, so chat problems are told apart from bad
// files by the wording Telegram uses for them.
func classify(err error) failKind {
	if errors.Is(err, errNoPhoto) {
		return failFile
	}
	if errors.Is(err, bot.ErrorForbidden) {
		return failChat
	}
	msg := strings.ToLower(err.Error())
	if errors.Is(err, bot.ErrorBadRequest) {
		for _, s := range []string{"chat not found", "chat_write_forbidden", "not enough rights", "need administrator", "have no rights"} {
			if strings.Contains(msg, s) {
				return failChat
			}
		}
		return failFile
	}
	// 413 is not mapped to a sentinel by the library; it arrives as text.
	if strings.Contains(msg, " 413 ") || strings.Contains(msg, "too large") {
		return failFile
	}
	return failTransient
}
