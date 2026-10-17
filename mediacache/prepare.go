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
	// LibraryPath is the library file Data was read from or matched by
	// SHA-256 (relative, forward slashes); empty for bytes known only to the
	// API. RememberPhoto stores the file_id on that row too.
	LibraryPath string
}

// Prepare returns m in a sendable form for a user who is waiting (private chat album).
//
// It never waits for the cache channel queue, with or without a cache chat:
// a waiting user goes first, and the file_ids Telegram returns for the album
// work everywhere for this bot. Order: a known file_id (Lookup); else the
// bytes of the library file when the base name is unique; else the API
// content, matched to a library file by SHA-256 when names collide. Callers
// that sent Data report the result with RememberPhoto, so the next Prepare
// returns a FileID and warmup skips the file.
func (c *Cache) Prepare(ctx context.Context, m memesearch.Meme) (Photo, error) {
	if err := notPhoto(m); err != nil {
		return Photo{}, err
	}
	if id, ok := c.Lookup(ctx, m); ok {
		return Photo{FileID: id, Name: photoName(m)}, nil
	}
	rows, err := c.libraryByName(ctx, m.Filename)
	if err != nil {
		c.log.WarnContext(ctx, "prepare: library lookup failed, using the API", "meme_id", m.ID, "err", err)
	}
	if len(rows) == 1 {
		rel := rows[0].relPath
		data, err := os.ReadFile(c.absPath(rel))
		if err == nil {
			c.log.DebugContext(ctx, "prepare: bytes from library file", "meme_id", m.ID, "rel_path", rel)
			return Photo{Name: path.Base(rel), Data: data, LibraryPath: rel}, nil
		}
		c.log.DebugContext(ctx, "prepare: library file unreadable, using the API", "meme_id", m.ID,
			"rel_path", rel, "err", err)
	}
	ct, err := fetchPhoto(ctx, c.api, c.log, m)
	if err != nil {
		return Photo{}, err
	}
	p := Photo{Name: photoName(m), Data: ct.Data}
	if len(rows) > 1 {
		if r, ok := matchRow(rows, ct.Data); ok {
			p.LibraryPath = r.relPath
		}
		c.log.DebugContext(ctx, "prepare: name collision", "meme_id", m.ID, "candidates", len(rows),
			"matched", p.LibraryPath)
	}
	return p, nil
}

// RememberPhoto records the file_id Telegram returned for a Photo from
// Prepare that was sent as Data: by meme id and, when the bytes came from a
// library file, on its row, so warmup does not upload it again and
// KnownUniqueID covers it.
func (c *Cache) RememberPhoto(ctx context.Context, memeID int64, p Photo, fileID, fileUniqueID string) error {
	if fileID == "" {
		return errors.New("mediacache: remember: empty file_id")
	}
	if p.LibraryPath != "" {
		if err := c.setLibraryFileID(ctx, p.LibraryPath, fileID, fileUniqueID); err != nil {
			return err
		}
	}
	if err := c.rememberMeme(ctx, memeID, fileID, fileUniqueID, p.LibraryPath); err != nil {
		return err
	}
	c.log.DebugContext(ctx, "remembered sent photo", "meme_id", memeID, "rel_path", p.LibraryPath)
	return nil
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

// NewDirect returns a Direct over api; a nil log discards its debug lines.
func NewDirect(api memesearch.Client, log *slog.Logger) *Direct {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Direct{api: api, log: log}
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

// RememberPhoto does nothing and returns nil.
func (d *Direct) RememberPhoto(context.Context, int64, Photo, string, string) error { return nil }
