package mediacache

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func (e *env) prepare(m memesearch.Meme) Photo {
	e.t.Helper()
	p, err := e.c.Prepare(e.t.Context(), m)
	if err != nil {
		e.t.Fatalf("Prepare(%d) = %v", m.ID, err)
	}
	if (p.FileID == "") == (p.Data == nil) {
		e.t.Fatalf("Prepare(%d) = %+v, want exactly one of FileID and Data", m.ID, p)
	}
	return p
}

func TestPrepareWithCacheChatSendsBytesNotQueue(t *testing.T) {
	// UploadsPerMin 1 and a used-up window: anything going through the queue
	// would block, so returning at all proves Prepare bypasses it.
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 1, WarmupUploadsPerMin: 1})
	e.file("a/uniq.jpg", "local uniq")
	e.scan()
	e.api.content[1] = []byte("used the window")
	e.api.content[2] = []byte("api bytes")
	e.run()
	e.resolve(memesearch.Meme{ID: 1})
	sentBefore := len(e.sender.sent())

	if p := e.prepare(memesearch.Meme{ID: 2, Filename: "far.jpg"}); string(p.Data) != "api bytes" || p.LibraryPath != "" {
		t.Fatalf("Prepare = %+v, want API bytes", p)
	}
	if p := e.prepare(memesearch.Meme{ID: 3, Filename: "uniq.jpg"}); string(p.Data) != "local uniq" || p.LibraryPath != "a/uniq.jpg" {
		t.Fatalf("Prepare = %+v, want library bytes", p)
	}
	if n := len(e.sender.sent()); n != sentBefore {
		t.Fatalf("Prepare uploaded to the cache chat (%d sends)", n-sentBefore)
	}
	if e.c.queue.Len(0) != 0 || e.c.queue.Len(1) != 0 {
		t.Fatal("Prepare queued an upload")
	}
}

func TestRememberPhotoRecordsLibraryRow(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/uniq.jpg", "local uniq")
	e.file("a/dup.jpg", "dup one")
	e.file("b/dup.jpg", "dup two")
	if st := e.scan(); st.pending != 3 {
		t.Fatalf("pending = %d, want 3", st.pending)
	}
	e.api.content[2] = []byte("dup two")
	ctx := t.Context()
	p1 := e.prepare(memesearch.Meme{ID: 1, Filename: "uniq.jpg"})
	p2 := e.prepare(memesearch.Meme{ID: 2, Filename: "dup.jpg"})
	if p2.LibraryPath != "b/dup.jpg" {
		t.Fatalf("collision not settled by hash: %+v", p2)
	}
	if err := e.c.RememberPhoto(ctx, 1, p1, "fid-u", "uniq-u"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RememberPhoto(ctx, 2, p2, "fid-d", "uniq-d"); err != nil {
		t.Fatal(err)
	}
	if r := e.libRow("a/uniq.jpg"); r.fileID != "fid-u" || r.fileUniqueID != "uniq-u" {
		t.Fatalf("row = %+v", r)
	}
	if st := e.scan(); st.pending != 1 {
		t.Fatalf("pending = %d, want only a/dup.jpg", st.pending)
	}
	if ok, _ := e.c.KnownUniqueID(ctx, "uniq-d"); !ok {
		t.Fatal("file_unique_id of the sent library file unknown")
	}
	if got, ok := e.c.Lookup(ctx, memesearch.Meme{ID: 2, Filename: "dup.jpg"}); !ok || got != "fid-d" {
		t.Fatalf("Lookup = %q, %v", got, ok)
	}
	if err := e.c.RememberPhoto(ctx, 3, Photo{Data: []byte("x")}, "", ""); err == nil {
		t.Fatal("empty file_id accepted")
	}
}

func TestEnqueueDownloadsOnlyWhenTheJobRuns(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 1, WarmupUploadsPerMin: 1})
	const n = 4
	for i := int64(1); i <= n; i++ {
		e.api.content[i] = []byte{byte(i)}
	}
	e.run()
	for i := int64(1); i <= n; i++ {
		e.c.Enqueue(memesearch.Meme{ID: i})
	}
	for turn := 1; turn <= n; turn++ {
		eventually(t, fmt.Sprintf("upload %d", turn), func() bool { return len(e.sender.sent()) == turn })
		if turn < n && !e.clk.WaitForWaiters(1, 5*time.Second) {
			t.Fatal("queue not waiting for the rate limit")
		}
		if got := e.api.calls(); got != turn {
			t.Fatalf("after %d uploads: %d API downloads, want %d", turn, got, turn)
		}
		e.clk.Advance(time.Minute)
	}
}

