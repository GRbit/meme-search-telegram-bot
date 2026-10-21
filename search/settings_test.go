package search

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func hasButton(bs [][2]string, data string) bool {
	for _, b := range bs {
		if b[1] == data {
			return true
		}
	}
	return false
}

func TestSettingsShowsDefaults(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(textUpdate("/settings"))
	msg := e.lastSent(t, "sendMessage")
	if txt := msg.fields["text"]; !strings.Contains(txt, "Smart") || !strings.Contains(txt, "Tags: none") {
		t.Fatalf("text = %q", txt)
	}
	bs := buttons(t, msg)
	for _, d := range []string{"srch:p:mode:vector", "srch:p:mode:keyword", "srch:p:tags", "srch:p:reset"} {
		if !hasButton(bs, d) {
			t.Fatalf("no %s button in %v", d, bs)
		}
	}
}

func TestSettingsModeAppliesToSearch(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	edit := e.lastSent(t, "editMessageText")
	if !strings.Contains(edit.fields["text"], "Words") {
		t.Fatalf("edited text = %q", edit.fields["text"])
	}
	e.process(textUpdate("cat"))
	if api.opts[0].Mode != memesearch.ModeKeyword {
		t.Fatalf("mode = %q", api.opts[0].Mode)
	}
	// Another user keeps the default.
	u := textUpdate("dog")
	u.Message.From.ID = userID + 1
	e.process(u)
	if api.opts[1].Mode != memesearch.ModeVector {
		t.Fatalf("other user's mode = %q", api.opts[1].Mode)
	}
}

// Typing tags is a settings change, not a search: it must not reach the API
// or the access middleware that charges searches.
func TestSettingsTagsInputIsNotASearch(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	var charged []string
	count := func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, u *models.Update) {
			if u.Message != nil {
				charged = append(charged, u.Message.Text)
			}
			next(ctx, b, u)
		}
	}
	e := newEnv(t, api, &fakeMedia{uploads: true}, func(_ *Config, d *Deps) { d.Middlewares = []bot.Middleware{count} })
	e.process(callbackUpdate(userID, "srch:p:tags"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "comma") {
		t.Fatalf("prompt = %q", txt)
	}
	e.process(textUpdate(" cat, #dog ,cat,  "))
	if api.searchCount() != 0 || len(charged) != 0 {
		t.Fatalf("tags input searched (%d) or passed the middleware (%q)", api.searchCount(), charged)
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "Tags: cat, dog") {
		t.Fatalf("confirmation = %q", txt)
	}
	e.process(textUpdate("funny"))
	if len(api.opts) != 1 || !slices.Equal(api.opts[0].Tags, []string{"cat", "dog"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	if !slices.Equal(charged, []string{"funny"}) {
		t.Fatalf("charged = %q", charged)
	}
}

func TestSettingsTagsInputRejectsBadTags(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:tags"))
	e.process(textUpdate("1,2,3,4,5,6,7,8,9,10,11"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "10") {
		t.Fatalf("reply = %q", txt)
	}
	e.process(textUpdate(strings.Repeat("x", 21)))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "20") {
		t.Fatalf("reply = %q", txt)
	}
	// Still waiting for tags; /cancel ends it without a change.
	e.process(textUpdate("/cancel"))
	e.process(textUpdate("cat"))
	if api.searchCount() != 1 || len(api.opts[0].Tags) != 0 {
		t.Fatalf("searches = %q opts = %+v", api.searches, api.opts)
	}
}

func TestSettingsClearAndReset(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	e.process(callbackUpdate(userID, "srch:p:tags"))
	e.process(textUpdate("cat"))
	e.process(textUpdate("/settings"))
	if !hasButton(buttons(t, e.lastSent(t, "sendMessage")), "srch:p:clear") {
		t.Fatal("no clear-tags button while tags are set")
	}
	e.process(callbackUpdate(userID, "srch:p:clear"))
	e.process(textUpdate("x"))
	if o := api.opts[0]; o.Mode != memesearch.ModeKeyword || len(o.Tags) != 0 {
		t.Fatalf("after clear: %+v", o)
	}
	e.process(callbackUpdate(userID, "srch:p:reset"))
	e.process(textUpdate("y"))
	if o := api.opts[1]; o.Mode != memesearch.ModeVector || len(o.Tags) != 0 {
		t.Fatalf("after reset: %+v", o)
	}
}

func TestSettingsSurviveRestart(t *testing.T) {
	db := testDB(t)
	withDB := func(_ *Config, d *Deps) { d.DB = db }
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true}, withDB)
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	api := &fakeAPI{results: memeRange(1)}
	e2 := newEnv(t, api, &fakeMedia{uploads: true}, withDB)
	e2.process(textUpdate("cat"))
	if api.opts[0].Mode != memesearch.ModeKeyword {
		t.Fatalf("mode after restart = %q", api.opts[0].Mode)
	}
}

func TestHelpListsSettings(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(textUpdate("/help"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "/settings") {
		t.Fatalf("help = %q", txt)
	}
}
