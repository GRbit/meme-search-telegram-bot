package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func TestParseOverrides(t *testing.T) {
	for _, tc := range []struct {
		in      string
		query   string
		tags    []string
		folders []string
		all     bool
	}{
		{in: "funny cat", query: "funny cat"},
		{in: "cat #funny", query: "cat", tags: []string{"funny"}},
		{in: "#кот мем", query: "мем", tags: []string{"кот"}},
		{in: "c# code", query: "c# code"},
		{in: "#a  #b #a x", query: "x", tags: []string{"a", "b"}},
		{in: "#cat, dog", query: "dog", tags: []string{"cat"}},
		{in: "#", query: "#"},
		{in: "#tag", query: "", tags: []string{"tag"}},
		{in: "dir:tg-alice x", query: "x", folders: []string{"tg-alice"}},
		{in: "DIR:Memes x", query: "x", folders: []string{"Memes"}},
		{in: "dir:* x", query: "x", all: true},
		{in: "dir: x", query: "dir: x"},
		{in: "a dir:x b dir:x", query: "a b", folders: []string{"x"}},
		{in: "  spaced   out  ", query: "spaced out"},
	} {
		o := parseOverrides(tc.in)
		if o.query != tc.query || !slices.Equal(o.tags, tc.tags) || !slices.Equal(o.folders, tc.folders) || o.allFolders != tc.all {
			t.Errorf("parseOverrides(%q) = %+v", tc.in, o)
		}
	}
}

func TestOverrideTagsReplaceSavedTags(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:tags"))
	e.process(textUpdate("dog"))
	e.process(textUpdate("#cat funny"))
	if len(api.searches) != 1 || api.searches[0] != "funny" || !slices.Equal(api.opts[0].Tags, []string{"cat"}) {
		t.Fatalf("searches = %q opts = %+v", api.searches, api.opts)
	}
	e.process(textUpdate("plain"))
	if !slices.Equal(api.opts[1].Tags, []string{"dog"}) {
		t.Fatalf("saved tags not used again: %+v", api.opts[1])
	}
}

// Filters alone are not a query: nothing to search, nothing to charge.
func TestOverridesWithoutQuery(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("#cat dir:x"))
	if api.searchCount() != 0 {
		t.Fatal("searched without a query")
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "#cat") {
		t.Fatalf("help = %q", txt)
	}
	if Chargeable(textUpdate("#cat dir:x")) {
		t.Fatal("filters-only text is charged")
	}
}

// The API limit applies to what is left after the filters.
func TestOverridesQueryLengthAfterStripping(t *testing.T) {
	q := strings.Repeat("a", memesearch.MaxQueryLen)
	if !Chargeable(textUpdate("#cat " + q)) {
		t.Fatal("200-char query with a tag is not charged")
	}
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("#cat " + q))
	if api.searchCount() != 1 || api.searches[0] != q {
		t.Fatalf("searches = %d", api.searchCount())
	}
}

func TestOverrideTagLimits(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("#1 #2 #3 #4 #5 #6 #7 #8 #9 #10 #11 x"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "10") {
		t.Fatalf("reply = %q", txt)
	}
	e.process(textUpdate("#" + strings.Repeat("x", memesearch.MaxTagLen+1) + " y"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "20") {
		t.Fatalf("reply = %q", txt)
	}
	if api.searchCount() != 0 {
		t.Fatal("invalid tags reached the API")
	}
}

// The server cannot filter by folder yet: search everywhere and say so.
func TestFolderOverrideNote(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("dir:tg-alice funny"))
	if api.searchCount() != 1 || api.searches[0] != "funny" {
		t.Fatalf("searches = %q", api.searches)
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "folder") {
		t.Fatalf("control = %q", txt)
	}
}
