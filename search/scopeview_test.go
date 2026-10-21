package search

import (
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func everywhereData(t *testing.T, c call) string {
	t.Helper()
	for _, b := range buttons(t, c) {
		if strings.HasPrefix(b[1], "srch:e:") {
			return b[1]
		}
	}
	return ""
}

func withSavedTags(e *env, tags string) {
	e.process(callbackUpdate(userID, "srch:p:tags"))
	e.process(textUpdate(tags))
}

func TestScopeShownWithSearchEverywhere(t *testing.T) {
	api := &fakeAPI{results: memeRange(8)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	withSavedTags(e, "cat")
	e.process(textUpdate("funny"))
	ctl := e.lastSent(t, "sendMessage")
	if txt := ctl.fields["text"]; !strings.Contains(txt, "tags: cat") || !strings.Contains(txt, "Page 1 of 2") {
		t.Fatalf("control = %q", txt)
	}
	data := everywhereData(t, ctl)
	if data == "" || e.moreData(t) == "" {
		t.Fatalf("buttons = %v", buttons(t, ctl))
	}
	if !Chargeable(callbackUpdate(userID, data)) {
		t.Fatal("search everywhere is a new search and must be charged")
	}
	e.process(callbackUpdate(userID, data))
	if len(api.opts) != 2 || api.searches[1] != "funny" || len(api.opts[1].Tags) != 0 {
		t.Fatalf("rerun: searches = %q opts = %+v", api.searches, api.opts)
	}
	if everywhereData(t, e.lastSent(t, "sendMessage")) != "" {
		t.Fatal("unfiltered rerun offers search everywhere again")
	}
}

func TestDefaultScopeShowsNothing(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: memeRange(1)}, &fakeMedia{uploads: true})
	e.process(textUpdate("funny"))
	ctl := e.lastSent(t, "sendMessage")
	if txt := ctl.fields["text"]; txt != "Page 1 of 1" || everywhereData(t, ctl) != "" {
		t.Fatalf("control = %q %v", txt, buttons(t, ctl))
	}
}

// Mode is not a filter: it is shown, but there is nothing to drop.
func TestModeOnlyScope(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	e.process(textUpdate("funny"))
	ctl := e.lastSent(t, "sendMessage")
	if txt := ctl.fields["text"]; !strings.Contains(txt, "Words") || everywhereData(t, ctl) != "" {
		t.Fatalf("control = %q %v", txt, buttons(t, ctl))
	}
}

func TestEmptyFilteredResultSaysSo(t *testing.T) {
	api := &fakeAPI{}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("#cat funny"))
	msg := e.lastSent(t, "sendMessage")
	if txt := msg.fields["text"]; !strings.Contains(txt, "tags: cat") {
		t.Fatalf("reply = %q", txt)
	}
	data := everywhereData(t, msg)
	if data == "" {
		t.Fatal("no search everywhere button")
	}
	api.results = memeRange(1)
	e.process(callbackUpdate(userID, data))
	if len(api.opts) != 2 || len(api.opts[1].Tags) != 0 || api.opts[1].Mode != memesearch.ModeVector {
		t.Fatalf("opts = %+v", api.opts)
	}
}

func TestSearchEverywhereRefusesOthersAndExpired(t *testing.T) {
	api := &fakeAPI{}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("#cat funny"))
	data := everywhereData(t, e.lastSent(t, "sendMessage"))
	e.process(callbackUpdate(userID+1, data))
	if api.searchCount() != 1 {
		t.Fatal("another user reran the search")
	}
	e.process(callbackUpdate(userID, "srch:e:unknowntoken"))
	if api.searchCount() != 1 {
		t.Fatal("expired session reran")
	}
	if txt := e.lastSent(t, "answerCallbackQuery").fields["text"]; !strings.Contains(txt, "expired") {
		t.Fatalf("answer = %q", txt)
	}
}
