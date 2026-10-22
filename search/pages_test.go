package search

import (
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestPageStoreBoundedAndTTL(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPageStore(time.Hour, 3)
	toks := make([]string, 3)
	for i := range toks {
		toks[i] = p.put(&session{created: t0.Add(time.Duration(i) * time.Minute)}, t0)
	}
	// Full: the oldest live session makes room.
	newest := p.put(&session{created: t0.Add(10 * time.Minute)}, t0.Add(10*time.Minute))
	if p.len() != 3 || p.get(toks[0], t0.Add(10*time.Minute)) != nil || p.get(newest, t0.Add(10*time.Minute)) == nil {
		t.Fatalf("len=%d", p.len())
	}
	// Past TTL a session is gone on read.
	if p.get(toks[1], t0.Add(time.Hour+time.Minute)) != nil {
		t.Fatal("expired session returned")
	}
	// When full of expired sessions, put drops them all instead of only the oldest.
	p.put(&session{created: t0.Add(11 * time.Minute)}, t0.Add(11*time.Minute))
	if p.len() != 3 {
		t.Fatalf("len=%d, want full", p.len())
	}
	later := t0.Add(5 * time.Hour)
	p.put(&session{created: later}, later)
	if p.len() != 1 {
		t.Fatalf("len=%d after expiry sweep", p.len())
	}
}

func TestTokensUniqueAndShort(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok := newToken()
		if len(tok) != 12 || seen[tok] {
			t.Fatalf("token %q", tok)
		}
		seen[tok] = true
	}
	if d := moreData(newToken(), 99); len(d) > 64 {
		t.Fatalf("callback data %d bytes", len(d))
	}
}

func TestParseMore(t *testing.T) {
	for data, want := range map[string]struct {
		tok  string
		page int
		ok   bool
	}{
		"srch:m:abc:2":  {"abc", 2, true},
		"srch:m:abc:0":  {"", 0, false},
		"srch:m:abc:x":  {"", 0, false},
		"srch:m::2":     {"", 0, false},
		"srch:m:abc":    {"", 0, false},
		"srch:s:abc:2":  {"", 0, false},
		"other:m:abc:2": {"", 0, false},
	} {
		tok, page, ok := parseMore(data)
		if tok != want.tok || page != want.page || ok != want.ok {
			t.Errorf("%q -> %q %d %v", data, tok, page, ok)
		}
	}
	if tok, page, ok := parseMore(moreData("tok", 3)); tok != "tok" || page != 3 || !ok {
		t.Fatal("round trip")
	}
}

func TestPageSlice(t *testing.T) {
	ms := memeRange(12)
	if pageCount(12, 5) != 3 || pageCount(10, 5) != 2 || pageCount(1, 5) != 1 {
		t.Fatal("pageCount")
	}
	if p := pageSlice(ms, 3, 5); len(p) != 2 || p[0].ID != 11 {
		t.Fatalf("page 3 = %v", p)
	}
	if pageSlice(ms, 0, 5) != nil || pageSlice(ms, 4, 5) != nil {
		t.Fatal("out of range page")
	}
}

func TestLargestPhoto(t *testing.T) {
	if _, ok := largestPhoto(nil); ok {
		t.Fatal("empty")
	}
	p, _ := largestPhoto([]models.PhotoSize{{FileID: "a", Width: 90, Height: 90}, {FileID: "b", Width: 1280, Height: 720}, {FileID: "c", Width: 320, Height: 240}})
	if p.FileID != "b" {
		t.Fatalf("got %s", p.FileID)
	}
}

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{
		"/start": "start", "/start payload": "start", "/help@bot": "help", "/help@bot x": "help", "hi": "", "/": "",
	} {
		if got := commandName(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestPageStoreMemeBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPageStore(time.Hour, 100)
	p.maxMemes = 50
	a := p.put(&session{created: t0, memes: memeRange(20)}, t0)
	b := p.put(&session{created: t0.Add(time.Minute), memes: memeRange(20)}, t0.Add(time.Minute))
	if p.get(a, t0.Add(2*time.Minute)) == nil { // a is now used more recently than b
		t.Fatal("a gone early")
	}
	c := p.put(&session{created: t0.Add(3 * time.Minute), memes: memeRange(20)}, t0.Add(3*time.Minute))
	now := t0.Add(4 * time.Minute)
	if p.get(b, now) != nil || p.get(a, now) == nil || p.get(c, now) == nil || p.memes() != 40 {
		t.Fatalf("after put: memes=%d", p.memes())
	}
	// Growth counts too, and never evicts the growing session itself.
	sc := p.get(c, now)
	p.grow(sc, 20, now)
	if p.get(a, now) != nil || p.get(c, now) == nil || p.memes() != 40 {
		t.Fatalf("after grow: memes=%d", p.memes())
	}
	// Expired sessions give their budget back.
	if p.get(c, now.Add(2*time.Hour)) != nil || p.memes() != 0 {
		t.Fatalf("after expiry: memes=%d", p.memes())
	}
}
