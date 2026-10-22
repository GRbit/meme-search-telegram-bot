package mediacache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/internal/fakeclock"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

const chatID = -100123

// sent is one SendPhoto call as the fake saw it.
type sent struct {
	chatID   any
	caption  string
	filename string
	sum      [32]byte
	silent   bool
}

// fakeSender records uploads and answers with sequential file ids. fail, if
// set, decides the error for a call (n counts from 1); gate, if set, holds
// every call until it receives a value or is closed.
type fakeSender struct {
	mu    sync.Mutex
	calls []sent
	fail  func(n int, s sent) error
	gate  chan struct{}
	start chan struct{} // receives once per call, before the gate
}

func (f *fakeSender) SendPhoto(ctx context.Context, p *bot.SendPhotoParams) (*models.Message, error) {
	up, ok := p.Photo.(*models.InputFileUpload)
	if !ok {
		return nil, fmt.Errorf("photo is %T, want upload", p.Photo)
	}
	data, err := io.ReadAll(up.Data)
	if err != nil {
		return nil, err
	}
	s := sent{chatID: p.ChatID, caption: p.Caption, filename: up.Filename, sum: sha256.Sum256(data), silent: p.DisableNotification}
	if f.start != nil {
		f.start <- struct{}{}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, s)
	n := len(f.calls)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(n, s); err != nil {
			return nil, err
		}
	}
	return &models.Message{Photo: []models.PhotoSize{
		{FileID: fmt.Sprintf("small-%d", n), FileUniqueID: fmt.Sprintf("usmall-%d", n), Width: 90, Height: 90},
		{FileID: fmt.Sprintf("fid-%d", n), FileUniqueID: fmt.Sprintf("uniq-%d", n), Width: 800, Height: 600},
		{FileID: fmt.Sprintf("mid-%d", n), FileUniqueID: fmt.Sprintf("umid-%d", n), Width: 320, Height: 240},
	}}, nil
}

func (f *fakeSender) sent() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.calls...)
}

type fakeAPI struct {
	mu       sync.Mutex
	content  map[int64][]byte
	media    map[int64]string // media type override; default image/jpeg
	contentN int
}

func (a *fakeAPI) Search(context.Context, string, memesearch.SearchOptions) ([]memesearch.Meme, error) {
	return nil, errors.New("not used")
}

func (a *fakeAPI) Meme(context.Context, int64) (memesearch.Meme, error) {
	return memesearch.Meme{}, errors.New("not used")
}

func (a *fakeAPI) Content(_ context.Context, id int64) (memesearch.Content, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contentN++
	d, ok := a.content[id]
	if !ok {
		return memesearch.Content{}, memesearch.ErrNotFound
	}
	mt := "image/jpeg"
	if v, ok := a.media[id]; ok {
		mt = v
	}
	return memesearch.Content{Data: d, MediaType: mt}, nil
}

func (a *fakeAPI) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.contentN
}

type env struct {
	t      *testing.T
	c      *Cache
	lib    string
	sender *fakeSender
	api    *fakeAPI
	clk    *fakeclock.Clock
	db     *sql.DB
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	return newEnvWith(t, cfg, nil)
}

// newEnvWith is newEnv with a hook to add optional Deps.
func newEnvWith(t *testing.T, cfg Config, mutate func(*Deps)) *env {
	t.Helper()
	e := &env{t: t, sender: &fakeSender{}, api: &fakeAPI{content: map[int64][]byte{}}}
	e.clk = fakeclock.New(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if cfg.LibraryDir == "" {
		cfg.LibraryDir = t.TempDir()
	}
	e.lib = cfg.LibraryDir
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.db = db
	deps := Deps{DB: db, API: e.api, Sender: e.sender, Log: testLogger(t), Now: e.clk.Now}
	if mutate != nil {
		mutate(&deps)
	}
	c, err := newCache(t.Context(), cfg, deps, e.clk)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	return e
}

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// run serves the upload queue without the scan loop, so warmup uploads do
// not race with the uploads a test asserts on. It stops at test end.
func (e *env) run() context.CancelFunc { return e.start(false) }

// runFull is the real Run, scan loop included.
func (e *env) runFull() context.CancelFunc { return e.start(true) }

func (e *env) start(scanLoop bool) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		if scanLoop {
			done <- e.c.Run(ctx)
		} else {
			done <- e.c.run(ctx, false)
		}
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				e.t.Errorf("Run = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			e.t.Error("Run did not return after cancel")
		}
	}
	e.t.Cleanup(stop)
	return stop
}

