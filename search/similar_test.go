package search

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func described(n int) []memesearch.Meme {
	ms := memeRange(n)
	for i := range ms {
		if i != 1 { // meme 2 has no description yet
			ms[i].Description = fmt.Sprintf("description of meme %d", ms[i].ID)
		}
	}
	return ms
}

func similarButtons(t *testing.T, c call) [][2]string {
	t.Helper()
	var out [][2]string
	for _, b := range buttons(t, c) {
		if strings.HasPrefix(b[1], "srch:s:") {
			out = append(out, b)
		}
	}
	return out
}

func TestSimilarButtonsForDescribedMemes(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: described(3)}, &fakeMedia{uploads: true})
	e.process(textUpdate("cat"))
	got := similarButtons(t, e.lastSent(t, "sendMessage"))
	if len(got) != 2 || got[0][0] != "Similar 1" || got[1][0] != "Similar 3" {
		t.Fatalf("similar buttons = %v", got)
	}
}

func TestSimilarSearchesByDescription(t *testing.T) {
	api := &fakeAPI{results: described(3)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	e.process(textUpdate("#cat funny"))
	data := similarButtons(t, e.lastSent(t, "sendMessage"))[1][1]
	if !Chargeable(callbackUpdate(userID, data)) {
		t.Fatal("similar runs a search and must be charged")
	}
	e.process(callbackUpdate(userID, data))
	if len(api.searches) != 2 || api.searches[1] != "description of meme 3" {
		t.Fatalf("searches = %q", api.searches)
	}
	if o := api.opts[1]; o.Mode != memesearch.ModeVector || !slices.Equal(o.Tags, []string{"cat"}) {
		t.Fatalf("similar opts = %+v (vector, keeping the search's tags)", o)
	}
	// The meme itself is the best match of its own description; it is not shown again.
	album := mediaOf(t, e.lastSent(t, "sendMediaGroup"))
	for _, m := range album {
		if m["media"] == "prep-3" {
			t.Fatalf("source meme repeated: %v", album)
		}
	}
}

func TestSimilarTruncatesLongDescriptions(t *testing.T) {
	ms := memeRange(1)
	ms[0].Description = strings.Repeat("к", memesearch.MaxQueryLen+50)
	api := &fakeAPI{results: ms}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("cat"))
	e.process(callbackUpdate(userID, similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]))
	if len(api.searches) != 2 || utf8.RuneCountInString(api.searches[1]) != memesearch.MaxQueryLen {
		t.Fatalf("searches = %d, last len %d", len(api.searches), utf8.RuneCountInString(api.searches[len(api.searches)-1]))
	}
}

func TestSimilarRefusesOthersAndBadData(t *testing.T) {
	api := &fakeAPI{results: described(3)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("cat"))
	data := similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]
	e.process(callbackUpdate(userID+1, data))
	e.process(callbackUpdate(userID, "srch:s:nosuchtoken:0"))
	tok := strings.Split(data, ":")[2]
	e.process(callbackUpdate(userID, "srch:s:"+tok+":99"))
	e.process(callbackUpdate(userID, "srch:s:"+tok+":1")) // meme without description
	if api.searchCount() != 1 {
		t.Fatalf("searches = %q", api.searches)
	}
}

// "More" removes only itself: the old page keeps its Similar buttons.
func TestMoreKeepsSimilarButtons(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: described(8)}, &fakeMedia{uploads: true})
	e.process(textUpdate("cat"))
	e.process(callbackUpdate(userID, e.moreData(t)))
	edit := e.lastSent(t, "editMessageReplyMarkup")
	if len(similarButtons(t, edit)) == 0 {
		t.Fatalf("old page lost its similar buttons: %v", buttons(t, edit))
	}
	for _, b := range buttons(t, edit) {
		if strings.HasPrefix(b[1], "srch:m:") {
			t.Fatalf("More still on the old page: %v", b)
		}
	}
}
