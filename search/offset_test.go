package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// pageTo presses "More" until page n is shown.
func pageTo(t *testing.T, e *env, n int) {
	t.Helper()
	for range n - 1 {
		data := e.moreData(t)
		if data == "" {
			t.Fatalf("no More button; last control = %q", e.lastSent(t, "sendMessage").fields["text"])
		}
		e.process(callbackUpdate(userID, data))
	}
}

func pagingServer(n int) *fakeServer {
	srv := newServer(&fakeAPI{results: memeRange(n)})
	srv.caps.Offset = true
	srv.paging = true
	return srv
}

func TestMoreFetchesNextOffset(t *testing.T) {
	srv := pagingServer(30)
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; txt != "Page 1 of 4+" {
		t.Fatalf("control = %q", txt)
	}
	pageTo(t, e, 4)
	if srv.searchCount() != 1 {
		t.Fatalf("cached pages called the API: %d", srv.searchCount())
	}
	pageTo(t, e, 2) // page 5: the first one past the cached 20
	if srv.searchCount() != 2 || srv.opts[1].Offset != 20 || srv.opts[1].Limit != memesearch.MaxLimit || srv.searches[1] != "cat" {
		t.Fatalf("searches = %q opts = %+v", srv.searches, srv.opts)
	}
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-21", "prep-22", "prep-23", "prep-24", "prep-25"}) {
		t.Fatalf("page 5 album = %v", got)
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; txt != "Page 5 of 6" {
		t.Fatalf("control = %q", txt)
	}
	pageTo(t, e, 2)
	if e.moreData(t) != "" || srv.searchCount() != 2 {
		t.Fatalf("last page: more=%q searches=%d", e.moreData(t), srv.searchCount())
	}
	if Chargeable(callbackUpdate(userID, "srch:m:x:5")) {
		t.Fatal("paging must stay free")
	}
}

// An old server sends no next_offset: paging ends after the 20 fetched memes, as before.
func TestMoreStopsWithoutNextOffset(t *testing.T) {
	srv := newServer(&fakeAPI{results: memeRange(30)}) // ignores offset like an old server
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; txt != "Page 1 of 4" {
		t.Fatalf("control = %q", txt)
	}
	pageTo(t, e, 4)
	if e.moreData(t) != "" || srv.searchCount() != 1 {
		t.Fatalf("more=%q searches=%d", e.moreData(t), srv.searchCount())
	}
}

// A server downgraded between pages ignores offset: its answer is page 1
// again, which must not be shown as page 5.
func TestMoreOffsetIgnored(t *testing.T) {
	srv := pagingServer(30)
	h := &levelCounter{}
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv), withLog(h))
	e.process(textUpdate("cat"))
	pageTo(t, e, 4)
	albums := len(e.tg.byMethod("sendMediaGroup"))
	srv.ignore = []memesearch.Param{memesearch.ParamOffset}
	e.process(callbackUpdate(userID, e.moreData(t)))
	if len(e.tg.byMethod("sendMediaGroup")) != albums {
		t.Fatal("an ignored offset produced an album")
	}
	if txt := e.lastSent(t, "answerCallbackQuery").fields["text"]; !strings.Contains(txt, "No more") {
		t.Fatalf("popup = %q", txt)
	}
	if h.warnings() == 0 {
		t.Fatal("an ignored offset must be logged at Warn")
	}
}

// Results that moved between requests (the library changed) are not shown twice.
func TestMoreDropsDuplicates(t *testing.T) {
	srv := pagingServer(30)
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	pageTo(t, e, 4)
	srv.results = append(memeRange(2), memeRange(30)...) // two memes shift everything by two
	e.process(callbackUpdate(userID, e.moreData(t)))
	if got := albumIDs(t, e); !slices.Equal(got, []string{"prep-21", "prep-22", "prep-23", "prep-24", "prep-25"}) {
		t.Fatalf("page 5 album = %v", got)
	}
}

func TestMoreFetchKeepsScopeAndExclude(t *testing.T) {
	srv := pagingServer(30)
	for i := range srv.results {
		srv.results[i].Description = "d"
	}
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("#cat funny"))
	e.process(callbackUpdate(userID, similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]))
	// The similar session holds memes 2..20 (meme 1 excluded); page 4 is the last cached one.
	pageTo(t, e, 4)
	e.process(callbackUpdate(userID, e.moreData(t)))
	last := srv.opts[len(srv.opts)-1]
	if last.Offset != 20 || last.Mode != memesearch.ModeVector || !slices.Equal(last.Tags, []string{"cat"}) {
		t.Fatalf("fetch opts = %+v", last)
	}
	if got := albumIDs(t, e); slices.Contains(got, "prep-1") {
		t.Fatalf("excluded meme shown: %v", got)
	}
}

func TestMoreFetchError(t *testing.T) {
	srv := pagingServer(30)
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat"))
	pageTo(t, e, 4)
	srv.err = memesearch.ErrRateLimited
	e.process(callbackUpdate(userID, e.moreData(t)))
	if txt := e.lastSent(t, "answerCallbackQuery").fields["text"]; !strings.Contains(txt, "busy") {
		t.Fatalf("popup = %q", txt)
	}
	// The button stays usable for a retry.
	srv.err = nil
	e.process(callbackUpdate(userID, e.moreData(t)))
	if got := albumIDs(t, e); len(got) != 5 || got[0] != "prep-21" {
		t.Fatalf("retry album = %v", got)
	}
}
