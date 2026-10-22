package mediacache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/mediacache/internal/limiter"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// listServer fakes GET /api/v1/memes. With old set it answers like a stock
// server (Rails' HTML 404). Cursors are "c<index>"; anything else is a 422.
type listServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	old     bool
	memes   []memesearch.Meme
	cursors []string // every cursor received, "" for none
}

func newListServer(t *testing.T, memes ...memesearch.Meme) *listServer {
	t.Helper()
	ls := &listServer{memes: memes}
	ls.srv = httptest.NewServer(http.HandlerFunc(ls.serve))
	t.Cleanup(ls.srv.Close)
	return ls
}

func (ls *listServer) serve(w http.ResponseWriter, r *http.Request) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if r.URL.Path != "/api/v1/memes" || ls.old {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>Not Found</html>"))
		return
	}
	cursor := r.URL.Query().Get("cursor")
	ls.cursors = append(ls.cursors, cursor)
	start := 0
	if cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
		if !strings.HasPrefix(cursor, "c") || err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_cursor","message":"cursor is invalid"}}`))
			return
		}
		start = n
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	end := min(start+limit, len(ls.memes))
	start = min(start, end)
	data := []map[string]any{}
	for _, m := range ls.memes[start:end] {
		d := map[string]any{
			"id": m.ID, "filename": m.Filename, "description": nil, "tags": []string{},
			"media_type": m.MediaType, "content_url": "/api/v1/memes/" + strconv.FormatInt(m.ID, 10) + "/content",
			"description_status": "pending",
		}
		if m.Folder != "" { // older servers omit the field
			d["folder"] = m.Folder
		}
		data = append(data, d)
	}
	meta := map[string]any{"limit": limit, "count": len(data), "has_more": end < len(ls.memes)}
	if len(data) > 0 {
		meta["next_cursor"] = "c" + strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": meta})
}

func (ls *listServer) seen() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]string(nil), ls.cursors...)
}

func (ls *listServer) add(ms ...memesearch.Meme) {
	ls.mu.Lock()
	ls.memes = append(ls.memes, ms...)
	ls.mu.Unlock()
}

