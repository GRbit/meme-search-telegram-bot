package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/internal/fakeclock"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// call is one recorded Bot API request.
type call struct {
	method string
	fields map[string]string
	files  map[string][]byte // multipart file parts by part name
}

// fakeTG is a minimal Bot API server: it records calls and answers with canned JSON.
type fakeTG struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	calls []call
	seq   int
	// fail decides whether a call is answered with a 400 error.
	fail func(c call) bool
	// admins answer getChatAdministrators; the first one is the owner.
	admins []int64
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	c := call{method: parts[len(parts)-1], fields: map[string]string{}, files: map[string][]byte{}}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			f.t.Errorf("parse form: %v", err)
		}
		for k, v := range r.MultipartForm.Value {
			c.fields[k] = v[0]
		}
		for k, fhs := range r.MultipartForm.File {
			fh, _ := fhs[0].Open()
			data, _ := io.ReadAll(fh)
			_ = fh.Close()
			c.files[k] = data
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fail := f.fail != nil && f.fail(c)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if fail {
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: rejected by fake"}`)
		return
	}
	result := f.result(c)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTG) nextID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}

func (f *fakeTG) photoMessage() map[string]any {
	n := f.nextID()
	return map[string]any{
		"message_id": n, "date": 1, "chat": map[string]any{"id": 1, "type": "private"},
		"photo": []map[string]any{
			{"file_id": fmt.Sprintf("small-%d", n), "file_unique_id": fmt.Sprintf("usmall-%d", n), "width": 90, "height": 90},
			{"file_id": fmt.Sprintf("fid-%d", n), "file_unique_id": fmt.Sprintf("uniq-%d", n), "width": 800, "height": 600},
		},
	}
}

func (f *fakeTG) result(c call) any {
	switch c.method {
	case "getMe":
		return map[string]any{"id": 999, "is_bot": true, "first_name": "Memes", "username": "meme_test_bot"}
	case "sendMediaGroup":
		var media []json.RawMessage
		_ = json.Unmarshal([]byte(c.fields["media"]), &media)
		out := make([]any, len(media))
		for i := range media {
			out[i] = f.photoMessage()
		}
		return out
	case "sendPhoto":
		return f.photoMessage()
	case "getChatAdministrators":
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for i, id := range f.admins {
			status := "administrator"
			if i == 0 {
				status = "creator"
			}
			out = append(out, map[string]any{"status": status, "user": map[string]any{"id": id, "is_bot": false, "first_name": "a"}})
		}
		return out
	case "sendMessage", "editMessageReplyMarkup", "editMessageText":
		return map[string]any{"message_id": f.nextID(), "date": 1, "chat": map[string]any{"id": 1, "type": "private"}}
	default:
		return true
	}
}

func (f *fakeTG) byMethod(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTG) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.method
	}
	return out
}

// fakeAPI implements memesearch.Client.
type fakeAPI struct {
	mu       sync.Mutex
	results  []memesearch.Meme
	err      error
	searches []string
	opts     []memesearch.SearchOptions
	contents []int64 // search must never fetch content itself
}

func (a *fakeAPI) Search(_ context.Context, q string, o memesearch.SearchOptions) ([]memesearch.Meme, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.searches = append(a.searches, q)
	a.opts = append(a.opts, o)
	if a.err != nil {
		return nil, a.err
	}
	// Like the real API, never more than asked for.
	if o.Limit > 0 && len(a.results) > o.Limit {
		return a.results[:o.Limit], nil
	}
	return a.results, nil
}

func (a *fakeAPI) Meme(_ context.Context, id int64) (memesearch.Meme, error) {
	return memesearch.Meme{}, memesearch.ErrNotFound
}

func (a *fakeAPI) Content(_ context.Context, id int64) (memesearch.Content, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contents = append(a.contents, id)
	return memesearch.Content{}, memesearch.ErrNotFound
}

func (a *fakeAPI) searchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.searches)
}

type remembered struct {
	memeID         int64
	fileID, unique string
	libPath        string
}

// fakeMedia implements MediaCache. Prepare returns FileID "prep-<id>" unless
// data or prepErr has an entry for the meme.
type fakeMedia struct {
	mu         sync.Mutex
	uploads    bool
	known      map[int64]string // Lookup hits
	data       map[int64][]byte // Prepare returns Data
	prepErr    map[int64]error
	prepared   []int64
	enqueued   []int64
	remembered []remembered
	// enqueuedFor is the user of each EnqueueFor call.
	enqueuedFor []int64
}

