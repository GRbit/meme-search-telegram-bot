package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("zero config: %v", err)
	}
	ok := Config{ResultsPerPage: 10, Mode: memesearch.ModeKeyword, PageTTL: time.Minute, InlineResults: memesearch.MaxLimit}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	bad := Config{ResultsPerPage: 11, Mode: "hybrid", PageTTL: -time.Second, InlineResults: memesearch.MaxLimit + 1}
	fields, rest := configerr.Fields(bad.Validate())
	if len(rest) != 0 {
		t.Fatalf("non-field errors: %v", rest)
	}
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"InlineResults", "Mode", "PageTTL", "ResultsPerPage"}) {
		t.Fatalf("fields = %v", names)
	}
	if fields, _ := configerr.Fields((Config{ResultsPerPage: -1}).Validate()); len(fields) != 1 {
		t.Fatal("negative ResultsPerPage accepted")
	}
	if fields, _ := configerr.Fields((Config{InlineResults: -1}).Validate()); len(fields) != 1 {
		t.Fatal("negative InlineResults accepted")
	}
}

func TestNewRequiresDeps(t *testing.T) {
	bundle, err := i18n.NewBundle("en", nil, LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	db := testDB(t)
	full := Deps{API: &fakeAPI{}, Media: &fakeMedia{}, I18n: bundle, DB: db}
	if _, err := New(Config{}, full); err != nil {
		t.Fatalf("full deps: %v", err)
	}
	for name, d := range map[string]Deps{
		"api":   {Media: full.Media, I18n: bundle, DB: db},
		"media": {API: full.API, I18n: bundle, DB: db},
		"i18n":  {API: full.API, Media: full.Media, DB: db},
		"db":    {API: full.API, Media: full.Media, I18n: bundle},
	} {
		if _, err := New(Config{}, d); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
	}
	if _, err := New(Config{}, Deps{API: full.API, I18n: bundle, DB: db}); err == nil || !strings.Contains(err.Error(), "mediacache.NewDirect") {
		t.Fatalf("nil Media error does not point to NewDirect: %v", err)
	}
	if _, err := New(Config{ResultsPerPage: 50}, full); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func TestTextSearchAlbum(t *testing.T) {
	api := &fakeAPI{results: memeRange(7)}
	media := &fakeMedia{uploads: true}
	e := newEnv(t, api, media)
	e.process(textUpdate("  funny cat  "))

	if len(api.searches) != 1 || api.searches[0] != "funny cat" {
		t.Fatalf("searches = %q", api.searches)
	}
	if o := api.opts[0]; o.Limit != memesearch.MaxLimit || o.Mode != memesearch.ModeVector {
		t.Fatalf("opts = %+v", o)
	}
	groups := e.tg.byMethod("sendMediaGroup")
	if len(groups) != 1 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	media0 := mediaOf(t, groups[0])
	if len(media0) != 5 {
		t.Fatalf("album has %d items, want 5", len(media0))
	}
	for i, m := range media0 {
		if want := fmt.Sprintf("prep-%d", i+1); m["media"] != want || m["type"] != "photo" {
			t.Fatalf("item %d = %v, want %s (order kept)", i, m, want)
		}
	}
	ctl := e.tg.byMethod("sendMessage")
	if len(ctl) != 1 || !strings.Contains(ctl[0].fields["text"], "Page 1 of 2") {
		t.Fatalf("control = %+v", ctl)
	}
	if !strings.HasPrefix(e.moreData(t), "srch:m:") {
		t.Fatal("no More button")
	}
	if len(media.remembered) != 0 {
		t.Fatal("Remember called although Resolve already cached")
	}
}

func TestTextSearchCustomConfig(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	e := newEnv(t, api, &fakeMedia{uploads: true}, func(c *Config, _ *Deps) {
		c.ResultsPerPage = 3
		c.Mode = memesearch.ModeKeyword
	})
	e.process(textUpdate("x"))
	if api.opts[0].Mode != memesearch.ModeKeyword {
		t.Fatalf("mode = %q", api.opts[0].Mode)
	}
	if n := len(mediaOf(t, e.tg.byMethod("sendMediaGroup")[0])); n != 3 {
		t.Fatalf("album %d", n)
	}
	if e.moreData(t) != "" {
		t.Fatal("More button on the only page")
	}
	if !strings.Contains(e.tg.byMethod("sendMessage")[0].fields["text"], "Page 1 of 1") {
		t.Fatal("control text")
	}
}

func TestTextSearchSingleResult(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: memeRange(1)}, &fakeMedia{uploads: true})
	e.process(textUpdate("x"))
	if len(e.tg.byMethod("sendMediaGroup")) != 0 {
		t.Fatal("album for one result")
	}
	ph := e.tg.byMethod("sendPhoto")
	if len(ph) != 1 || ph[0].fields["photo"] != "prep-1" || ph[0].fields["chat_id"] != fmt.Sprint(chatID) {
		t.Fatalf("sendPhoto = %+v", ph)
	}
}

