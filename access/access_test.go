package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// call is one Bot API request the fake server received.
type call struct {
	method string
	params map[string]string
}

// fakeAPI records Bot API calls and answers each with a canned success.
type fakeAPI struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]string{}
	if err := r.ParseMultipartForm(1 << 20); err == nil {
		for k, v := range r.MultipartForm.Value {
			params[k] = v[0]
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{method: method, params: params})
	f.mu.Unlock()
	result := `true`
	if method == "sendMessage" {
		result = `{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":` + result + `}`))
}

func (f *fakeAPI) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// clock is a settable fake time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	a     *Access
	db    *sql.DB
	api   *fakeAPI
	bot   *bot.Bot
	clock *clock
	// passed counts calls that reached the next handler.
	passed atomic.Int64
}

const (
	saverID   = 100
	trustedID = 200
)

func baseConfig() Config {
	return Config{
		SaveUserIDs:        []int64{saverID},
		TrustedUserIDs:     []int64{trustedID},
		PublicDailyPerUser: 2,
		PublicDailyTotal:   3,
		InlineChargeWindow: 10 * time.Second,
		AdminContact:       "@admin",
	}
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newEnvDB(t, cfg, db)
}

func newEnvDB(t *testing.T, cfg Config, db *sql.DB) *env {
	t.Helper()
	bundle, err := i18n.NewBundle("en", nil, LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, api: &fakeAPI{}, clock: &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}}
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	e.bot, err = bot.New("123:TEST", bot.WithServerURL(srv.URL), bot.WithSkipGetMe(), bot.WithNotAsyncHandlers())
	if err != nil {
		t.Fatal(err)
	}
	e.a, err = New(context.Background(), cfg, Deps{DB: db, I18n: bundle, Log: slog.New(slog.DiscardHandler), Now: e.clock.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func everything(*models.Update) bool { return true }

// run passes u through the middleware and reports whether next was called.
func (e *env) run(u *models.Update) bool {
	return e.runWith(everything, u)
}

func (e *env) runWith(chargeable func(*models.Update) bool, u *models.Update) bool {
	var called bool
	h := e.a.Middleware(chargeable)(func(context.Context, *bot.Bot, *models.Update) {
		called = true
		e.passed.Add(1)
	})
	h(context.Background(), e.bot, u)
	return called
}

func msg(userID int64, lang string) *models.Update {
	return &models.Update{Message: &models.Message{
		ID:   7,
		From: &models.User{ID: userID, LanguageCode: lang},
		Chat: models.Chat{ID: userID, Type: models.ChatTypePrivate},
		Text: "cats",
	}}
}

func inline(userID int64) *models.Update {
	return &models.Update{InlineQuery: &models.InlineQuery{ID: "iq1", From: &models.User{ID: userID}, Query: "cats"}}
}

func callback(userID int64) *models.Update {
	return &models.Update{CallbackQuery: &models.CallbackQuery{ID: "cb1", From: models.User{ID: userID}, Data: "srch:sim:1"}}
}

func TestTier(t *testing.T) {
	e := newEnv(t, Config{SaveUserIDs: []int64{1}, TrustedUserIDs: []int64{1, 2}})
	tests := []struct {
		id      int64
		tier    Tier
		canSave bool
	}{
		{1, TierSaver, true},
		{2, TierTrusted, false},
		{3, TierPublic, false},
	}
	for _, tt := range tests {
		if got := e.a.Tier(tt.id); got != tt.tier {
			t.Errorf("Tier(%d) = %v, want %v", tt.id, got, tt.tier)
		}
		if got := e.a.CanSave(tt.id); got != tt.canSave {
			t.Errorf("CanSave(%d) = %v, want %v", tt.id, got, tt.canSave)
		}
	}
}

func TestPrivilegedUsersAreNeverCharged(t *testing.T) {
	e := newEnv(t, baseConfig())
	for range 10 {
		if !e.run(msg(saverID, "en")) || !e.run(msg(trustedID, "en")) {
			t.Fatal("privileged user blocked")
		}
	}
	if n := countRows(t, e.db, "access_usage"); n != 0 {
		t.Errorf("privileged searches wrote %d usage rows", n)
	}
	if len(e.api.all()) != 0 {
		t.Errorf("unexpected API calls: %v", e.api.all())
	}
}

func TestPerUserLimit(t *testing.T) {
	e := newEnv(t, baseConfig())
	for i := range 2 {
		if !e.run(msg(1, "en")) {
			t.Fatalf("search %d should pass", i+1)
		}
	}
	if e.run(msg(1, "en")) {
		t.Fatal("third search should be denied")
	}
	calls := e.api.all()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls = %v, want one sendMessage", calls)
	}
	p := calls[0].params
	if p["chat_id"] != "1" || !strings.Contains(p["text"], "all 2 searches") || !strings.Contains(p["text"], "@admin") {
		t.Errorf("denial params = %v", p)
	}
	if !strings.Contains(p["reply_parameters"], `"message_id":7`) {
		t.Errorf("denial is not a reply: %v", p["reply_parameters"])
	}
	// Another user still has their own allowance (total is 3).
	if !e.run(msg(2, "en")) {
		t.Error("other user blocked by someone else's per-user limit")
	}
}

func TestTotalLimit(t *testing.T) {
	e := newEnv(t, baseConfig())
	for _, id := range []int64{1, 1, 2} {
		if !e.run(msg(id, "en")) {
			t.Fatalf("search by %d should pass", id)
		}
	}
	if e.run(msg(3, "ru")) {
		t.Fatal("total limit not enforced")
	}
	calls := e.api.all()
	want := e.a.i18n.Localizer("ru").T("access.limit_total", map[string]any{"Admin": "@admin"})
	if len(calls) != 1 || calls[0].params["text"] != want || strings.Contains(want, "daily") {
		t.Errorf("want the ru total-limit denial %q, got %v", want, calls)
	}
	// A denied search must not consume the allowance.
	if got := usage(t, e.db, "2026-10-18", 3); got != 0 {
		t.Errorf("denied user count = %d, want 0", got)
	}
}

func TestPublicOff(t *testing.T) {
	for _, cfg := range []Config{{PublicDailyPerUser: 0, PublicDailyTotal: 10}, {PublicDailyPerUser: 10, PublicDailyTotal: 0}} {
		cfg.AdminContact = "@admin"
		e := newEnv(t, cfg)
		if e.run(msg(1, "en")) {
			t.Fatalf("%+v: public search should be off", cfg)
		}
		calls := e.api.all()
		if len(calls) != 1 || !strings.Contains(calls[0].params["text"], "approved users only") {
			t.Errorf("%+v: calls = %v", cfg, calls)
		}
	}
}

func TestDayRolloverInLocation(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Location = tokyo
	e := newEnv(t, cfg)
	// 14:59 UTC is 23:59 in Tokyo.
	e.clock.t = time.Date(2026, 10, 18, 14, 59, 0, 0, time.UTC)
	e.run(msg(1, "en"))
	e.run(msg(1, "en"))
	if e.run(msg(1, "en")) {
		t.Fatal("limit not reached before midnight")
	}
	// One minute later it is still Oct 18 in UTC but a new day in Tokyo.
	e.clock.Add(time.Minute)
	if !e.run(msg(1, "en")) {
		t.Fatal("limit did not reset at Tokyo midnight")
	}
	if got := usage(t, e.db, "2026-10-19", 1); got != 1 {
		t.Errorf("Oct 19 count = %d, want 1", got)
	}
}

func TestPruneOldDays(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.run(msg(1, "en"))
	e.clock.Add(3 * 24 * time.Hour)
	e.run(msg(1, "en"))
	e.clock.Add(7 * 24 * time.Hour)
	e.run(msg(1, "en"))
	if got := usage(t, e.db, "2026-10-18", 1); got != 0 {
		t.Errorf("10-day-old row kept: %d", got)
	}
	if got := usage(t, e.db, "2026-10-21", 1); got != 1 {
		t.Errorf("7-day-old row pruned: %d", got)
	}
	if n := countRows(t, e.db, "access_usage_total"); n != 2 {
		t.Errorf("total rows = %d, want 2", n)
	}
}

func TestInlineWindow(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 3
	cfg.PublicDailyTotal = 10
	e := newEnv(t, cfg)
	for range 5 {
		if !e.run(inline(1)) {
			t.Fatal("inline query inside the window blocked")
		}
		e.clock.Add(time.Second)
	}
	if got := usage(t, e.db, "2026-10-18", 1); got != 1 {
		t.Fatalf("keystrokes inside one window charged %d times, want 1", got)
	}
	// 15s after the first charge: a new window starts and is charged.
	e.clock.Add(10 * time.Second)
	e.run(inline(1))
	if got := usage(t, e.db, "2026-10-18", 1); got != 2 {
		t.Errorf("after the window count = %d, want 2", got)
	}
	// Text searches are never covered by the inline window.
	e.run(msg(1, "en"))
	if got := usage(t, e.db, "2026-10-18", 1); got != 3 {
		t.Errorf("text search count = %d, want 3", got)
	}
	// Inside the window of the last charge the user is now at the limit, but the
	// query they are typing is still the one already paid for.
	if !e.run(inline(1)) {
		t.Error("query inside the paid window blocked")
	}
	// Once the window ends, the next inline query is over the limit.
	e.clock.Add(11 * time.Second)
	if e.run(inline(1)) {
		t.Error("inline query over the limit allowed")
	}
	e.clock.Add(time.Second)
	if e.run(inline(1)) {
		t.Error("denied inline query opened a free window")
	}
}

func TestInlineWindowZeroChargesEveryQuery(t *testing.T) {
	cfg := baseConfig()
	cfg.InlineChargeWindow = 0
	e := newEnv(t, cfg)
	e.run(inline(1))
	e.run(inline(1))
	if got := usage(t, e.db, "2026-10-18", 1); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func TestNonChargeablePassesUntouched(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 0 // public off: a charged update would be denied
	e := newEnv(t, cfg)
	never := func(*models.Update) bool { return false }
	for _, u := range []*models.Update{msg(1, "en"), inline(1), callback(1), {}} {
		if !e.runWith(never, u) {
			t.Errorf("non-chargeable update %+v blocked", u)
		}
	}
	if len(e.api.all()) != 0 || countRows(t, e.db, "access_usage") != 0 {
		t.Error("non-chargeable update touched the API or the counters")
	}
}

func TestNoUserPassesThrough(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 0
	e := newEnv(t, cfg)
	channelPost := &models.Update{Message: &models.Message{ID: 1, Chat: models.Chat{ID: -5}}}
	for _, u := range []*models.Update{channelPost, {}} {
		if !e.run(u) {
			t.Errorf("update without a user blocked: %+v", u)
		}
	}
}

func TestDenialInline(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 0
	e := newEnv(t, cfg)
	if e.run(inline(1)) {
		t.Fatal("want denial")
	}
	calls := e.api.all()
	if len(calls) != 1 || calls[0].method != "answerInlineQuery" {
		t.Fatalf("calls = %v", calls)
	}
	p := calls[0].params
	if p["inline_query_id"] != "iq1" || p["results"] != "[]" || p["is_personal"] != "true" {
		t.Errorf("params = %v", p)
	}
	// cache_time 0 would be dropped by omitempty and Telegram would cache the denial for 300s.
	if p["cache_time"] != "1" {
		t.Errorf("cache_time = %q, want 1", p["cache_time"])
	}
	var btn models.InlineQueryResultsButton
	if err := json.Unmarshal([]byte(p["button"]), &btn); err != nil {
		t.Fatalf("button %q: %v", p["button"], err)
	}
	if btn.StartParameter != "limit" || btn.Text == "" || strings.Contains(btn.Text, "@admin") {
		t.Errorf("button = %+v, want short text and start parameter", btn)
	}
}

func TestDenialCallback(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 0
	e := newEnv(t, cfg)
	if e.run(callback(1)) {
		t.Fatal("want denial")
	}
	calls := e.api.all()
	if len(calls) != 1 || calls[0].method != "answerCallbackQuery" {
		t.Fatalf("calls = %v", calls)
	}
	p := calls[0].params
	if p["callback_query_id"] != "cb1" || p["show_alert"] != "true" || !strings.Contains(p["text"], "@admin") {
		t.Errorf("params = %v", p)
	}
}

// denialText runs the case's updates and returns the one denial text sent.
func denialText(t *testing.T, cfg Config, runs int, lang string) string {
	t.Helper()
	e := newEnv(t, cfg)
	for range runs {
		e.run(msg(1, lang))
	}
	calls := e.api.all()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want one denial", calls)
	}
	return calls[0].params["text"]
}

// TestDenialWithoutAdminContact checks language-independently that without an
// admin the text is the with-admin text minus its trailing admin sentence.
func TestDenialWithoutAdminContact(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*Config)
		runs int
	}{
		{"public off", func(c *Config) { c.PublicDailyPerUser = 0 }, 1},
		{"per user", func(c *Config) { c.PublicDailyPerUser = 1 }, 2},
		{"total", func(c *Config) { c.PublicDailyTotal = 1; c.PublicDailyPerUser = 5 }, 2},
	}
	for _, lang := range []string{"en", "ru"} {
		for _, tc := range cases {
			t.Run(lang+" "+tc.name, func(t *testing.T) {
				cfg := baseConfig()
				tc.cfg(&cfg)
				with := denialText(t, cfg, tc.runs, lang)
				cfg.AdminContact = ""
				without := denialText(t, cfg, tc.runs, lang)
				if !strings.Contains(with, "@admin") || !strings.HasPrefix(with, without+" ") {
					t.Errorf("with admin %q should extend %q by one sentence", with, without)
				}
				if !strings.HasSuffix(without, ".") || strings.Contains(without, "<no value>") || strings.Contains(without, "  ") {
					t.Errorf("text reads badly without admin: %q", without)
				}
			})
		}
	}
}