func (m *fakeMedia) UploadsEnabled() bool { return m.uploads }

func (m *fakeMedia) Lookup(_ context.Context, meme memesearch.Meme) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.known[meme.ID]
	return id, ok
}

func (m *fakeMedia) Prepare(_ context.Context, meme memesearch.Meme) (mediacache.Photo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepared = append(m.prepared, meme.ID)
	if err := m.prepErr[meme.ID]; err != nil {
		return mediacache.Photo{}, err
	}
	if d, ok := m.data[meme.ID]; ok {
		return mediacache.Photo{Name: meme.Filename, Data: d, LibraryPath: "lib/" + meme.Filename}, nil
	}
	return mediacache.Photo{FileID: fmt.Sprintf("prep-%d", meme.ID), Name: meme.Filename}, nil
}

func (m *fakeMedia) EnqueueFor(user int64, ms ...memesearch.Meme) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueuedFor = append(m.enqueuedFor, user)
	for _, x := range ms {
		m.enqueued = append(m.enqueued, x.ID)
	}
}

func (m *fakeMedia) RememberPhoto(_ context.Context, memeID int64, p mediacache.Photo, fileID, unique string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remembered = append(m.remembered, remembered{memeID, fileID, unique, p.LibraryPath})
	return nil
}

func memes(ids ...int64) []memesearch.Meme {
	out := make([]memesearch.Meme, len(ids))
	for i, id := range ids {
		out[i] = memesearch.Meme{
			ID: id, Filename: fmt.Sprintf("m%d.jpg", id), MediaType: "image/jpeg",
			ContentURL: fmt.Sprintf("/api/v1/memes/%d/content", id),
		}
	}
	return out
}

func memeRange(n int) []memesearch.Meme {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return memes(ids...)
}

// levelCounter is a slog.Handler counting records at Warn and above.
type levelCounter struct {
	mu   sync.Mutex
	warn int
}

func (h *levelCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.warn++
		h.mu.Unlock()
	}
	return nil
}
func (h *levelCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelCounter) WithGroup(string) slog.Handler      { return h }

func (h *levelCounter) warnings() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warn
}

func withLog(h slog.Handler) envOpt {
	return func(_ *Config, d *Deps) { d.Log = slog.New(h) }
}

type env struct {
	tg    *fakeTG
	api   *fakeAPI
	media *fakeMedia
	clock *fakeclock.Clock
	mod   *Module
	b     *bot.Bot
}

type envOpt func(*Config, *Deps)

func newEnv(t *testing.T, api *fakeAPI, media *fakeMedia, opts ...envOpt) *env {
	t.Helper()
	bundle, err := i18n.NewBundle("en", nil, LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{tg: newFakeTG(t), api: api, media: media, clock: fakeclock.New(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))}
	cfg := Config{}
	deps := Deps{
		API: api, Media: media, I18n: bundle, Now: e.clock.Now, DB: testDB(t),
		Log: slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(&cfg, &deps)
	}
	e.mod, err = New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	e.mod.after = e.clock.After
	e.b, err = bot.New("123:secret", bot.WithServerURL(e.tg.srv.URL), bot.WithSkipGetMe(), bot.WithNotAsyncHandlers(),
		bot.WithDefaultHandler(func(context.Context, *bot.Bot, *models.Update) {}))
	if err != nil {
		t.Fatal(err)
	}
	e.mod.Register(e.b)
	return e
}

const (
	userID = 42
	chatID = 42
)

func textUpdate(text string) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: 10, Text: text,
		From: &models.User{ID: userID, LanguageCode: "en", Username: "ann"},
		Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate},
	}}
}

func callbackUpdate(from int64, data string) *models.Update {
	return &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb1", Data: data, From: models.User{ID: from, LanguageCode: "en"},
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: 77, Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate}},
		},
	}}
}

func inlineUpdate(q string) *models.Update {
	return inlineUpdateFrom(userID, "iq1", q)
}

func inlineUpdateFrom(user int64, id, q string) *models.Update {
	return &models.Update{InlineQuery: &models.InlineQuery{
		ID: id, Query: q, From: &models.User{ID: user, LanguageCode: "en"},
	}}
}

// processAsync runs an update in its own goroutine, as the real bot does, so a
// debounced handler can sleep on the fake clock while the test advances it.
func (e *env) processAsync(wg *sync.WaitGroup, u *models.Update) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.process(u)
	}()
}