func TestTextSearchNoResults(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(textUpdate("nothing"))
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) != 1 || msgs[0].fields["text"] != `Nothing found for "nothing".` {
		t.Fatalf("msgs = %+v", msgs)
	}
	if len(e.tg.byMethod("sendPhoto"))+len(e.tg.byMethod("sendMediaGroup")) != 0 {
		t.Fatal("media sent")
	}
}

func TestTextSearchTooLong(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate(strings.Repeat("\u044f", memesearch.MaxQueryLen+1)))
	if api.searchCount() != 0 {
		t.Fatal("API called")
	}
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0].fields["text"], "200") {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestTextSearchErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"rate limited": {&memesearch.APIError{Status: 429}, "busy"},
		"other":        {errors.New("boom"), "Search failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, &fakeAPI{err: tc.err}, &fakeMedia{uploads: true})
			e.process(textUpdate("x"))
			msgs := e.tg.byMethod("sendMessage")
			if len(msgs) != 1 || !strings.Contains(msgs[0].fields["text"], tc.want) {
				t.Fatalf("msgs = %+v", msgs)
			}
		})
	}
}

func TestTextSearchLocalized(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	u := textUpdate("kot")
	u.Message.From.LanguageCode = "ru"
	e.process(u)
	if got := e.tg.byMethod("sendMessage")[0].fields["text"]; !strings.Contains(got, "\u043d\u0438\u0447\u0435\u0433\u043e") {
		t.Fatalf("text = %q", got)
	}
}

func TestIgnoredMessages(t *testing.T) {
	api := &fakeAPI{results: memeRange(2)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	group := textUpdate("x")
	group.Message.Chat.Type = models.ChatTypeGroup
	edited := &models.Update{EditedMessage: textUpdate("x").Message}
	for _, u := range []*models.Update{group, edited, textUpdate("/unknown"), textUpdate("   ")} {
		e.process(u)
	}
	if api.searchCount() != 0 || len(e.tg.methods()) != 0 {
		t.Fatalf("searches=%d methods=%v", api.searchCount(), e.tg.methods())
	}
}

func TestPagingServesFromMemory(t *testing.T) {
	api := &fakeAPI{results: memeRange(12)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("x"))
	data := e.moreData(t)
	if !strings.HasSuffix(data, ":2") {
		t.Fatalf("data = %q", data)
	}
	e.process(callbackUpdate(userID, data))
	if api.searchCount() != 1 {
		t.Fatalf("paging called the API: %d searches", api.searchCount())
	}
	groups := e.tg.byMethod("sendMediaGroup")
	if len(groups) != 2 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	if first := mediaOf(t, groups[1])[0]["media"]; first != "prep-6" {
		t.Fatalf("page 2 starts with %v", first)
	}
	edits := e.tg.byMethod("editMessageReplyMarkup")
	if len(edits) != 1 || edits[0].fields["message_id"] != "77" || strings.Contains(edits[0].fields["reply_markup"], "srch:") {
		t.Fatalf("edits = %+v", edits)
	}
	if len(e.tg.byMethod("answerCallbackQuery")) != 1 {
		t.Fatal("callback not answered")
	}
	if !strings.Contains(e.tg.byMethod("sendMessage")[1].fields["text"], "Page 2 of 3") {
		t.Fatal("control text")
	}

	// Last page: 2 memes -> album; no More button.
	e.process(callbackUpdate(userID, e.moreData(t)))
	if n := len(mediaOf(t, e.tg.byMethod("sendMediaGroup")[2])); n != 2 {
		t.Fatalf("last page %d items", n)
	}
	if e.moreData(t) != "" {
		t.Fatal("More on last page")
	}
}

func TestPagingForeignUser(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: memeRange(8)}, &fakeMedia{uploads: true})
	e.process(textUpdate("x"))
	e.process(callbackUpdate(userID+1, e.moreData(t)))
	ans := e.tg.byMethod("answerCallbackQuery")
	if len(ans) != 1 || ans[0].fields["text"] == "" {
		t.Fatalf("answers = %+v", ans)
	}
	if len(e.tg.byMethod("sendMediaGroup")) != 1 || len(e.tg.byMethod("editMessageReplyMarkup")) != 0 {
		t.Fatalf("foreign user paged: %v", e.tg.methods())
	}
}

