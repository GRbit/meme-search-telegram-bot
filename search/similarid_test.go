package search

import (
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// similarEnv runs "cat" and returns the env and the data of the "Similar 1" button.
func similarEnv(t *testing.T, srv *fakeServer, opts ...envOpt) (*env, string) {
	t.Helper()
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, append([]envOpt{withServer(srv)}, opts...)...)
	e.process(textUpdate("cat"))
	return e, similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]
}

func albumIDs(t *testing.T, e *env) []string {
	t.Helper()
	var out []string
	for _, m := range mediaOf(t, e.lastSent(t, "sendMediaGroup")) {
		out = append(out, m["media"].(string))
	}
	return out
}

func TestSimilarByID(t *testing.T) {
	srv := newServer(&fakeAPI{results: described(3)})
	srv.caps.Similar = true
	srv.similar = memes(7, 8)
	e, data := similarEnv(t, srv)
	e.process(callbackUpdate(userID, data))
	if len(srv.similarCalls) != 1 || srv.similarCalls[0] != [2]int64{1, memesearch.MaxLimit} {
		t.Fatalf("similar calls = %v", srv.similarCalls)
	}
	if srv.searchCount() != 1 {
		t.Fatalf("searches = %q, the description must not be searched", srv.searches)
	}
	if got := albumIDs(t, e); len(got) != 2 || got[0] != "prep-7" || got[1] != "prep-8" {
		t.Fatalf("album = %v", got)
	}
	if more := e.moreData(t); more != "" {
		t.Fatalf("two results need no More: %q", more)
	}
}

func TestSimilarByIDFallsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		capOn bool
		err   error
		res   []memesearch.Meme
		calls int
	}{
		"old server":       {capOn: true, err: memesearch.ErrUnsupported, calls: 1},
		"capability off":   {capOn: false, calls: 0},
		"no embedding yet": {capOn: true, res: nil, calls: 1},
		"meme deleted":     {capOn: true, err: memesearch.ErrNotFound, calls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(&fakeAPI{results: described(3)})
			srv.caps.Similar = tc.capOn
			srv.similarErr, srv.similar = tc.err, tc.res
			e, data := similarEnv(t, srv)
			e.process(callbackUpdate(userID, data))
			if len(srv.similarCalls) != tc.calls {
				t.Fatalf("similar calls = %v", srv.similarCalls)
			}
			if srv.searchCount() != 2 || srv.searches[1] != "description of meme 1" {
				t.Fatalf("searches = %q, want the description fallback", srv.searches)
			}
		})
	}
}

// The endpoint has no tag or folder filter, so a filtered session keeps
// searching by description within its scope.
func TestSimilarInFilteredScopeUsesDescription(t *testing.T) {
	srv := newServer(&fakeAPI{results: described(3)}, "a")
	srv.caps.Similar = true
	srv.similar = memes(7)
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("#cat funny"))
	e.process(callbackUpdate(userID, similarButtons(t, e.lastSent(t, "sendMessage"))[0][1]))
	if len(srv.similarCalls) != 0 || srv.searchCount() != 2 {
		t.Fatalf("similar calls = %v searches = %q", srv.similarCalls, srv.searches)
	}
}

func TestSimilarByIDRateLimited(t *testing.T) {
	srv := newServer(&fakeAPI{results: described(3)})
	srv.caps.Similar = true
	srv.similarErr = memesearch.ErrRateLimited
	e, data := similarEnv(t, srv)
	e.process(callbackUpdate(userID, data))
	if srv.searchCount() != 1 {
		t.Fatalf("a rate limit must not be answered with another request: %q", srv.searches)
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; txt != "The meme server is busy, please try again in a minute." {
		t.Fatalf("reply = %q", txt)
	}
}