// waitTimers blocks until n handlers sleep on the fake clock; advancing the
// clock earlier would fire nothing and leave the handlers waiting forever.
func (e *env) waitTimers(t *testing.T, n int) {
	t.Helper()
	if !e.clock.WaitForWaiters(n, 5*time.Second) {
		t.Fatalf("waiters = %d, want %d", e.clock.Waiters(), n)
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// buttons returns text -> callback data of the keyboard in c, in order.
func buttons(t *testing.T, c call) [][2]string {
	t.Helper()
	raw := c.fields["reply_markup"]
	if raw == "" {
		return nil
	}
	var kb models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &kb); err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			out = append(out, [2]string{btn.Text, btn.CallbackData})
		}
	}
	return out
}

func (e *env) lastSent(t *testing.T, method string) call {
	t.Helper()
	cs := e.tg.byMethod(method)
	if len(cs) == 0 {
		t.Fatalf("no %s; methods = %v", method, e.tg.methods())
	}
	return cs[len(cs)-1]
}

func (e *env) process(u *models.Update) {
	e.b.ProcessUpdate(context.Background(), u)
}

// moreData returns the callback data of the "More" button in the last control message, or "".
func (e *env) moreData(t *testing.T) string {
	t.Helper()
	msgs := e.tg.byMethod("sendMessage")
	if len(msgs) == 0 {
		t.Fatal("no control message")
	}
	raw := msgs[len(msgs)-1].fields["reply_markup"]
	if raw == "" {
		return ""
	}
	var kb models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &kb); err != nil {
		t.Fatal(err)
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, "srch:m:") {
				return btn.CallbackData
			}
		}
	}
	return ""
}

func mediaOf(t *testing.T, c call) []map[string]any {
	t.Helper()
	var media []map[string]any
	if err := json.Unmarshal([]byte(c.fields["media"]), &media); err != nil {
		t.Fatalf("media field: %v (%q)", err, c.fields["media"])
	}
	return media
}

// fakeServer adds the newer endpoints to fakeAPI. Searches through SearchPage
// are recorded in the embedded fakeAPI like plain searches.
type fakeServer struct {
	*fakeAPI
	caps       memesearch.Capabilities
	folders    []memesearch.Folder
	foldersErr error
	// ignore is reported as Page.Ignored on every SearchPage, like an older server.
	ignore      []memesearch.Param
	folderCalls int
	tags        []memesearch.Tag
	tagCalls    int
	// similar answers Similar; similarErr fails it. similarCalls records id and limit.
	similar      []memesearch.Meme
	similarErr   error
	similarCalls [][2]int64
}

func newServer(api *fakeAPI, folders ...string) *fakeServer {
	s := &fakeServer{fakeAPI: api, caps: memesearch.Capabilities{FolderList: true, FolderFilter: true}}
	for i, f := range folders {
		s.folders = append(s.folders, memesearch.Folder{Name: f, MemeCount: i + 1})
	}
	return s
}

func (s *fakeServer) Capabilities() memesearch.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

func (s *fakeServer) SearchPage(ctx context.Context, q string, o memesearch.SearchOptions) (memesearch.Page, error) {
	found, err := s.Search(ctx, q, o)
	if err != nil {
		return memesearch.Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return memesearch.Page{Memes: found, Ignored: s.ignore}, nil
}

func (s *fakeServer) Folders(context.Context) ([]memesearch.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.folderCalls++
	return s.folders, s.foldersErr
}

func (s *fakeServer) Tags(context.Context) ([]memesearch.Tag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tagCalls++
	if !s.caps.TagList {
		return nil, memesearch.ErrUnsupported
	}
	return s.tags, nil
}

// withTags turns on the tag list and tag_match and serves names with counts 1, 2, ...
func (s *fakeServer) withTags(names ...string) *fakeServer {
	s.caps.TagList, s.caps.TagMatch = true, true
	for i, n := range names {
		s.tags = append(s.tags, memesearch.Tag{Name: n, MemeCount: i + 1})
	}
	return s
}

func (s *fakeServer) Similar(_ context.Context, id int64, limit int) ([]memesearch.Meme, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.similarCalls = append(s.similarCalls, [2]int64{id, int64(limit)})
	if s.similarErr != nil {
		return nil, s.similarErr
	}
	return s.similar, nil
}

func withServer(s *fakeServer) envOpt {
	return func(_ *Config, d *Deps) { d.Server = s }
}