func TestPagingExpired(t *testing.T) {
	e := newEnv(t, &fakeAPI{results: memeRange(8)}, &fakeMedia{uploads: true})
	e.process(textUpdate("x"))
	data := e.moreData(t)
	e.clock.Advance(time.Hour + time.Second)
	e.process(callbackUpdate(userID, data))
	ans := e.tg.byMethod("answerCallbackQuery")
	if len(ans) != 1 || !strings.Contains(ans[0].fields["text"], "expired") {
		t.Fatalf("answers = %+v", ans)
	}
	if len(e.tg.byMethod("sendMediaGroup")) != 1 {
		t.Fatal("expired page sent")
	}

	e.process(callbackUpdate(userID, "srch:m:nosuchtoken:2"))
	e.process(callbackUpdate(userID, "srch:m:garbage"))
	if n := len(e.tg.byMethod("answerCallbackQuery")); n != 3 {
		t.Fatalf("answers = %d, want 3", n)
	}
}

func TestSimilarCallbackAnswered(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(callbackUpdate(userID, "srch:s:abc:1"))
	if len(e.tg.byMethod("answerCallbackQuery")) != 1 {
		t.Fatal("not answered")
	}
}

func TestMixedAlbumUploadsAndRemembers(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	media := &fakeMedia{data: map[int64][]byte{1: []byte("one"), 3: []byte("three")}}
	e := newEnv(t, api, media)
	e.process(textUpdate("x"))

	if len(api.contents) != 0 {
		t.Fatal("search fetched content itself")
	}
	g := e.tg.byMethod("sendMediaGroup")
	if len(g) != 1 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	items := mediaOf(t, g[0])
	if len(items) != 3 || items[1]["media"] != "prep-2" {
		t.Fatalf("items = %v", items)
	}
	for _, i := range []int{0, 2} {
		ref, _ := items[i]["media"].(string)
		name, ok := strings.CutPrefix(ref, "attach://")
		if !ok {
			t.Fatalf("item %d media %q", i, ref)
		}
		want := map[int]string{0: "one", 2: "three"}[i]
		if string(g[0].files[name]) != want {
			t.Fatalf("part %s = %q", name, g[0].files[name])
		}
	}
	slices.SortFunc(media.remembered, func(a, b remembered) int { return int(a.memeID - b.memeID) })
	// The fake numbers album messages in order; memes 1 and 3 are messages 1 and 3.
	// Meme 2 went by file_id, so there is nothing new to remember.
	// The library path travels back so the cache marks that file as uploaded.
	want := []remembered{{1, "fid-1", "uniq-1", "lib/m1.jpg"}, {3, "fid-3", "uniq-3", "lib/m3.jpg"}}
	if !slices.Equal(media.remembered, want) {
		t.Fatalf("remembered = %+v", media.remembered)
	}
}