func TestPrepareWithoutCacheChat(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/uniq.jpg", "local uniq")
	e.file("a/dup.jpg", "dup one")
	e.file("b/dup.jpg", "dup two")
	e.scan()
	e.api.content[2] = []byte("dup two")
	e.api.content[3] = []byte("remote only")
	e.api.content[5] = []byte("api uniq")

	if p := e.prepare(memesearch.Meme{ID: 1, Filename: "uniq.jpg"}); string(p.Data) != "local uniq" || p.Name != "uniq.jpg" {
		t.Fatalf("unique name: %+v", p)
	}
	if e.api.calls() != 0 {
		t.Fatal("unique library name downloaded from the API")
	}
	if p := e.prepare(memesearch.Meme{ID: 2, Filename: "dup.jpg"}); string(p.Data) != "dup two" || p.Name != "dup.jpg" {
		t.Fatalf("collision: %+v", p)
	}
	if p := e.prepare(memesearch.Meme{ID: 3, Filename: "far.png"}); string(p.Data) != "remote only" || p.Name != "far.png" {
		t.Fatalf("no row: %+v", p)
	}
	if p := e.prepare(memesearch.Meme{ID: 3}); p.Name != "meme-3.jpg" {
		t.Fatalf("no filename: %+v", p)
	}
	if err := e.c.Remember(t.Context(), 3, "fid-sent", "uniq-sent"); err != nil {
		t.Fatal(err)
	}
	if p := e.prepare(memesearch.Meme{ID: 3, Filename: "far.png"}); p.FileID != "fid-sent" {
		t.Fatalf("after Remember: %+v", p)
	}
	if len(e.sender.sent()) != 0 {
		t.Fatal("Prepare uploaded without a cache chat")
	}
}

func TestPrepareRejectsNonPhoto(t *testing.T) {
	e := newEnv(t, Config{})
	e.api.media = map[int64]string{4: "video/mp4"}
	e.api.content[4] = []byte("video")
	if _, err := e.c.Prepare(t.Context(), memesearch.Meme{ID: 4}); !errors.Is(err, ErrNotPhoto) {
		t.Fatalf("Prepare = %v, want ErrNotPhoto", err)
	}
}

func TestDirect(t *testing.T) {
	api := &fakeAPI{content: map[int64][]byte{1: []byte("bytes")}}
	d := NewDirect(api, nil)
	ctx := t.Context()
	if d.UploadsEnabled() {
		t.Fatal("Direct must not enable uploads")
	}
	d.Enqueue(memesearch.Meme{ID: 1})
	if _, ok := d.Lookup(ctx, memesearch.Meme{ID: 1}); ok {
		t.Fatal("Direct Lookup hit")
	}
	if err := d.Remember(ctx, 1, "f", "u"); err != nil {
		t.Fatal(err)
	}
	if err := d.RememberPhoto(ctx, 1, Photo{Data: []byte("x")}, "f", "u"); err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(ctx, memesearch.Meme{ID: 1, Filename: "x.png"})
	if err != nil || string(p.Data) != "bytes" || p.Name != "x.png" || p.FileID != "" {
		t.Fatalf("Prepare = %+v, %v", p, err)
	}
	if _, ok := d.Lookup(ctx, memesearch.Meme{ID: 1}); ok {
		t.Fatal("Direct remembered something")
	}
	api.media = map[int64]string{1: "video/mp4"}
	if _, err := d.Prepare(ctx, memesearch.Meme{ID: 1}); !errors.Is(err, ErrNotPhoto) {
		t.Fatalf("Prepare of a video = %v, want ErrNotPhoto", err)
	}
}

// The search response already says a meme is a GIF or a video; downloading it
// (up to 25 MB, on every inline keystroke) only to reject it must not happen.
func TestNonPhotoMemesAreNotDownloaded(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[4] = []byte("video")
	e.api.media = map[int64]string{4: "video/mp4"}
	e.run()
	for _, mt := range []string{"video/mp4", "image/gif"} {
		m := memesearch.Meme{ID: 4, Filename: "clip", MediaType: mt}
		if _, err := e.c.Prepare(t.Context(), m); !errors.Is(err, ErrNotPhoto) {
			t.Errorf("Prepare(%s) = %v, want ErrNotPhoto", mt, err)
		}
		if _, err := e.c.Resolve(t.Context(), m); !errors.Is(err, ErrNotPhoto) {
			t.Errorf("Resolve(%s) = %v, want ErrNotPhoto", mt, err)
		}
		e.c.Enqueue(m)
		if _, err := NewDirect(e.api, nil).Prepare(t.Context(), m); !errors.Is(err, ErrNotPhoto) {
			t.Errorf("Direct.Prepare(%s) = %v, want ErrNotPhoto", mt, err)
		}
	}
	if n := e.api.calls(); n != 0 {
		t.Fatalf("non-photo memes downloaded %d times, want 0", n)
	}
}