// TestRussianPluralLimit checks that the limit picks the CLDR plural form:
// 1 and 21 share one form, 2 and 24 another, 5 and 11 a third.
func TestRussianPluralLimit(t *testing.T) {
	form := func(limit int) string {
		cfg := baseConfig()
		cfg.PublicDailyPerUser = limit
		cfg.PublicDailyTotal = 100
		text := denialText(t, cfg, limit+1, "ru")
		if !strings.Contains(text, " "+strconv.Itoa(limit)+" ") {
			t.Errorf("limit %d missing from %q", limit, text)
		}
		return strings.Replace(text, " "+strconv.Itoa(limit)+" ", " N ", 1)
	}
	one, few, many := form(1), form(2), form(5)
	if one == few || few == many || one == many {
		t.Errorf("plural forms not distinct: %q %q %q", one, few, many)
	}
	for limit, want := range map[int]string{21: one, 24: few, 11: many} {
		if got := form(limit); got != want {
			t.Errorf("limit %d: %q, want form %q", limit, got, want)
		}
	}
}

func TestConcurrentChargesNeverExceedLimit(t *testing.T) {
	cfg := baseConfig()
	cfg.PublicDailyPerUser = 5
	cfg.PublicDailyTotal = 100
	e := newEnv(t, cfg)
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() { e.run(msg(1, "en")) })
	}
	wg.Wait()
	if got := e.passed.Load(); got != 5 {
		t.Errorf("allowed %d searches, want exactly 5", got)
	}
	if got := usage(t, e.db, "2026-10-18", 1); got != 5 {
		t.Errorf("count = %d, want 5", got)
	}
	if got := len(e.api.all()); got != 35 {
		t.Errorf("denials = %d, want 35", got)
	}
}

