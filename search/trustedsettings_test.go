package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const trustedOnlyNote = "trusted users"

// trustedEnv lets the test flip userID between trusted and public.
func trustedEnv(t *testing.T, api *fakeAPI, media *fakeMedia, opts ...envOpt) (*env, *bool) {
	t.Helper()
	trusted := true
	e := newEnv(t, api, media, append(opts, func(_ *Config, d *Deps) {
		d.Trusted = func(user int64) bool { return trusted && user == userID }
	})...)
	return e, &trusted
}

func TestSettingsRefusedForPublicUsers(t *testing.T) {
	srv := newServer(&fakeAPI{}, "a").withTags("cat")
	e, trusted := trustedEnv(t, srv.fakeAPI, &fakeMedia{uploads: true}, withServer(srv))
	*trusted = false
	e.process(textUpdate("/settings"))
	msg := e.lastSent(t, "sendMessage")
	if !strings.Contains(msg.fields["text"], trustedOnlyNote) || len(buttons(t, msg)) != 0 {
		t.Fatalf("/settings for a public user = %q %v", msg.fields["text"], buttons(t, msg))
	}
	for _, data := range []string{"srch:p:mode:keyword", "srch:p:tags", "srch:p:clear", "srch:p:reset",
		"srch:p:match:all", "srch:p:f:p:0", "srch:p:tg:p:0", pickToggleData(tagPrefix, 0, "cat")} {
		e.process(callbackUpdate(userID, data))
		if txt := e.lastSent(t, "answerCallbackQuery").fields["text"]; !strings.Contains(txt, trustedOnlyNote) {
			t.Fatalf("%s: popup = %q", data, txt)
		}
	}
	if p := e.mod.loadPrefs(t.Context(), userID); !p.isZero() {
		t.Fatalf("a public user changed settings: %+v", p)
	}
	if len(e.tg.byMethod("editMessageText")) != 0 {
		t.Fatal("a refused press edited the settings message")
	}
	// "Set tags" was refused, so the next text is a search, not a tag list.
	e.process(textUpdate("cat, dog"))
	if srv.searchCount() != 1 {
		t.Fatalf("searches = %q", srv.searches)
	}
}

// Saved settings are ignored, not deleted, while the user is public.
func TestPublicUserSearchesWithDefaults(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e, trusted := trustedEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	withSavedTags(e, "cat")

	*trusted = false
	e.process(textUpdate("funny"))
	e.process(inlineUpdate("funny"))
	for i, o := range api.opts {
		if o.Mode != memesearch.ModeVector || len(o.Tags) != 0 {
			t.Fatalf("public search %d used saved settings: %+v", i, o)
		}
	}
	if p := e.mod.loadPrefs(t.Context(), userID); p.Mode != memesearch.ModeKeyword || !slices.Equal(p.Tags, []string{"cat"}) {
		t.Fatalf("saved settings lost: %+v", p)
	}

	*trusted = true
	e.process(textUpdate("funny"))
	if o := api.opts[len(api.opts)-1]; o.Mode != memesearch.ModeKeyword || !slices.Equal(o.Tags, []string{"cat"}) {
		t.Fatalf("trusted again, opts = %+v", o)
	}
}