func (ls *listServer) client(t *testing.T) *memesearch.HTTPClient {
	t.Helper()
	c, err := memesearch.New(memesearch.Config{URL: ls.srv.URL, Token: "tok"}, ls.srv.Client(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func listed() memesearch.Capabilities { return memesearch.Capabilities{Listing: true} }

// newFeedEnv is newEnv with the feed wired to ls.
func newFeedEnv(t *testing.T, cfg Config, ls *listServer, caps memesearch.Capabilities) *env {
	t.Helper()
	e := newEnvWith(t, cfg, func(d *Deps) {
		d.Lister = ls.client(t)
		d.Caps = memesearch.StaticCapabilities(caps)
	})
	return e
}

func jpg(id int64, name string) memesearch.Meme {
	return memesearch.Meme{ID: id, Filename: name, MediaType: "image/jpeg"}
}

func noLibrary(t *testing.T) string { return filepath.Join(t.TempDir(), "not-mounted") }

func TestFeedWarmsUpFromTheAPIWithoutALibrary(t *testing.T) {
	ls := newListServer(t, jpg(1, "2026-01-01_a.jpg"),
		memesearch.Meme{ID: 2, Filename: "anim.gif", MediaType: "image/gif"}, jpg(3, "b.png"))
	e := newFeedEnv(t, Config{LibraryDir: noLibrary(t), CacheChatID: chatID}, ls, listed())
	e.api.content[1] = []byte("one")
	e.api.content[3] = []byte("three")
	e.run()
	e.c.pollFeed(t.Context())
	// The fake counts a send before the cache stores its file_id: wait for the store.
	eventually(t, "both photos cached", func() bool {
		_, ok1 := e.c.Lookup(t.Context(), jpg(1, "2026-01-01_a.jpg"))
		_, ok3 := e.c.Lookup(t.Context(), jpg(3, "b.png"))
		return ok1 && ok3
	})
	if n := len(e.sender.sent()); n != 2 {
		t.Fatalf("uploads = %d, want 2", n)
	}
	if n := e.api.calls(); n != 2 {
		t.Errorf("content downloads = %d, want 2 (the GIF is skipped)", n)
	}
}

func TestFeedResumesFromTheStoredCursor(t *testing.T) {
	ls := newListServer(t, jpg(1, "a.jpg"), jpg(2, "b.jpg"), jpg(3, "c.jpg"))
	e := newFeedEnv(t, Config{}, ls, listed())
	e.c.feedLimit, e.c.feedPages = 1, 2

	e.c.pollFeed(t.Context())
	if got := ls.seen(); !slices.Equal(got, []string{"", "c1"}) {
		t.Fatalf("first poll cursors = %q, want two pages per poll", got)
	}
	e.c.pollFeed(t.Context())
	e.c.pollFeed(t.Context()) // nothing newer: empty page, no next_cursor
	if got := ls.seen(); !slices.Equal(got, []string{"", "c1", "c2", "c3"}) {
		t.Fatalf("cursors = %q", got)
	}

	// A restart continues where the last poll ended.
	ls.add(jpg(4, "d.jpg"))
	c2, err := newCache(t.Context(), Config{}, Deps{DB: e.db, API: e.api, Lister: ls.client(t),
		Caps: memesearch.StaticCapabilities(listed()), Log: testLogger(t)}, e.clk)
	if err != nil {
		t.Fatal(err)
	}
	c2.pollFeed(t.Context())
	if got := ls.seen(); got[len(got)-1] != "c3" {
		t.Fatalf("after restart cursors = %q, want c3 last", got)
	}
}

func TestFeedOnAnOldServer(t *testing.T) {
	t.Run("capability off: no request", func(t *testing.T) {
		ls := newListServer(t, jpg(1, "a.jpg"))
		e := newFeedEnv(t, Config{LibraryDir: noLibrary(t), CacheChatID: chatID}, ls, memesearch.Capabilities{})
		e.c.pollFeed(t.Context())
		if got := ls.seen(); len(got) != 0 {
			t.Fatalf("requests = %q", got)
		}
	})
	t.Run("stale capability: 404 is harmless", func(t *testing.T) {
		ls := newListServer(t, jpg(1, "a.jpg"))
		ls.old = true
		e := newFeedEnv(t, Config{LibraryDir: noLibrary(t), CacheChatID: chatID}, ls, listed())
		e.c.pollFeed(t.Context())
		if n := e.c.queue.Len(limiter.Background); n != 0 {
			t.Fatalf("queued = %d", n)
		}
		if cur, err := e.c.feedCursor(t.Context()); err != nil || cur != "" {
			t.Fatalf("cursor = %q, %v", cur, err)
		}
	})
}

func TestFeedInvalidCursorStartsOver(t *testing.T) {
	ls := newListServer(t, jpg(1, "a.jpg"))
	e := newFeedEnv(t, Config{}, ls, listed())
	if err := e.c.setFeedCursor(t.Context(), "bogus"); err != nil {
		t.Fatal(err)
	}
	e.c.pollFeed(t.Context())
	e.c.pollFeed(t.Context())
	if got := ls.seen(); !slices.Equal(got, []string{"bogus", ""}) {
		t.Fatalf("cursors = %q", got)
	}
}

func TestFeedWithLibraryRescansForUnknownFiles(t *testing.T) {
	ls := newListServer(t, jpg(1, "known.jpg"), memesearch.Meme{ID: 2, Filename: "new.gif", MediaType: "image/gif"})
	e := newFeedEnv(t, Config{CacheChatID: chatID, WarmupInterval: time.Hour}, ls, listed())
	e.file("a/known.jpg", "k")
	e.runFull()
	eventually(t, "first scan", func() bool { return e.c.scans.Load() == 1 })

	e.c.pollFeed(t.Context())
	time.Sleep(20 * time.Millisecond)
	if n := e.c.scans.Load(); n != 1 {
		t.Fatalf("scans = %d: a known file or a GIF must not trigger a rescan", n)
	}

	e.file("b/new.jpg", "n")
	ls.add(jpg(3, "new.jpg"))
	e.c.pollFeed(t.Context())
	eventually(t, "rescan after feed", func() bool { return e.c.scans.Load() == 2 })
	if ok, _ := e.c.KnownSHA256(t.Context(), sum("n")); !ok {
		t.Fatal("rescan did not index the new file")
	}
	if n := e.api.calls(); n != 0 {
		t.Fatalf("content downloads = %d: with a library, warmup reads files", n)
	}
}

func TestAPIWarmupRetriesAndForgetsDeletedMemes(t *testing.T) {
	ls := newListServer(t, jpg(1, "a.jpg"), jpg(2, "gone.jpg"))
	e := newFeedEnv(t, Config{LibraryDir: noLibrary(t), CacheChatID: chatID}, ls, listed())
	e.api.content[1] = []byte("one") // 2 was deleted: Content answers 404
	e.sender.fail = func(n int, _ sent) error {
		if n == 1 {
			return errors.New("network down")
		}
		return nil
	}
	e.run()
	e.c.pollFeed(t.Context())
	eventually(t, "failed upload and deleted meme forgotten", func() bool {
		n, err := e.c.apiMemeCount(t.Context())
		return len(e.sender.sent()) == 1 && err == nil && n == 1
	})

	if err := e.c.queueAPIWarmup(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The fake counts a send before the cache stores its file_id: wait for the store.
	eventually(t, "meme 1 cached after the retry", func() bool {
		_, ok := e.c.Lookup(t.Context(), jpg(1, "a.jpg"))
		return ok
	})
	if n := len(e.sender.sent()); n != 2 {
		t.Fatalf("uploads = %d, want 2", n)
	}
	if n := e.api.calls(); n != 3 {
		t.Fatalf("content downloads = %d, want 3 (the deleted meme is not retried)", n)
	}
}

func TestFeedLoopPollsEveryInterval(t *testing.T) {
	ls := newListServer(t)
	e := newFeedEnv(t, Config{FeedInterval: time.Minute, WarmupInterval: time.Hour}, ls, listed())
	e.runFull()
	// No poll at startup: the capability probe has not answered yet, and a
	// poll before the first library scan would ask for a needless rescan.
	for want := 1; want <= 2; want++ {
		if !e.clk.WaitForWaiters(2, 5*time.Second) {
			t.Fatal("scan and feed loops not sleeping")
		}
		if n := len(ls.seen()); n != want-1 {
			t.Fatalf("polls = %d before tick %d", n, want)
		}
		e.clk.Advance(time.Minute)
		eventually(t, "poll after the interval", func() bool { return len(ls.seen()) == want })
	}
}

func (c *Cache) apiMemeCount(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mediacache_api_memes`).Scan(&n)
	return n, err
}
