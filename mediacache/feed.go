package mediacache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// MemeLister is the slice of the API the change feed needs;
// *memesearch.HTTPClient implements it.
type MemeLister interface {
	ListMemes(ctx context.Context, opts memesearch.ListOptions) (memesearch.MemePage, error)
}

// DefaultFeedInterval applies when Config.FeedInterval is zero.
const DefaultFeedInterval = time.Minute

// defaultFeedPages bounds one poll: the listing has its own budget of 60
// requests per minute, and a first full listing of a large library should
// spread over several polls instead of hitting 429.
const defaultFeedPages = 20

func (c *Cache) feedEnabled() bool { return c.lister != nil && c.caps != nil }

// feedLoop polls the feed every FeedInterval. It does not poll at startup:
// the capability probe has not answered yet, and a poll before the first
// library scan would request a needless rescan.
func (c *Cache) feedLoop(ctx context.Context) {
	for {
		tick, stop := c.clock.After(c.cfg.FeedInterval)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-tick:
		}
		c.pollFeed(ctx)
	}
}

// pollFeed reads the feed from the stored cursor, at most feedPages pages.
// The cursor is stored after every page, so a restart or an error resumes
// where it stopped; an empty cursor lists the whole collection once.
func (c *Cache) pollFeed(ctx context.Context) {
	if !c.feedEnabled() {
		return
	}
	c.feedMu.Lock()
	defer c.feedMu.Unlock()
	if !c.caps.Capabilities().Listing {
		c.log.DebugContext(ctx, "feed: server has no meme listing, poll skipped")
		return
	}
	cursor, err := c.feedCursor(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "feed: read cursor", "err", err)
		return
	}
	for range c.feedPages {
		page, err := c.lister.ListMemes(ctx, memesearch.ListOptions{Cursor: cursor, Limit: c.feedLimit})
		if err != nil {
			c.feedErr(ctx, err)
			return
		}
		c.log.DebugContext(ctx, "feed page", "count", len(page.Memes), "has_more", page.HasMore, "from_start", cursor == "")
		if len(page.Memes) == 0 {
			return
		}
		c.handleFeed(ctx, page.Memes)
		if page.NextCursor != "" {
			cursor = page.NextCursor
			if err := c.setFeedCursor(ctx, cursor); err != nil {
				c.log.WarnContext(ctx, "feed: store cursor", "err", err)
				return
			}
		}
		if !page.HasMore {
			return
		}
	}
	c.log.DebugContext(ctx, "feed: page budget used, continuing next poll", "pages", c.feedPages)
}

func (c *Cache) feedErr(ctx context.Context, err error) {
	var apiErr *memesearch.APIError
	switch {
	case errors.Is(err, memesearch.ErrUnsupported):
		// The probe flips the capability off; until then every poll asks once.
		c.log.DebugContext(ctx, "feed: server has no meme listing", "err", err)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && apiErr.Code == "invalid_cursor":
		// The cursor is opaque and its format may change with a server upgrade.
		c.log.WarnContext(ctx, "feed: server rejected the stored cursor, listing from the start next poll")
		if err := c.setFeedCursor(ctx, ""); err != nil {
			c.log.WarnContext(ctx, "feed: reset cursor", "err", err)
		}
	case errors.Is(err, memesearch.ErrRateLimited):
		c.log.DebugContext(ctx, "feed: rate limited, continuing next poll", "err", err)
	default:
		c.log.WarnContext(ctx, "feed: list memes failed", "err", err)
	}
}

// handleFeed settles warmup for a feed page: with a library
// a new file is indexed by an early rescan (the scan reads the file, not the
// API); without one the memes are uploaded from the API.
func (c *Cache) handleFeed(ctx context.Context, memes []memesearch.Meme) {
	if dir, _ := c.libraryDir(); dir != "" {
		c.rescanForUnknown(ctx, memes)
		return
	}
	if c.cfg.CacheChatID == 0 {
		return
	}
	c.warmFromFeed(ctx, memes)
}

func (c *Cache) rescanForUnknown(ctx context.Context, memes []memesearch.Meme) {
	unknown := 0
	for _, m := range memes {
		if notPhoto(m) != nil || !indexable(m.Filename) {
			continue
		}
		known, err := c.indexed(ctx, m)
		if err != nil {
			c.log.WarnContext(ctx, "feed: library lookup", "meme_id", m.ID, "err", err)
			continue
		}
		if !known {
			unknown++
		}
	}
	if unknown == 0 {
		return
	}
	c.log.DebugContext(ctx, "feed: memes not in the library index, requesting a rescan", "count", unknown)
	select {
	case c.rescan <- struct{}{}:
	default: // one is pending already
	}
}

func (c *Cache) warmFromFeed(ctx context.Context, memes []memesearch.Meme) {
	var photos []memesearch.Meme
	for _, m := range memes {
		if notPhoto(m) == nil {
			photos = append(photos, m)
		}
	}
	if err := c.upsertAPIMemes(ctx, photos); err != nil {
		c.log.WarnContext(ctx, "feed: store memes", "err", err)
		return
	}
	var jobs []*job
	for _, m := range photos {
		mr, ok, err := c.memeRow(ctx, m.ID)
		if err != nil {
			c.log.WarnContext(ctx, "feed: read meme", "meme_id", m.ID, "err", err)
			continue
		}
		if ok && (mr.fileID != "" || !mr.failedAt.IsZero()) {
			continue
		}
		jobs = append(jobs, apiWarmupJob(m))
	}
	queued := c.pushWarmupJobs(ctx, jobs)
	c.log.DebugContext(ctx, "feed: API warmup queued", "photos", len(photos), "new_jobs", queued)
}

// queueAPIWarmup queues every meme the feed reported that is still not
// uploaded: transient failures and a full queue are retried here, once per
// WarmupInterval, without listing the collection again.
func (c *Cache) queueAPIWarmup(ctx context.Context) error {
	memes, err := c.pendingAPIWarmup(ctx)
	if err != nil {
		return err
	}
	jobs := make([]*job, len(memes))
	for i, m := range memes {
		jobs[i] = apiWarmupJob(m)
	}
	queued := c.pushWarmupJobs(ctx, jobs)
	c.log.DebugContext(ctx, "API warmup queued", "new_jobs", queued, "pending", len(memes))
	return nil
}

func apiWarmupJob(m memesearch.Meme) *job {
	return &job{memeID: m.ID, meme: m, caption: fmt.Sprintf("meme %d", m.ID), filename: photoName(m), date: nameDate(m.Filename)}
}

func apiJobKey(id int64) string { return "api:" + strconv.FormatInt(id, 10) }

// indexed reports whether m's file is in the library index. A name found in
// another folder does not count when the index has m's folder at all: then
// the folder layout matches and m's own file is simply not scanned yet. Without
// that folder the layouts differ (LibraryDir at another level) and only the
// name can be trusted, or every poll would ask for a rescan.
func (c *Cache) indexed(ctx context.Context, m memesearch.Meme) (bool, error) {
	rows, err := c.libraryFor(ctx, m)
	if err != nil || len(rows) == 0 || m.Folder == "" {
		return len(rows) > 0, err
	}
	if rows[0].relPath == path.Join(m.Folder, m.Filename) {
		return true, nil
	}
	has, err := c.libraryHasFolder(ctx, m.Folder)
	return !has, err
}