func TestSinglePhotoFromData(t *testing.T) {
	media := &fakeMedia{data: map[int64][]byte{1: []byte("one")}}
	e := newEnv(t, &fakeAPI{results: memeRange(1)}, media)
	e.process(textUpdate("x"))
	ph := e.tg.byMethod("sendPhoto")
	if len(ph) != 1 || string(ph[0].files["photo"]) != "one" {
		t.Fatalf("sendPhoto = %+v", ph)
	}
	if len(media.remembered) != 1 || media.remembered[0].memeID != 1 {
		t.Fatalf("remembered = %+v", media.remembered)
	}
}

func TestSkipsMemesThatFail(t *testing.T) {
	api := &fakeAPI{results: memeRange(4)}
	logs := &levelCounter{}
	media := &fakeMedia{uploads: true, prepErr: map[int64]error{
		2: errors.New("upload failed"),
		4: fmt.Errorf("%w: meme 4 is image/gif", mediacache.ErrNotPhoto),
	}}
	e := newEnv(t, api, media, withLog(logs))
	e.process(textUpdate("x"))
	items := mediaOf(t, e.tg.byMethod("sendMediaGroup")[0])
	if len(items) != 2 || items[0]["media"] != "prep-1" || items[1]["media"] != "prep-3" {
		t.Fatalf("items = %v", items)
	}
	// Only the real failure warns; a GIF in the results is expected, not a problem.
	if n := logs.warnings(); n != 1 {
		t.Fatalf("warnings = %d, want 1", n)
	}
}

func TestNothingDeliverableSendsError(t *testing.T) {
	media := &fakeMedia{prepErr: map[int64]error{1: errors.New("a"), 2: errors.New("b")}}
	e := newEnv(t, &fakeAPI{results: memeRange(2)}, media)
	e.process(textUpdate("x"))
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0].fields["text"], "Search failed") {
		t.Fatalf("msgs = %+v, methods %v", msgs, e.tg.methods())
	}
}

func TestAlbumRejectedFallsBackToSingles(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	media := &fakeMedia{data: map[int64][]byte{1: []byte("one"), 2: []byte("two"), 3: []byte("three")}}
	e := newEnv(t, api, media)
	e.tg.fail = func(c call) bool {
		if c.method == "sendMediaGroup" {
			return true
		}
		// Reject the single upload of meme 2.
		return c.method == "sendPhoto" && string(c.files["photo"]) == "two"
	}
	e.process(textUpdate("x"))
	photos := e.tg.byMethod("sendPhoto")
	if len(photos) != 3 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	for i, want := range []string{"one", "two", "three"} {
		if got := string(photos[i].files["photo"]); got != want {
			t.Fatalf("photo %d = %q, want %q (bytes re-read after the failed album)", i, got, want)
		}
	}
	ids := make([]int64, 0, len(media.remembered))
	for _, r := range media.remembered {
		ids = append(ids, r.memeID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 3}) {
		t.Fatalf("remembered %v", ids)
	}
	if len(e.tg.byMethod("sendMessage")) != 1 {
		t.Fatal("no control message after fallback")
	}
}

func TestInlineHitsAndMisses(t *testing.T) {
	api := &fakeAPI{results: memeRange(4)}
	media := &fakeMedia{uploads: true, known: map[int64]string{1: "f1", 3: "f3"}}
	e := newEnv(t, api, media, func(c *Config, _ *Deps) { c.InlineResults = 4 })
	e.process(inlineUpdate("  cat "))
	if len(api.searches) != 1 || api.searches[0] != "cat" || api.opts[0].Limit != 4 {
		t.Fatalf("searches %q opts %+v", api.searches, api.opts)
	}
	ans := e.tg.byMethod("answerInlineQuery")
	if len(ans) != 1 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(ans[0].fields["results"]), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0]["photo_file_id"] != "f1" || results[1]["photo_file_id"] != "f3" ||
		results[0]["type"] != "photo" {
		t.Fatalf("results = %v", results)
	}
	if ans[0].fields["is_personal"] != "true" || ans[0].fields["cache_time"] == "" {
		t.Fatalf("fields = %v", ans[0].fields)
	}
	if !slices.Equal(media.enqueued, []int64{2, 4}) {
		t.Fatalf("enqueued = %v", media.enqueued)
	}
	// The user id lets the cache demote this user's older queries' uploads.
	if !slices.Equal(media.enqueuedFor, []int64{userID}) {
		t.Fatalf("enqueued for = %v", media.enqueuedFor)
	}
	if len(media.prepared) != 0 {
		t.Fatal("inline blocked on Prepare")
	}
}

