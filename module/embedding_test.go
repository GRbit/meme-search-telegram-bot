package module_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/access"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/module"
	"github.com/grbit/meme-search-telegram-bot/saver"
	"github.com/grbit/meme-search-telegram-bot/search"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// TestModulesShareOneBot builds a bot the way a foreign host would: its own
// handler for voice messages (the transcriber) plus search and saver, without
// cmd/bot. Each update must reach exactly the handler it belongs to.
func TestModulesShareOneBot(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	b, err := bot.New("123:abc", bot.WithServerURL(tg.URL), bot.WithSkipGetMe(), bot.WithNotAsyncHandlers())
	if err != nil {
		t.Fatal(err)
	}

	var voices int
	b.RegisterHandlerMatchFunc(func(u *models.Update) bool {
		return u.Message != nil && u.Message.Voice != nil
	}, func(context.Context, *bot.Bot, *models.Update) { voices++ })

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bundle, err := i18n.NewBundle("en", nil, search.LocaleFS(), saver.LocaleFS(), access.LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	const saverID = 7
	acc, err := access.New(ctx, access.Config{SaveUserIDs: []int64{saverID}, PublicDailyPerUser: 5, PublicDailyTotal: 50},
		access.Deps{DB: db, I18n: bundle})
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	srch, err := search.New(search.Config{}, search.Deps{
		API: api, Media: mediacache.NewDirect(api), I18n: bundle,
		Middlewares: []bot.Middleware{acc.Middleware(search.Chargeable)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sv, err := saver.New(saver.Config{Dir: t.TempDir()}, saver.Deps{Permit: acc, I18n: bundle})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []module.Module{srch, sv} {
		m.Register(b)
	}

	user := &models.User{ID: saverID, Username: "alice", LanguageCode: "en"}
	chat := models.Chat{ID: saverID, Type: models.ChatTypePrivate}
	b.ProcessUpdate(ctx, &models.Update{ID: 1, Message: &models.Message{ID: 1, From: user, Chat: chat, Voice: &models.Voice{FileID: "v"}}})
	b.ProcessUpdate(ctx, &models.Update{ID: 2, Message: &models.Message{ID: 2, From: user, Chat: chat, Text: "cat"}})
	b.ProcessUpdate(ctx, &models.Update{ID: 3, Message: &models.Message{ID: 3, From: user, Chat: chat,
		Photo: []models.PhotoSize{{FileID: "p", FileUniqueID: "pu", Width: 10, Height: 10}}}})

	if voices != 1 {
		t.Errorf("foreign voice handler ran %d times, want 1", voices)
	}
	if got := api.queries(); len(got) != 1 || got[0] != "cat" {
		t.Errorf("search queries = %v, want [cat]", got)
	}
	if n := tg.count("getFile"); n != 1 {
		t.Errorf("saver getFile calls = %d, want 1 (photo must reach the saver)", n)
	}
}

type fakeAPI struct {
	mu sync.Mutex
	qs []string
}

func (f *fakeAPI) Search(_ context.Context, q string, _ memesearch.SearchOptions) ([]memesearch.Meme, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qs = append(f.qs, q)
	return nil, nil
}

func (f *fakeAPI) Meme(context.Context, int64) (memesearch.Meme, error) {
	return memesearch.Meme{}, memesearch.ErrNotFound
}

func (f *fakeAPI) Content(context.Context, int64) (memesearch.Content, error) {
	return memesearch.Content{}, memesearch.ErrNotFound
}

func (f *fakeAPI) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.qs...)
}

// fakeTelegram answers every Bot API method with a generic message and counts calls.
type fakeTelegram struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{calls: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := path.Base(r.URL.Path)
		f.mu.Lock()
		f.calls[method]++
		f.mu.Unlock()
		var result any = map[string]any{"message_id": 1, "date": 0, "chat": map[string]any{"id": 1, "type": "private"}}
		switch strings.ToLower(method) {
		case "getme":
			result = map[string]any{"id": 1, "is_bot": true, "first_name": "b", "username": "memebot"}
		case "getfile":
			// Stop the saver here: routing is what this test checks, not saving.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: test"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}