func TestConcurrentInlineKeystrokesChargeOnce(t *testing.T) {
	e := newEnv(t, baseConfig())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { e.run(inline(1)) })
	}
	wg.Wait()
	if got := usage(t, e.db, "2026-10-18", 1); got != 1 {
		t.Errorf("concurrent keystrokes charged %d times, want 1", got)
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("zero config: %v", err)
	}
	if err := baseConfig().Validate(); err != nil {
		t.Errorf("base config: %v", err)
	}
	bad := Config{
		SaveUserIDs:        []int64{1, 0},
		TrustedUserIDs:     []int64{-3},
		PublicDailyPerUser: -1,
		PublicDailyTotal:   -1,
		InlineChargeWindow: -time.Second,
	}
	fields, rest := configerr.Fields(bad.Validate())
	if len(rest) != 0 {
		t.Errorf("non-field errors: %v", rest)
	}
	got := map[string]bool{}
	for _, f := range fields {
		got[f.Name] = true
	}
	for _, name := range []string{"SaveUserIDs", "TrustedUserIDs", "PublicDailyPerUser", "PublicDailyTotal", "InlineChargeWindow"} {
		if !got[name] {
			t.Errorf("no error for %s, got %v", name, fields)
		}
	}
}

func TestNewErrors(t *testing.T) {
	ctx := context.Background()
	bundle, err := i18n.NewBundle("en", nil, LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := New(ctx, baseConfig(), Deps{I18n: bundle}); err == nil {
		t.Error("nil DB: want error")
	}
	if _, err := New(ctx, baseConfig(), Deps{DB: db}); err == nil {
		t.Error("nil I18n: want error")
	}
	var f *configerr.Field
	if _, err := New(ctx, Config{PublicDailyTotal: -1}, Deps{DB: db, I18n: bundle}); !errors.As(err, &f) {
		t.Errorf("invalid config: want *configerr.Field, got %v", err)
	}
	// New must run store.Check before touching tables; a closed handle fails it.
	closed, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if _, err := New(ctx, baseConfig(), Deps{DB: closed, I18n: bundle}); err == nil || !strings.Contains(err.Error(), "check") {
		t.Errorf("closed handle: want store.Check error, got %v", err)
	}
	// Nil Log, Now and Location get defaults; migrations are idempotent.
	for range 2 {
		if _, err := New(ctx, baseConfig(), Deps{DB: db, I18n: bundle}); err != nil {
			t.Errorf("New with defaults: %v", err)
		}
	}
}

func usage(t *testing.T, db *sql.DB, day string, userID int64) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(), `SELECT count FROM access_usage WHERE day = ? AND user_id = ?`, day, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
