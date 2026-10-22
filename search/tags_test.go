package search

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

const tagMatchNote = "cannot require all tags"

func TestSettingsTagPickerOnlyWithTagList(t *testing.T) {
	api := &fakeAPI{}
	srv := newServer(api).withTags("cat")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("/settings"))
	bs := buttons(t, e.lastSent(t, "sendMessage"))
	if !hasButton(bs, "srch:p:tg:p:0") || !hasButton(bs, "srch:p:tags") {
		t.Fatalf("buttons = %v", bs)
	}
	srv.caps.TagList = false
	e.process(textUpdate("/settings"))
	bs = buttons(t, e.lastSent(t, "sendMessage"))
	if hasButton(bs, "srch:p:tg:p:0") || !hasButton(bs, "srch:p:tags") {
		t.Fatalf("old server buttons = %v", bs)
	}
}

func TestTagPicker(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api).withTags("t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "zebra")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(callbackUpdate(userID, "srch:p:tg:p:0"))
	view := e.lastSent(t, "editMessageText")
	if strings.Contains(view.fields["reply_markup"], "zebra") || !strings.Contains(view.fields["text"], "tags") {
		t.Fatalf("first page = %q %v", view.fields["text"], buttons(t, view))
	}
	e.process(callbackUpdate(userID, pickerButton(t, view, "t2 (2)")))
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), ">")))
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "zebra")))
	e.process(textUpdate("cat"))
	if !slices.Equal(api.opts[0].Tags, []string{"t2", "zebra"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "No tags")))
	e.process(textUpdate("cat"))
	if len(api.opts[1].Tags) != 0 {
		t.Fatalf("opts = %+v", api.opts)
	}
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "Back")))
	if txt := e.lastSent(t, "editMessageText").fields["text"]; !strings.Contains(txt, "Search settings") {
		t.Fatalf("back = %q", txt)
	}
}

func TestTagPickerLimitAndCache(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	var names []string
	for i := range memesearch.MaxTags + 1 {
		names = append(names, "t"+string(rune('a'+i)))
	}
	srv := newServer(api).withTags(names...)
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	for i := range memesearch.MaxTags + 1 {
		e.process(callbackUpdate(userID, pickToggleData(tagPrefix, i, names[i])))
	}
	e.process(textUpdate("cat"))
	if len(api.opts[0].Tags) != memesearch.MaxTags {
		t.Fatalf("tags = %v", api.opts[0].Tags)
	}
	if srv.tagCalls != 1 {
		t.Fatalf("tag list calls = %d, want 1 (cached)", srv.tagCalls)
	}
	e.clock.Advance(10 * time.Minute)
	e.process(callbackUpdate(userID, "srch:p:tg:p:0"))
	if srv.tagCalls != 2 {
		t.Fatalf("tag list calls after TTL = %d", srv.tagCalls)
	}
}

func TestTagMatchAll(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api).withTags("cat", "dog")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("/settings"))
	if hasButton(buttons(t, e.lastSent(t, "sendMessage")), "srch:p:match:all") {
		t.Fatal("tag match offered without two tags")
	}
	withSavedTags(e, "cat, dog")
	if bs := buttons(t, e.lastSent(t, "sendMessage")); !hasButton(bs, "srch:p:match:all") || !hasButton(bs, "srch:p:match:any") {
		t.Fatalf("buttons = %v", bs)
	}
	e.process(callbackUpdate(userID, "srch:p:match:all"))
	if txt := e.lastSent(t, "editMessageText").fields["text"]; !strings.Contains(txt, "all of") {
		t.Fatalf("settings = %q", txt)
	}
	e.process(textUpdate("funny"))
	if api.opts[0].TagMatch != memesearch.TagMatchAll {
		t.Fatalf("opts = %+v", api.opts)
	}
	ctl := e.lastSent(t, "sendMessage")
	if txt := ctl.fields["text"]; !strings.Contains(txt, "all tags: cat, dog") || strings.Contains(txt, tagMatchNote) {
		t.Fatalf("control = %q", txt)
	}
	// Overriding tags in the query keeps the user's way of combining them.
	e.process(textUpdate("#a #b funny"))
	if api.opts[1].TagMatch != memesearch.TagMatchAll {
		t.Fatalf("opts = %+v", api.opts[1])
	}
	e.process(callbackUpdate(userID, "srch:p:match:any"))
	e.process(textUpdate("funny"))
	if api.opts[2].TagMatch != "" {
		t.Fatalf("any must send nothing: %+v", api.opts[2])
	}
	// Clearing tags forgets the match mode, so "Reset"-equal settings leave no row.
	e.process(callbackUpdate(userID, "srch:p:match:all"))
	e.process(callbackUpdate(userID, "srch:p:clear"))
	if p := e.mod.loadPrefs(t.Context(), userID); !p.isZero() {
		t.Fatalf("prefs after clear = %+v", p)
	}
}

func TestTagMatchIgnoredByOldServer(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api).withTags("cat", "dog")
	h := &levelCounter{}
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv), withLog(h))
	withSavedTags(e, "cat, dog")
	e.process(callbackUpdate(userID, "srch:p:match:all"))
	srv.ignore = []memesearch.Param{memesearch.ParamTagMatch}
	srv.caps.TagMatch = false
	e.process(textUpdate("funny"))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, tagMatchNote) {
		t.Fatalf("control = %q", txt)
	}
	if h.warnings() == 0 {
		t.Fatal("ignored tag_match must be logged at Warn")
	}
	e.process(textUpdate("/settings"))
	if bs := buttons(t, e.lastSent(t, "sendMessage")); hasButton(bs, "srch:p:match:any") {
		t.Fatalf("match buttons offered on an old server: %v", bs)
	}
}

func TestInlineTagMatch(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api).withTags("cat", "dog")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withServer(srv))
	e.tg.admins = []int64{7}
	withSavedTags(e, "cat, dog")
	e.process(callbackUpdate(userID, "srch:p:match:all"))
	e.process(inlineUpdate("funny"))
	if api.opts[0].TagMatch != memesearch.TagMatchAll {
		t.Fatalf("own tags: %+v", api.opts[0])
	}
	e.process(groupCmd("/memescope set #g1 #g2", 7))
	code := scopeCode(t, e)
	e.process(inlineUpdate(code + " funny"))
	if !slices.Equal(api.opts[1].Tags, []string{"g1", "g2"}) || api.opts[1].TagMatch != "" {
		t.Fatalf("a group's tags must match any: %+v", api.opts[1])
	}
}

// A database written before the tag_match column keeps its rows.
func TestPrefsSurviveTagMatchMigration(t *testing.T) {
	db := testDB(t)
	if err := store.Migrate(t.Context(), db, "search", migrations[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO search_prefs (user_id, mode, tags, folders, updated_at)
		VALUES (?, 'keyword', '["cat","dog"]', '[]', 1)`, userID); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true}, func(_ *Config, d *Deps) { d.DB = db })
	e.process(textUpdate("funny"))
	if !slices.Equal(api.opts[0].Tags, []string{"cat", "dog"}) || api.opts[0].Mode != memesearch.ModeKeyword || api.opts[0].TagMatch != "" {
		t.Fatalf("opts = %+v", api.opts[0])
	}
}
