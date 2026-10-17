package mediacache

import (
	"errors"
	"testing"

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

func TestPrepareWithCacheChatResolves(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("one")
	e.run()
	if p := e.prepare(memesearch.Meme{ID: 1, Filename: "one.jpg"}); p.FileID != "fid-1" {
		t.Fatalf("Prepare = %+v, want the uploaded file_id", p)
	}
	if p := e.prepare(memesearch.Meme{ID: 1, Filename: "one.jpg"}); p.FileID != "fid-1" || len(e.sender.sent()) != 1 {
		t.Fatalf("second Prepare = %+v after %d uploads", p, len(e.sender.sent()))
	}
}

func TestPrepareFallsBackToBytesWhenStopped(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("one")
	stop := e.run()
	stop()
	if p := e.prepare(memesearch.Meme{ID: 1, Filename: "one.jpg"}); string(p.Data) != "one" || p.Name != "one.jpg" {
		t.Fatalf("Prepare = %+v, want API bytes", p)
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
	d := NewDirect(api)
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
		if _, err := NewDirect(e.api).Prepare(t.Context(), m); !errors.Is(err, ErrNotPhoto) {
			t.Errorf("Direct.Prepare(%s) = %v, want ErrNotPhoto", mt, err)
		}
	}
	if n := e.api.calls(); n != 0 {
		t.Fatalf("non-photo memes downloaded %d times, want 0", n)
	}
}