func TestInlineEmptyQuery(t *testing.T) {
	api := &fakeAPI{results: memeRange(4)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(inlineUpdate("   "))
	ans := e.tg.byMethod("answerInlineQuery")
	if api.searchCount() != 0 || len(ans) != 1 || ans[0].fields["results"] != "[]" {
		t.Fatalf("searches=%d answers=%+v", api.searchCount(), ans)
	}
}

func TestInlineDisabled(t *testing.T) {
	api := &fakeAPI{results: memeRange(4)}
	e := newEnv(t, api, &fakeMedia{uploads: false})
	e.process(inlineUpdate("cat"))
	if api.searchCount() != 0 || len(e.tg.methods()) != 0 {
		t.Fatalf("inline handled: %v", e.tg.methods())
	}
}

func TestChargeable(t *testing.T) {
	group := textUpdate("x")
	group.Message.Chat.Type = models.ChatTypeSupergroup
	for _, tc := range []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"text", textUpdate("cat"), true},
		{"blank text", textUpdate("  "), false},
		{"too long text", textUpdate(strings.Repeat("a", memesearch.MaxQueryLen+1)), false},
		{"command", textUpdate("/start"), false},
		{"group text", group, false},
		{"edited", &models.Update{EditedMessage: textUpdate("cat").Message}, false},
		{"inline", inlineUpdate("cat"), true},
		{"empty inline", inlineUpdate(" "), false},
		{"paging", callbackUpdate(userID, "srch:m:tok:2"), false},
		// Not charged until phase 4 implements "similar": a no-op press must not burn quota.
		{"similar (not implemented yet)", callbackUpdate(userID, "srch:s:tok:1"), false},
		{"foreign callback", callbackUpdate(userID, "other:s:1"), false},
		{"empty update", &models.Update{}, false},
	} {
		if got := Chargeable(tc.u); got != tc.want {
			t.Errorf("%s: Chargeable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMiddlewareApplied(t *testing.T) {
	api := &fakeAPI{results: memeRange(8)}
	var seen []string
	block := func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, u *models.Update) {
			switch {
			case u.Message != nil:
				seen = append(seen, "message")
			case u.InlineQuery != nil:
				seen = append(seen, "inline")
			case u.CallbackQuery != nil:
				seen = append(seen, "callback")
			}
		}
	}
	e := newEnv(t, api, &fakeMedia{uploads: true}, func(_ *Config, d *Deps) { d.Middlewares = []bot.Middleware{block} })
	e.process(textUpdate("x"))
	e.process(inlineUpdate("x"))
	e.process(callbackUpdate(userID, "srch:s:x:1"))
	if api.searchCount() != 0 || len(e.tg.methods()) != 0 {
		t.Fatalf("middleware bypassed: %v", e.tg.methods())
	}
	if !slices.Equal(seen, []string{"message", "inline", "callback"}) {
		t.Fatalf("seen = %v", seen)
	}
	// Commands never run a search, so the access middleware must not see them.
	e.process(textUpdate("/help"))
	if len(seen) != 3 || len(e.tg.byMethod("sendMessage")) != 1 {
		t.Fatalf("seen=%v methods=%v", seen, e.tg.methods())
	}
}