// file writes data at rel inside the library with a given mtime offset.
func (e *env) file(rel, data string) string {
	e.t.Helper()
	p := filepath.Join(e.lib, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// scan rescans and drops the warmup jobs it queued, so a test sees only the
// uploads it triggers; warmup has its own tests through runFull.
func (e *env) scan() scanStats {
	e.t.Helper()
	st, err := e.c.scan(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	e.c.queue.DropBackground()
	return st
}

func (e *env) resolve(m memesearch.Meme) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 5*time.Second)
	defer cancel()
	id, err := e.c.Resolve(ctx, m)
	if err != nil {
		e.t.Fatalf("Resolve(%d) = %v", m.ID, err)
	}
	return id
}

func (e *env) libRow(rel string) libRow {
	e.t.Helper()
	r, ok, err := e.c.libraryRow(e.t.Context(), rel)
	if err != nil || !ok {
		e.t.Fatalf("library row %q: ok=%v err=%v", rel, ok, err)
	}
	return r
}

func sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// eventually polls cond for up to 5s; the async paths (Enqueue, warmup) have
// no completion signal a test could wait on.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		fields []string
	}{
		{"zero is valid", Config{}, nil},
		{"explicit", Config{CacheChatID: -1, WarmupInterval: time.Hour, UploadsPerMin: 30, WarmupUploadsPerMin: 30}, nil},
		{"zero warmup with low total", Config{UploadsPerMin: 5}, nil},
		{"negatives", Config{WarmupInterval: -1, UploadsPerMin: -1, WarmupUploadsPerMin: -1, FeedInterval: -1},
			[]string{"WarmupInterval", "UploadsPerMin", "WarmupUploadsPerMin", "FeedInterval"}},
		{"warmup above total", Config{UploadsPerMin: 5, WarmupUploadsPerMin: 6}, []string{"WarmupUploadsPerMin"}},
		{"warmup above default total", Config{WarmupUploadsPerMin: 21}, []string{"WarmupUploadsPerMin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields, rest := configerr.Fields(tc.cfg.Validate())
			if len(rest) != 0 {
				t.Fatalf("non-field errors: %v", rest)
			}
			var names []string
			for _, f := range fields {
				names = append(names, f.Name)
			}
			if fmt.Sprint(names) != fmt.Sprint(tc.fields) {
				t.Fatalf("fields = %v, want %v", names, tc.fields)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c := Config{UploadsPerMin: 5}.withDefaults()
	if c.WarmupInterval != 6*time.Hour || c.UploadsPerMin != 5 || c.WarmupUploadsPerMin != 5 {
		t.Fatalf("defaults = %+v", c)
	}
	c = Config{}.withDefaults()
	if c.UploadsPerMin != 20 || c.WarmupUploadsPerMin != 10 || c.FeedInterval != time.Minute {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = New(t.Context(), Config{UploadsPerMin: -1}, Deps{DB: db, API: &fakeAPI{}, Sender: &fakeSender{}})
	var f *configerr.Field
	if !errors.As(err, &f) || f.Name != "UploadsPerMin" {
		t.Fatalf("New = %v, want a configerr.Field for UploadsPerMin", err)
	}
}

func TestScanIndexesImagesOnly(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/x.jpg", "ax")
	e.file("b/x.JPG", "bx")
	e.file("c/y.png", "cy")
	e.file("c/z.webp", "cz")
	e.file("c/w.jpeg", "cw")
	e.file(".hidden/h.jpg", "h")
	e.file("d/.dot.jpg", "dot")
	e.file("e/anim.gif", "gif")
	e.file("f/readme.txt", "txt")
	st := e.scan()
	if st.files != 5 || st.added != 5 || st.changed != 0 || st.removed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	r := e.libRow("c/y.png")
	if r.baseName != "y.png" || r.sha256 != sum("cy") || r.size != 2 {
		t.Fatalf("row = %+v", r)
	}
	for _, s := range []string{"ax", "bx", "cy", "cz", "cw"} {
		if ok, err := e.c.KnownSHA256(t.Context(), sum(s)); err != nil || !ok {
			t.Fatalf("KnownSHA256(%q) = %v, %v", s, ok, err)
		}
	}
	for _, s := range []string{"h", "dot", "gif", "txt"} {
		if ok, _ := e.c.KnownSHA256(t.Context(), sum(s)); ok {
			t.Fatalf("%q must not be indexed", s)
		}
	}
}

func TestRescanPicksUpChanges(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/keep.jpg", "keep")
	e.file("a/change.jpg", "old")
	e.file("a/gone.jpg", "gone")
	e.scan()
	if err := e.c.setLibraryFileID(t.Context(), "a/change.jpg", "fid-old", "uniq-old"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.setLibraryFileID(t.Context(), "a/keep.jpg", "fid-keep", "uniq-keep"); err != nil {
		t.Fatal(err)
	}
	e.file("a/change.jpg", "new content")
	if err := os.Remove(filepath.Join(e.lib, "a", "gone.jpg")); err != nil {
		t.Fatal(err)
	}
	e.file("b/new.png", "new")
	st := e.scan()
	if st.files != 3 || st.added != 1 || st.changed != 1 || st.removed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if r := e.libRow("a/change.jpg"); r.fileID != "" || r.sha256 != sum("new content") {
		t.Fatalf("changed row = %+v, want new hash and no file_id", r)
	}
	if r := e.libRow("a/keep.jpg"); r.fileID != "fid-keep" {
		t.Fatalf("unchanged row lost its file_id: %+v", r)
	}
	if _, ok, _ := e.c.libraryRow(t.Context(), "a/gone.jpg"); ok {
		t.Fatal("vanished file still indexed")
	}
	if ok, _ := e.c.KnownUniqueID(t.Context(), "uniq-old"); ok {
		t.Fatal("old file_unique_id of a changed file still known")
	}
}

func TestMissingLibraryDirDisablesIndex(t *testing.T) {
	e := newEnv(t, Config{LibraryDir: filepath.Join(t.TempDir(), "nope"), CacheChatID: chatID})
	st := e.scan()
	if st.enabled || st.files != 0 {
		t.Fatalf("stats = %+v, want disabled", st)
	}
}

func TestLookup(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/uniq.jpg", "u")
	e.file("a/dup.jpg", "d1")
	e.file("b/dup.jpg", "d2")
	e.file("a/bare.jpg", "bare")
	e.scan()
	ctx := t.Context()
	for _, rel := range []string{"a/uniq.jpg", "a/dup.jpg", "b/dup.jpg"} {
		if err := e.c.setLibraryFileID(ctx, rel, "fid-"+rel, "uniq-"+rel); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.c.Remember(ctx, 1, "fid-remembered", "uniq-remembered"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		m    memesearch.Meme
		want string
	}{
		{memesearch.Meme{ID: 1, Filename: "dup.jpg"}, "fid-remembered"},
		{memesearch.Meme{ID: 2, Filename: "uniq.jpg"}, "fid-a/uniq.jpg"},
		{memesearch.Meme{ID: 3, Filename: "dup.jpg"}, ""},
		{memesearch.Meme{ID: 4, Filename: "bare.jpg"}, ""},
		{memesearch.Meme{ID: 5, Filename: "missing.jpg"}, ""},
	}
	for _, tc := range cases {
		got, ok := e.c.Lookup(ctx, tc.m)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("Lookup(%d) = %q, %v; want %q", tc.m.ID, got, ok, tc.want)
		}
	}
	// The unique-name hit is remembered by meme id, so a later rename of the
	// library file does not lose it.
	if err := os.Remove(filepath.Join(e.lib, "a", "uniq.jpg")); err != nil {
		t.Fatal(err)
	}
	e.scan()
	if got, ok := e.c.Lookup(ctx, memesearch.Meme{ID: 2, Filename: "uniq.jpg"}); !ok || got != "fid-a/uniq.jpg" {
		t.Fatalf("remembered lookup = %q, %v", got, ok)
	}
	if len(e.sender.sent()) != 0 || e.api.calls() != 0 {
		t.Fatal("Lookup must not touch the network")
	}
}

func TestKnownUniqueID(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/x.jpg", "x")
	e.scan()
	ctx := t.Context()
	if err := e.c.setLibraryFileID(ctx, "a/x.jpg", "f1", "lib-uniq"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Remember(ctx, 9, "f2", "meme-uniq"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"lib-uniq": true, "meme-uniq": true, "other": false, "": false} {
		if got, err := e.c.KnownUniqueID(ctx, id); err != nil || got != want {
			t.Errorf("KnownUniqueID(%q) = %v, %v; want %v", id, got, err, want)
		}
	}
}

func TestResolveRequiresCacheChat(t *testing.T) {
	e := newEnv(t, Config{})
	e.run()
	if _, err := e.c.Resolve(t.Context(), memesearch.Meme{ID: 1}); !errors.Is(err, ErrNoCacheChat) {
		t.Fatalf("Resolve = %v, want ErrNoCacheChat", err)
	}
	if err := e.c.Remember(t.Context(), 1, "f", "u"); err != nil {
		t.Fatal(err)
	}
	if id, err := e.c.Resolve(t.Context(), memesearch.Meme{ID: 1}); err != nil || id != "f" {
		t.Fatalf("Resolve of a remembered meme = %q, %v", id, err)
	}
}

func TestResolveSingleLibraryRowUploadsFile(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("tg-bob/cat.jpg", "cat bytes")
	e.scan()
	e.run()
	id := e.resolve(memesearch.Meme{ID: 5, Filename: "cat.jpg"})
	if id != "fid-1" {
		t.Fatalf("file_id = %q, want the largest size", id)
	}
	s := e.sender.sent()
	if len(s) != 1 || s[0].caption != "tg-bob/cat.jpg" || s[0].sum != sum("cat bytes") || s[0].chatID != int64(chatID) ||
		!s[0].silent || s[0].filename != "cat.jpg" {
		t.Fatalf("sent = %+v", s)
	}
	if e.api.calls() != 0 {
		t.Fatal("a unique library name must not download from the API")
	}
	if r := e.libRow("tg-bob/cat.jpg"); r.fileID != "fid-1" || r.fileUniqueID != "uniq-1" {
		t.Fatalf("library row = %+v", r)
	}
	if got, ok := e.c.Lookup(t.Context(), memesearch.Meme{ID: 5}); !ok || got != "fid-1" {
		t.Fatalf("not remembered by meme id: %q %v", got, ok)
	}
	e.resolve(memesearch.Meme{ID: 5, Filename: "cat.jpg"})
	if len(e.sender.sent()) != 1 {
		t.Fatal("second Resolve uploaded again")
	}
}

func TestResolveNameCollisionPicksRowByHash(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	e.api.content[7] = []byte("second")
	e.run()
	if id := e.resolve(memesearch.Meme{ID: 7, Filename: "x.jpg"}); id != "fid-1" {
		t.Fatalf("file_id = %q", id)
	}
	if s := e.sender.sent(); len(s) != 1 || s[0].caption != "b/x.jpg" || s[0].sum != sum("second") {
		t.Fatalf("sent = %+v", s)
	}
	if r := e.libRow("b/x.jpg"); r.fileID != "fid-1" {
		t.Fatalf("matching row = %+v", r)
	}
	if r := e.libRow("a/x.jpg"); r.fileID != "" {
		t.Fatalf("other row got a file_id: %+v", r)
	}
}

func TestResolveNameCollisionMatchedRowAlreadyUploaded(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	if err := e.c.setLibraryFileID(t.Context(), "a/x.jpg", "fid-a", "uniq-a"); err != nil {
		t.Fatal(err)
	}
	e.api.content[7] = []byte("first")
	e.run()
	if id := e.resolve(memesearch.Meme{ID: 7, Filename: "x.jpg"}); id != "fid-a" {
		t.Fatalf("file_id = %q, want fid-a", id)
	}
	if len(e.sender.sent()) != 0 {
		t.Fatal("uploaded although the matching row had a file_id")
	}
	if got, ok := e.c.Lookup(t.Context(), memesearch.Meme{ID: 7, Filename: "x.jpg"}); !ok || got != "fid-a" {
		t.Fatal("hash match not remembered by meme id")
	}
}

func TestResolveNameCollisionWithoutHashMatchUploadsAPIBytes(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	e.api.content[7] = []byte("third")
	e.run()
	e.resolve(memesearch.Meme{ID: 7, Filename: "x.jpg"})
	if s := e.sender.sent(); len(s) != 1 || s[0].caption != "meme 7" || s[0].sum != sum("third") {
		t.Fatalf("sent = %+v", s)
	}
	if e.libRow("a/x.jpg").fileID != "" || e.libRow("b/x.jpg").fileID != "" {
		t.Fatal("a library row got a foreign file_id")
	}
}

func TestResolveWithoutLibraryRowUploadsAPIBytes(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[7] = []byte("api bytes")
	e.run()
	if id := e.resolve(memesearch.Meme{ID: 7, Filename: "elsewhere.jpg"}); id != "fid-1" {
		t.Fatalf("file_id = %q", id)
	}
	if s := e.sender.sent(); len(s) != 1 || s[0].caption != "meme 7" || s[0].sum != sum("api bytes") ||
		s[0].filename != "elsewhere.jpg" {
		t.Fatalf("sent = %+v", s)
	}
	if ok, _ := e.c.KnownUniqueID(t.Context(), "uniq-1"); !ok {
		t.Fatal("uploaded meme's file_unique_id not known")
	}
}

func TestResolveVanishedLibraryFileFallsBackToAPI(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	p := e.file("a/x.jpg", "local")
	e.scan()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	e.api.content[7] = []byte("remote")
	e.run()
	e.resolve(memesearch.Meme{ID: 7, Filename: "x.jpg"})
	if s := e.sender.sent(); len(s) != 1 || s[0].sum != sum("remote") {
		t.Fatalf("sent = %+v", s)
	}
}

func badRequest() error {
	return fmt.Errorf("%w, Bad Request: PHOTO_INVALID_DIMENSIONS", bot.ErrorBadRequest)
}

func TestRejectedFileIsRecordedAndNotRetriedUntilChanged(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/huge.jpg", "huge")
	e.scan()
	e.sender.fail = func(n int, _ sent) error {
		if n == 1 {
			return badRequest()
		}
		return nil
	}
	e.run()
	m := memesearch.Meme{ID: 3, Filename: "huge.jpg"}
	if _, err := e.c.Resolve(t.Context(), m); !errors.Is(err, ErrRejected) {
		t.Fatalf("Resolve = %v, want ErrRejected", err)
	}
	r := e.libRow("a/huge.jpg")
	if !r.failed || r.failReason == "" {
		t.Fatalf("failure not recorded: %+v", r)
	}
	if _, err := e.c.Resolve(t.Context(), m); !errors.Is(err, ErrRejected) {
		t.Fatalf("second Resolve = %v, want ErrRejected", err)
	}
	if n := len(e.sender.sent()); n != 1 {
		t.Fatalf("rejected file uploaded %d times", n)
	}
	if st := e.scan(); st.pending != 0 {
		t.Fatalf("failed file queued for warmup: %+v", st)
	}
	e.file("a/huge.jpg", "smaller now!")
	e.scan()
	if id := e.resolve(m); id != "fid-2" {
		t.Fatalf("after change Resolve = %q", id)
	}
}

func TestRejectedAPIUploadIsNotRetriedSoon(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[3] = []byte("bad")
	e.sender.fail = func(int, sent) error { return badRequest() }
	e.run()
	m := memesearch.Meme{ID: 3, Filename: "bad.jpg"}
	for range 2 {
		if _, err := e.c.Resolve(t.Context(), m); !errors.Is(err, ErrRejected) {
			t.Fatalf("Resolve = %v, want ErrRejected", err)
		}
	}
	if n := len(e.sender.sent()); n != 1 {
		t.Fatalf("uploaded %d times", n)
	}
	e.clk.Advance(memeRetryAfter)
	e.sender.fail = nil
	if id := e.resolve(m); id == "" {
		t.Fatal("no retry after memeRetryAfter")
	}
}

func TestChatErrorIsNotAFileFailure(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/x.jpg", "x")
	e.scan()
	e.sender.fail = func(int, sent) error { return fmt.Errorf("%w, Bad Request: chat not found", bot.ErrorBadRequest) }
	e.run()
	if _, err := e.c.Resolve(t.Context(), memesearch.Meme{ID: 1, Filename: "x.jpg"}); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("Resolve = %v, want a non-rejection error", err)
	}
	if r := e.libRow("a/x.jpg"); r.failed {
		t.Fatal("chat error recorded as file failure")
	}
}

func TestConcurrentResolveSharesOneUpload(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[4] = []byte("shared")
	e.sender.gate = make(chan struct{})
	e.sender.start = make(chan struct{}, 10)
	e.run()
	m := memesearch.Meme{ID: 4, Filename: "s.jpg"}
	const n = 5
	ids := make(chan string, n)
	for range n {
		go func() {
			id, err := e.c.Resolve(t.Context(), m)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}()
	}
	<-e.sender.start
	e.c.Enqueue(m, m)
	time.Sleep(5 * time.Millisecond) // let the other callers join before releasing
	close(e.sender.gate)
	for range n {
		if id := <-ids; id != "fid-1" {
			t.Fatalf("id = %q", id)
		}
	}
	if got := len(e.sender.sent()); got != 1 {
		t.Fatalf("uploads = %d, want 1", got)
	}
	if e.api.calls() != 1 {
		t.Fatalf("API content calls = %d, want 1", e.api.calls())
	}
}

func TestEnqueueUploadsInBackground(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("one")
	e.api.content[2] = []byte("two")
	e.run()
	e.c.Enqueue(memesearch.Meme{ID: 1}, memesearch.Meme{ID: 2})
	eventually(t, "both enqueued memes cached", func() bool {
		_, ok1 := e.c.Lookup(t.Context(), memesearch.Meme{ID: 1})
		_, ok2 := e.c.Lookup(t.Context(), memesearch.Meme{ID: 2})
		return ok1 && ok2
	})
}

func TestEnqueueIsBounded(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.c.maxEnqueued = 1
	for i := int64(1); i <= 3; i++ {
		e.api.content[i] = []byte{byte(i)}
	}
	e.sender.gate = make(chan struct{})
	e.sender.start = make(chan struct{}, 10)
	e.run()
	e.c.Enqueue(memesearch.Meme{ID: 1})
	<-e.sender.start
	e.c.Enqueue(memesearch.Meme{ID: 2}, memesearch.Meme{ID: 3})
	close(e.sender.gate)
	eventually(t, "meme 1 cached", func() bool { _, ok := e.c.Lookup(t.Context(), memesearch.Meme{ID: 1}); return ok })
	time.Sleep(5 * time.Millisecond)
	if n := len(e.sender.sent()); n != 1 {
		t.Fatalf("uploads = %d, want 1: the rest must be dropped while the bound is reached", n)
	}
}

func TestEnqueueWithoutCacheChatIsNoop(t *testing.T) {
	e := newEnv(t, Config{})
	e.api.content[1] = []byte("x")
	e.run()
	e.c.Enqueue(memesearch.Meme{ID: 1})
	time.Sleep(5 * time.Millisecond)
	if len(e.sender.sent()) != 0 || e.api.calls() != 0 {
		t.Fatal("Enqueue without a cache chat did work")
	}
}

func TestWarmupUploadsLibrary(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/1.jpg", "1")
	e.file("a/2.jpg", "2")
	e.file("a/3.jpg", "3")
	e.runFull()
	eventually(t, "three warmup uploads", func() bool { return len(e.sender.sent()) == 3 })
	for _, rel := range []string{"a/1.jpg", "a/2.jpg", "a/3.jpg"} {
		if e.libRow(rel).fileID == "" {
			t.Fatalf("%s has no file_id after warmup", rel)
		}
	}
	if got, ok := e.c.Lookup(t.Context(), memesearch.Meme{ID: 1, Filename: "2.jpg"}); !ok || got == "" {
		t.Fatal("warmed file not found by name")
	}
}

func TestRescanLoopRunsEveryInterval(t *testing.T) {
	e := newEnv(t, Config{WarmupInterval: time.Hour})
	e.runFull()
	eventually(t, "first scan", func() bool { return e.c.scans.Load() == 1 })
	e.file("a/later.jpg", "later")
	if !e.clk.WaitForWaiters(1, 5*time.Second) {
		t.Fatal("scan loop not sleeping")
	}
	e.clk.Advance(time.Hour)
	eventually(t, "second scan", func() bool { return e.c.scans.Load() == 2 })
	if ok, _ := e.c.KnownSHA256(t.Context(), sum("later")); !ok {
		t.Fatal("rescan did not index the new file")
	}
}

func TestRealtimeGoesAheadOfWarmup(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 10, WarmupUploadsPerMin: 1})
	e.file("a/1.jpg", "1")
	e.file("a/2.jpg", "2")
	e.file("a/3.jpg", "3")
	e.api.content[9] = []byte("urgent")
	e.runFull()
	eventually(t, "first warmup upload", func() bool { return len(e.sender.sent()) == 1 })
	// The warmup cap now holds the other two files; a realtime upload must not wait.
	e.resolve(memesearch.Meme{ID: 9})
	s := e.sender.sent()
	if len(s) != 2 || s[1].caption != "meme 9" {
		t.Fatalf("sent = %+v", s)
	}
	e.clk.Advance(time.Minute)
	eventually(t, "second warmup upload", func() bool { return len(e.sender.sent()) == 3 })
}

func TestTooManyRequestsPausesAndRetries(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("x")
	e.sender.fail = func(n int, _ sent) error {
		if n == 1 {
			return &bot.TooManyRequestsError{Message: "too many requests", RetryAfter: 7}
		}
		return nil
	}
	e.run()
	res := make(chan error, 1)
	go func() { _, err := e.c.Resolve(t.Context(), memesearch.Meme{ID: 1}); res <- err }()
	eventually(t, "first attempt", func() bool { return len(e.sender.sent()) == 1 })
	if !e.clk.WaitForWaiters(1, 5*time.Second) {
		t.Fatal("queue not paused")
	}
	e.clk.Advance(6 * time.Second)
	time.Sleep(5 * time.Millisecond)
	if n := len(e.sender.sent()); n != 1 {
		t.Fatalf("retried before retry_after: %d calls", n)
	}
	e.clk.Advance(time.Second)
	select {
	case err := <-res:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve did not finish after retry_after")
	}
	if n := len(e.sender.sent()); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestRunCancelUnblocksResolve(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 1, WarmupUploadsPerMin: 1})
	e.api.content[1] = []byte("1")
	e.api.content[2] = []byte("2")
	stop := e.runFull()
	e.resolve(memesearch.Meme{ID: 1})
	res := make(chan error, 1)
	go func() { _, err := e.c.Resolve(context.Background(), memesearch.Meme{ID: 2}); res <- err }()
	// Waiters: rescan timer and the rate-limit wait for meme 2.
	if !e.clk.WaitForWaiters(2, 5*time.Second) {
		t.Fatal("second upload not waiting for the rate limit")
	}
	stop()
	select {
	case err := <-res:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("Resolve = %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve still blocked after Run ended")
	}
	if _, err := e.c.Resolve(context.Background(), memesearch.Meme{ID: 3}); !errors.Is(err, ErrStopped) {
		t.Fatalf("Resolve after Run = %v, want ErrStopped", err)
	}
}

func TestResolveBeforeRunWaitsForWorker(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("1")
	res := make(chan string, 1)
	go func() { res <- e.resolve(memesearch.Meme{ID: 1}) }()
	eventually(t, "upload queued", func() bool { return e.c.queue.Len(0) == 1 })
	e.run()
	select {
	case id := <-res:
		if id != "fid-1" {
			t.Fatal(id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve did not finish once Run started")
	}
}

func TestResolveCallerCtxCancel(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.api.content[1] = []byte("1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.c.Resolve(ctx, memesearch.Meme{ID: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve = %v", err)
	}
}

func TestAddLibraryFile(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.run()
	outside := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(outside, []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddLibraryFile(t.Context(), outside); err == nil {
		t.Fatal("file outside LibraryDir accepted")
	}
	if err := e.c.AddLibraryFile(t.Context(), filepath.Join(e.lib, "..", "sneaky.jpg")); err == nil {
		t.Fatal("path escaping LibraryDir accepted")
	}
	p := e.file("tg-bob/2026-10-18_bob_1.jpg", "saved")
	if err := e.c.AddLibraryFile(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.c.KnownSHA256(t.Context(), sum("saved")); !ok {
		t.Fatal("added file not indexed")
	}
	eventually(t, "added file uploaded", func() bool { return len(e.sender.sent()) == 1 })
	if s := e.sender.sent(); s[0].caption != "tg-bob/2026-10-18_bob_1.jpg" {
		t.Fatalf("sent = %+v", s)
	}
}

func TestAddLibraryFileWithoutLibraryDir(t *testing.T) {
	e := newEnv(t, Config{LibraryDir: filepath.Join(t.TempDir(), "missing")})
	if err := e.c.AddLibraryFile(t.Context(), filepath.Join(e.lib, "a.jpg")); err == nil {
		t.Fatal("want an error when the file does not exist")
	}
}
