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
	const other = "fedcba9876543210fedc"
	for name, want := range map[string]string{
		"2026-10-18_alice_123_" + hash20 + ".jpg":            hash20, // the saver's own layout
		"2026-10-18_alice_123_" + hash20:                     hash20, // no extension
		"2026-10-18_alice_123_" + hash20 + "_1700000000.jpg": hash20, // meme-search's clash suffix
		hash20 + ".jpg":                                  hash20, // the whole name
		"copy of " + hash20 + " (2).png":                 hash20, // bounded by non-hex characters
		"2026-10-18_alice_123_0123456789ABCDEF0123.jpg":  hash20, // compared case-insensitively
		"x" + hash20 + ".jpg":                            hash20, // x is not hex, so it bounds the run
		"2026-10-18_alice_123_0123456789abcdef012.jpg":   "",     // 19 hex
		"2026-10-18_alice_123_0123456789abcdef01234.jpg": "",     // 21 hex: a longer run is not a hash
		"a" + hash20 + ".jpg":                            "",     // a is hex: the run is 21 long
		hash20 + "_" + other + ".jpg":                    "",     // two different runs: ambiguous
		hash20 + "_" + hash20 + ".jpg":                   hash20, // the same run twice is one hash
		"2026-10-18_alice_123.jpg":                       "",     // saved before names had a hash
		"cat.jpg":                                        "",
		".jpg":                                           "",
		"":                                               "",
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
		{"same hash, other names", nil, []memesearch.Meme{named(1, saved("alice")), named(2, saved("bob")), named(3, "dog.jpg")}, []int64{1, 3}, 1},
		{"same hash, other case and layout", nil, []memesearch.Meme{named(1, saved("alice")), named(2, "0123456789ABCDEF0123.png")}, []int64{1}, 1},
		{"same file name alone is not a duplicate", nil, []memesearch.Meme{named(1, "cat.jpg"), named(2, "cat.jpg")}, []int64{1, 2}, 0},
		{"same meme twice", nil, []memesearch.Meme{named(1, "a.jpg"), named(1, "a.jpg")}, []int64{1}, 1},
		{"ambiguous names are not matched", nil, []memesearch.Meme{named(1, hash20+"_fedcba9876543210fedc.jpg"), named(2, saved("bob"))}, []int64{1, 2}, 0},
		{"keeps the best ranked", nil, []memesearch.Meme{named(5, saved("x")), named(1, saved("y"))}, []int64{5}, 1},
		{"seeded with what was shown", []memesearch.Meme{named(9, saved("alice"))}, []memesearch.Meme{named(1, saved("bob")), named(9, "z.jpg"), named(2, "dog.jpg")}, []int64{2}, 2},
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
	api := &fakeAPI{results: []memesearch.Meme{named(1, "a_"+hash20+".jpg"), named(2, "b_"+hash20+".jpg"), named(3, "dog.jpg")}}
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
	srv.results[0] = named(1, "a_"+hash20+".jpg")
	srv.results[22] = named(23, "b_"+hash20+".jpg") // a copy of the best result, saved under another name
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	pageTo(t, e, 5)
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-21", "prep-22", "prep-24", "prep-25", "prep-26"}) {
		t.Fatalf("page 5 album = %v", got)
	}
}

func TestInlineDropsDuplicates(t *testing.T) {
	api := &fakeAPI{results: []memesearch.Meme{named(1, "a_"+hash20+".jpg"), named(2, "b_"+hash20+".jpg"), named(3, "dog.jpg")}}
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
	results := []memesearch.Meme{named(1, "a_"+hash20+".jpg"), named(2, "dog.jpg"), named(3, "b_"+hash20+".jpg"), named(4, "owl.jpg")}
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