func TestStartAndHelp(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(textUpdate("/start"))
	e.process(textUpdate("/start some-payload"))
	e.process(textUpdate("/help"))
	e.process(textUpdate("/help@meme_test_bot"))
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) != 4 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	for i, m := range msgs {
		if !strings.Contains(m.fields["text"], "@meme_test_bot") {
			t.Fatalf("msg %d = %q", i, m.fields["text"])
		}
	}
	if !strings.HasPrefix(msgs[0].fields["text"], "Send me any text") || !strings.HasPrefix(msgs[2].fields["text"], "Send a text") {
		t.Fatalf("texts %q / %q", msgs[0].fields["text"], msgs[2].fields["text"])
	}
	if n := len(e.tg.byMethod("getMe")); n != 1 {
		t.Fatalf("getMe called %d times, want 1 (cached)", n)
	}
}

// Without a cache channel there is no inline mode, so /start and /help must not
// tell users to type @bot in other chats.
func TestStartWithoutInlineDoesNotAdvertiseIt(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: false})
	e.process(textUpdate("/start"))
	e.process(textUpdate("/help"))
	for _, m := range e.tg.byMethod("sendMessage") {
		if strings.Contains(m.fields["text"], "@meme_test_bot") {
			t.Fatalf("text advertises inline mode: %q", m.fields["text"])
		}
	}
}

// A miss means the answer is incomplete: the button tells the user why, and
// cache_time 1 keeps Telegram from replaying the incomplete grid.
func TestInlineMissesShowLoadingButton(t *testing.T) {
	for lang, want := range map[string]string{
		"en": "Memes are loading, try again in a few seconds",
		"ru": "Мемы загружаются, попробуй через пару секунд",
	} {
		media := &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}
		e := newEnv(t, &fakeAPI{results: memeRange(2)}, media)
		u := inlineUpdate("cat")
		u.InlineQuery.From.LanguageCode = lang
		e.process(u)
		ans := e.tg.byMethod("answerInlineQuery")
		if len(ans) != 1 {
			t.Fatalf("%s: methods = %v", lang, e.tg.methods())
		}
		var btn map[string]any
		if err := json.Unmarshal([]byte(ans[0].fields["button"]), &btn); err != nil {
			t.Fatalf("%s: button %q: %v", lang, ans[0].fields["button"], err)
		}
		if btn["text"] != want || btn["start_parameter"] != "loading" {
			t.Fatalf("%s: button = %v", lang, btn)
		}
		if ans[0].fields["cache_time"] != "1" || ans[0].fields["is_personal"] != "true" {
			t.Fatalf("%s: fields = %v", lang, ans[0].fields)
		}
	}
}

func TestInlineAllHitsNoButton(t *testing.T) {
	media := &fakeMedia{uploads: true, known: map[int64]string{1: "f1", 2: "f2"}}
	e := newEnv(t, &fakeAPI{results: memeRange(2)}, media)
	e.process(inlineUpdate("cat"))
	ans := e.tg.byMethod("answerInlineQuery")
	if len(ans) != 1 {
		t.Fatalf("methods = %v", e.tg.methods())
	}
	if _, ok := ans[0].fields["button"]; ok || ans[0].fields["cache_time"] != "30" {
		t.Fatalf("fields = %v", ans[0].fields)
	}
}

// The loading button opens the private chat with /start loading; it must greet
// exactly like a plain /start.
func TestStartLoadingPayloadLikePlainStart(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.process(textUpdate("/start"))
	e.process(textUpdate("/start loading"))
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) != 2 || msgs[0].fields["text"] != msgs[1].fields["text"] {
		t.Fatalf("messages = %+v", msgs)
	}
	if Chargeable(textUpdate("/start loading")) {
		t.Fatal("/start loading is charged")
	}
}

