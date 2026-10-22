package saver

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// noticeEnv is an env whose server has the listing (so the change feed can
// report descriptions) and whose clock the test moves.
func noticeEnv(t *testing.T, caps memesearch.Capabilities) (*env, *time.Time) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	e := newEnvWith(t, nil, func(d *Deps) {
		d.DB = db
		d.Caps = memesearch.StaticCapabilities(caps)
		d.Now = func() time.Time { return now }
	})
	return e, &now
}

func described(name string, st memesearch.DescriptionStatus) memesearch.Meme {
	return memesearch.Meme{ID: 7, Filename: name, MediaType: "image/jpeg", DescriptionStatus: st}
}

func savedName(id int, data string) string {
	return "2026-10-18_alice_" + strconv.Itoa(id) + "_" + hash20(data) + ".jpg"
}

func (e *env) changed(ms ...memesearch.Meme) { e.mod.MemesChanged(context.Background(), ms) }

func TestSearchableNoticeRepliesToTheSavedMeme(t *testing.T) {
	e, _ := noticeEnv(t, memesearch.Capabilities{Listing: true})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)

	e.changed(described(savedName(5, "meme"), memesearch.StatusPending), described("other.jpg", memesearch.StatusReady))
	e.api.noMoreReplies(t)

	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	if got := e.api.reply(t); got != "Search finds this meme now." {
		t.Fatalf("notice = %q", got)
	}
	if got := e.api.replyTo[len(e.api.replyTo)-1]; got != 5 {
		t.Fatalf("notice replies to %d, want the saved message 5", got)
	}
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
}

func TestSearchableNoticeWaitsForTheWholeAlbum(t *testing.T) {
	e, _ := noticeEnv(t, memesearch.Capabilities{Listing: true})
	e.api.files["a"] = []byte("A")
	e.api.files["b"] = []byte("B")
	for i, id := range []string{"a", "b"} {
		m := photoMsg(20+i, id)
		m.MediaGroupID = "grp"
		e.send(m)
	}
	e.fireTimers()
	e.api.reply(t)

	e.changed(described(savedName(20, "A"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
	e.changed(described(savedName(21, "B"), memesearch.StatusFailed))
	want := "Search finds 1 of these memes now.\nCould not be described, search will not find them: 1."
	if got := e.api.reply(t); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if got := e.api.replyTo[len(e.api.replyTo)-1]; got != 20 {
		t.Fatalf("notice replies to %d, want the album's first message", got)
	}
}

func TestSearchableNoticeFailedSingle(t *testing.T) {
	e, _ := noticeEnv(t, memesearch.Capabilities{Listing: true})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	e.changed(described(savedName(5, "meme"), memesearch.StatusFailed))
	if got := e.api.reply(t); got != "meme-search could not describe this meme, so search will not find it." {
		t.Fatalf("notice = %q", got)
	}
}

func TestNoSearchableNoticeOnAnOldServer(t *testing.T) {
	e, _ := noticeEnv(t, memesearch.Capabilities{})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
}

func TestNoSearchableNoticeWithoutDB(t *testing.T) {
	e := newEnvWith(t, nil, func(d *Deps) { d.Caps = memesearch.StaticCapabilities{Listing: true} })
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
}

func TestSearchableNoticeExpires(t *testing.T) {
	e, now := noticeEnv(t, memesearch.Capabilities{Listing: true})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	*now = now.Add(noticeTTL + time.Minute)
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
}

func TestSearchableNoticeCanBeTurnedOff(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := newEnvWith(t, func(c *Config) { c.NoNotify = true }, func(d *Deps) {
		d.DB = db
		d.Caps = memesearch.StaticCapabilities{Listing: true}
	})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	e.api.noMoreReplies(t)
}
