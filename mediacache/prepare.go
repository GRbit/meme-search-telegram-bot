package mediacache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// Photo is one meme in a sendable form: exactly one of FileID or Data is set.
type Photo struct {
	FileID string
	Name   string // file name for the upload (base name of the meme)
	Data   []byte
}

// Prepare returns m in a sendable form for a user who is waiting (private chat album).
//
// Order: a known file_id (Lookup); with a cache chat, Resolve (real-time
// upload) and its file_id; otherwise the bytes of the library file when the
// base name is unique, else the API content. Callers that sent Data report
// the result with Remember so the next Prepare returns a FileID.
//
// With a cache chat, Prepare falls back to Data only when the upload queue is
// full or stopped: the user should still get the album. Other Resolve errors
// (ErrRejected, ErrNotPhoto, API failures) are returned, as sending the same
// bytes would fail the same way. For colliding base names the API bytes are
// returned as they are: hashing them to pick a library row would yield the
// same bytes.
func (c *Cache) Prepare(ctx context.Context, m memesearch.Meme) (Photo, error) {
	if err := notPhoto(m); err != nil {
		return Photo{}, err
	}
	if id, ok := c.Lookup(ctx, m); ok {
		return Photo{FileID: id, Name: photoName(m)}, nil
	}
	if c.cfg.CacheChatID != 0 {
		id, err := c.Resolve(ctx, m)
		if err == nil {
			return Photo{FileID: id, Name: photoName(m)}, nil
		}
		if !errors.Is(err, ErrStopped) && !errors.Is(err, ErrQueueFull) {
			return Photo{}, err
		}
		c.log.DebugContext(ctx, "prepare: upload unavailable, sending bytes", "meme_id", m.ID, "err", err)
	}
	rows, err := c.libraryByName(ctx, m.Filename)
	if err != nil {
		c.log.WarnContext(ctx, "prepare: library lookup failed, using the API", "meme_id", m.ID, "err", err)
	}
	if len(rows) == 1 {
		data, err := os.ReadFile(c.absPath(rows[0].relPath))
		if err == nil {
			c.log.DebugContext(ctx, "prepare: bytes from library file", "meme_id", m.ID, "rel_path", rows[0].relPath)
			return Photo{Name: path.Base(rows[0].relPath), Data: data}, nil
		}
		c.log.DebugContext(ctx, "prepare: library file unreadable, using the API", "meme_id", m.ID,
			"rel_path", rows[0].relPath, "err", err)
	}
	ct, err := fetchPhoto(ctx, c.api, c.log, m)
	if err != nil {
		return Photo{}, err
	}
	return Photo{Name: photoName(m), Data: ct.Data}, nil
}

func photoName(m memesearch.Meme) string {
	if m.Filename != "" {
		return m.Filename
	}
	return fmt.Sprintf("meme-%d.jpg", m.ID)
}

// notPhoto rejects memes the API already labels as something Telegram cannot
// send as a photo, before any download. An empty media type is let through and
// checked again on the downloaded bytes.
func notPhoto(m memesearch.Meme) error {
	if m.MediaType == "" || isPhotoType(m.MediaType) {
		return nil
	}
	return fmt.Errorf("%w: meme %d is %s", ErrNotPhoto, m.ID, m.MediaType)
}

func isPhotoType(mt string) bool {
	return strings.HasPrefix(mt, "image/") && mt != "image/gif"
}

// fetchPhoto downloads m and rejects media Telegram cannot send as a photo.
// GIF counts as not a photo: Telegram turns it into an animation.
func fetchPhoto(ctx context.Context, api memesearch.Client, log *slog.Logger, m memesearch.Meme) (memesearch.Content, error) {
	if err := notPhoto(m); err != nil {
		return memesearch.Content{}, err
	}
	start := time.Now()
	ct, err := api.Content(ctx, m.ID)
	if err != nil {
		return ct, fmt.Errorf("mediacache: fetch meme %d: %w", m.ID, err)
	}
	log.DebugContext(ctx, "fetched meme content", "meme_id", m.ID, "bytes", len(ct.Data),
		"media_type", ct.MediaType, "duration", time.Since(start))
	if !isPhotoType(ct.MediaType) {
		return ct, fmt.Errorf("%w: meme %d is %s", ErrNotPhoto, m.ID, ct.MediaType)
	}
	return ct, nil
}

// Direct serves memes straight from the API: no cache channel, no database,
// nothing remembered. It is for hosts that run without a media cache; it has
// the same Lookup, Enqueue, Prepare and Remember methods as Cache.
type Direct struct {
	api memesearch.Client
	log *slog.Logger
}

// NewDirect returns a Direct over api.
func NewDirect(api memesearch.Client) *Direct {
	return &Direct{api: api, log: slog.New(slog.DiscardHandler)}
}

// UploadsEnabled is always false: there is no cache channel.
func (d *Direct) UploadsEnabled() bool { return false }

// Lookup always misses.
func (d *Direct) Lookup(context.Context, memesearch.Meme) (string, bool) { return "", false }

// Enqueue does nothing.
func (d *Direct) Enqueue(...memesearch.Meme) {}

// Prepare returns the API content of m as Data.
func (d *Direct) Prepare(ctx context.Context, m memesearch.Meme) (Photo, error) {
	ct, err := fetchPhoto(ctx, d.api, d.log, m)
	if err != nil {
		return Photo{}, err
	}
	return Photo{Name: photoName(m), Data: ct.Data}, nil
}

// Remember does nothing and returns nil.
func (d *Direct) Remember(context.Context, int64, string, string) error { return nil }