// Fewer inline results means fewer uploads queued per keystroke; the default
// is small on purpose.
func TestInlineResultsDefaultLimit(t *testing.T) {
	api := &fakeAPI{results: memeRange(6)}
	media := &fakeMedia{uploads: true, known: map[int64]string{1: "f1", 5: "f5"}}
	e := newEnv(t, api, media)
	e.process(inlineUpdate("cat"))
	if len(api.opts) != 1 || api.opts[0].Limit != 3 {
		t.Fatalf("opts = %+v", api.opts)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(e.tg.byMethod("answerInlineQuery")[0].fields["results"]), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0]["photo_file_id"] != "f1" {
		t.Fatalf("results = %v", results)
	}
	if !slices.Equal(media.enqueued, []int64{2, 3}) {
		t.Fatalf("enqueued = %v", media.enqueued)
	}
}

func withDebounce(d time.Duration) envOpt {
	return func(c *Config, _ *Deps) { c.InlineDebounce = d }
}

func inlineAnswerIDs(e *env) []string {
	var ids []string
	for _, c := range e.tg.byMethod("answerInlineQuery") {
		ids = append(ids, c.fields["inline_query_id"])
	}
	return ids
}

// "c" at 0s, "ca" at 0.5s: when "c"'s debounce ends at 1s it is no longer the
// user's latest query, so it never reaches the API and is never answered.
func TestInlineDebounceDropsSuperseded(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withDebounce(time.Second))
	var wg sync.WaitGroup
	e.processAsync(&wg, inlineUpdateFrom(userID, "iq1", "c"))
	e.waitTimers(t, 1)
	e.clock.Advance(500 * time.Millisecond)
	e.processAsync(&wg, inlineUpdateFrom(userID, "iq2", "ca"))
	e.waitTimers(t, 2)
	if n := api.searchCount(); n != 0 {
		t.Fatalf("searched %d times before the debounce ended", n)
	}
	e.clock.Advance(500 * time.Millisecond)
	e.clock.Advance(500 * time.Millisecond)
	wg.Wait()
	if !slices.Equal(api.searches, []string{"ca"}) {
		t.Fatalf("searches = %q", api.searches)
	}
	if ids := inlineAnswerIDs(e); !slices.Equal(ids, []string{"iq2"}) {
		t.Fatalf("answered = %v", ids)
	}
}

func TestInlineDebounceOtherUsersIndependent(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withDebounce(time.Second))
	var wg sync.WaitGroup
	e.processAsync(&wg, inlineUpdateFrom(userID, "iq1", "cat"))
	e.processAsync(&wg, inlineUpdateFrom(userID+1, "iq2", "dog"))
	e.waitTimers(t, 2)
	e.clock.Advance(time.Second)
	wg.Wait()
	ids := inlineAnswerIDs(e)
	slices.Sort(ids)
	if api.searchCount() != 2 || !slices.Equal(ids, []string{"iq1", "iq2"}) {
		t.Fatalf("searches = %q, answered = %v", api.searches, ids)
	}
}

// After a query ran, the next one from the same user is not "superseded" by
// stale state: the user's entry is cleaned up when the latest query finishes.
func TestInlineDebounceSequentialQueriesBothRun(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withDebounce(time.Second))
	for i, q := range []string{"cat", "dog"} {
		var wg sync.WaitGroup
		e.processAsync(&wg, inlineUpdateFrom(userID, "iq"+strconv.Itoa(i), q))
		e.waitTimers(t, 1)
		e.clock.Advance(time.Second)
		wg.Wait()
	}
	if !slices.Equal(api.searches, []string{"cat", "dog"}) {
		t.Fatalf("searches = %q", api.searches)
	}
	if n := e.mod.inline.len(); n != 0 {
		t.Fatalf("debounce entries left = %d", n)
	}
}

func TestInlineDebounceBounds(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, 5*time.Second + time.Millisecond} {
		fields, _ := configerr.Fields((Config{InlineDebounce: d}).Validate())
		if len(fields) != 1 || fields[0].Name != "InlineDebounce" {
			t.Fatalf("%v: fields = %v", d, fields)
		}
	}
	if err := (Config{InlineDebounce: 5 * time.Second}).Validate(); err != nil {
		t.Fatalf("5s rejected: %v", err)
	}
}
