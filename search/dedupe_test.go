package search

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const hash20 = "0123456789abcdef0123"

func TestContentHash(t *testing.T) {
	for name, want := range map[string]string{
		"2026-10-18_alice_123_" + hash20 + ".jpg":            hash20,
		"2026-10-18_Some_Channel_5_" + hash20 + ".png":       hash20, // sanitized sources keep underscores
		"2026-10-18_alice_123_" + hash20:                     hash20, // no extension
		"2026-10-18_alice_123_0123456789ABCDEF0123.jpg":      "",     // the saver writes lowercase hex
		"2026-10-18_alice_123_0123456789abcdef012.jpg":       "",     // 19 hex chars
		"2026-10-18_alice_123_0123456789abcdef01234.jpg":     "",     // 21 hex chars
		"alice_123_" + hash20 + ".jpg":                       "",     // no date
		"2026-10-18_alice_x1_" + hash20 + ".jpg":             "",     // message id not a number
		"2026-10-18_alice_123_" + hash20 + "_1700000000.jpg": "",     // meme-search's clash suffix
		"2026-10-18_alice_123.jpg":                           "",     // saved before names had a hash
		"2026-10-18_alice_123_2.jpg":                         "",
		hash20 + ".jpg":                                      "",
		"2026-10-18__123_" + hash20 + ".jpg":                 "", // empty source
		"":                                                   "",
	} {
		if got := contentHash(name); got != want {
			t.Errorf("contentHash(%q) = %q, want %q", name, got, want)
		}
	}
}

func named(id int64, filename string) memesearch.Meme {
	m := memes(id)[0]
	m.Filename = filename
	return m
}

func ids(ms []memesearch.Meme) []int64 {
	var out []int64
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestDupKeysFilter(t *testing.T) {
	saved := func(src string) string { return "2026-10-18_" + src + "_7_" + hash20 + ".jpg" }
	for _, tc := range []struct {
		name    string
		seed    []memesearch.Meme
		in      []memesearch.Meme
		want    []int64
		dropped int
	}{
		{"same name in another folder", nil, []memesearch.Meme{named(1, "cat.jpg"), named(2, "cat.jpg"), named(3, "dog.jpg")}, []int64{1, 3}, 1},
		{"same saved hash, other names", nil, []memesearch.Meme{named(1, saved("alice")), named(2, saved("bob"))}, []int64{1}, 1},
		{"name rule is exact", nil, []memesearch.Meme{named(1, "cat.jpg"), named(2, "Cat.jpg"), named(3, "cat.png")}, []int64{1, 2, 3}, 0},
		{"same meme twice", nil, []memesearch.Meme{named(1, "a.jpg"), named(1, "a.jpg")}, []int64{1}, 1},
		{"empty names are not a match", nil, []memesearch.Meme{named(1, ""), named(2, "")}, []int64{1, 2}, 0},
		{"keeps the best ranked", nil, []memesearch.Meme{named(5, "x.jpg"), named(1, "x.jpg")}, []int64{5}, 1},
		{"seeded with what was shown", []memesearch.Meme{named(9, "cat.jpg")}, []memesearch.Meme{named(1, "cat.jpg"), named(9, "z.jpg"), named(2, "dog.jpg")}, []int64{2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := newDupKeys(tc.seed...).filter(tc.in)
			if !slices.Equal(ids(got), tc.want) || dropped != tc.dropped {
				t.Fatalf("kept %v dropped %d, want %v and %d", ids(got), dropped, tc.want, tc.dropped)
			}
		})
	}
}

func TestSearchDropsDuplicates(t *testing.T) {
	var buf bytes.Buffer
	api := &fakeAPI{results: []memesearch.Meme{named(1, "cat.jpg"), named(2, "cat.jpg"), named(3, "dog.jpg")}}
	e := newEnv(t, api, &fakeMedia{uploads: true}, withLog(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	e.process(textUpdate("cat"))
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-1", "prep-3"}) {
		t.Fatalf("album = %v", got)
	}
	if !strings.Contains(buf.String(), "duplicates_dropped=1") {
		t.Fatalf("drop count not logged:\n%s", buf.String())
	}
}

func TestMoreFetchDropsDuplicatesOfShownMemes(t *testing.T) {
	srv := pagingServer(30)
	srv.results[22] = named(23, "m1.jpg") // a copy of the best result, in another folder
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	pageTo(t, e, 5)
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-21", "prep-22", "prep-24", "prep-25", "prep-26"}) {
		t.Fatalf("page 5 album = %v", got)
	}
}

func TestInlineDropsDuplicates(t *testing.T) {
	api := &fakeAPI{results: []memesearch.Meme{named(1, "cat.jpg"), named(2, "cat.jpg"), named(3, "dog.jpg")}}
	media := &fakeMedia{uploads: true, known: map[int64]string{1: "f1", 2: "f2", 3: "f3"}}
	e := newEnv(t, api, media)
	e.process(inlineUpdate("cat"))
	res := e.lastSent(t, "answerInlineQuery").fields["results"]
	if !strings.Contains(res, `"f1"`) || strings.Contains(res, `"f2"`) || !strings.Contains(res, `"f3"`) {
		t.Fatalf("results = %s", res)
	}
}

// A copy of the meme "Similar" started from is the same picture, not a similar one.
func TestSimilarDropsCopiesOfTheSource(t *testing.T) {
	results := []memesearch.Meme{named(1, "cat.jpg"), named(2, "dog.jpg"), named(3, "cat.jpg"), named(4, "owl.jpg")}
	for i := range results {
		results[i].Description = "d"
	}
	api := &fakeAPI{results: results[:2]}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("cat"))
	api.results = results
	e.process(callbackUpdate(userID, similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]))
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-2", "prep-4"}) {
		t.Fatalf("similar album = %v", got)
	}
}
