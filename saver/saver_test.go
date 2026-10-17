package saver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
)

const testToken = "123:SECRETTOKEN"

// fakeAPI serves getFile, file downloads and sendMessage, recording what it saw.
type fakeAPI struct {
	srv *httptest.Server

	mu        sync.Mutex
	files     map[string][]byte // file_id -> bytes
	paths     map[string]string // file_id -> file_path override
	downloads int
	sent      []string
	sentCh    chan string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{files: map[string][]byte{}, paths: map[string]string{}, sentCh: make(chan string, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rest, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+testToken+"/"); ok {
		f.downloads++
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "files/"), filepath.Ext(rest))
		data, ok := f.files[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
		return
	}
	p := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(1 << 20)
		for k, v := range r.MultipartForm.Value {
			p[k] = v[0]
		}
	} else {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/getFile"):
		id, _ := p["file_id"].(string)
		fp := f.paths[id]
		if fp == "" {
			fp = "files/" + id + ".jpg"
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"` + id + `","file_path":"` + fp + `"}}`))
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		text, _ := p["text"].(string)
		f.sent = append(f.sent, text)
		f.sentCh <- text
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) reply(t *testing.T) string {
	t.Helper()
	select {
	case s := <-f.sentCh:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no reply sent")
		return ""
	}
}

func (f *fakeAPI) noMoreReplies(t *testing.T) {
	t.Helper()
	select {
	case s := <-f.sentCh:
		t.Fatalf("unexpected extra reply %q", s)
	case <-time.After(50 * time.Millisecond):
	}
}

type fakePermit map[int64]bool

func (p fakePermit) CanSave(id int64) bool { return p[id] }

type fakeKnown struct {
	uniq  map[string]bool
	sums  map[[32]byte]bool
	added []string
}

func (k *fakeKnown) KnownUniqueID(_ context.Context, id string) (bool, error) { return k.uniq[id], nil }
func (k *fakeKnown) KnownSHA256(_ context.Context, s [32]byte) (bool, error)  { return k.sums[s], nil }
func (k *fakeKnown) AddLibraryFile(_ context.Context, p string) error {
	k.added = append(k.added, p)
	return nil
}

type env struct {
	api   *fakeAPI
	mod   *Module
	bot   *bot.Bot
	dir   string
	known *fakeKnown
	after []func() // captured album timers
}

func newEnv(t *testing.T, mutate func(*Config)) *env {
	t.Helper()
	api := newFakeAPI(t)
	dir := filepath.Join(t.TempDir(), "memes")
	bundle, err := i18n.NewBundle("en", nil, LocaleFS())
	if err != nil {
		t.Fatal(err)
	}
	known := &fakeKnown{uniq: map[string]bool{}, sums: map[[32]byte]bool{}}
	cfg := Config{Dir: dir, DirPrefix: "tg-"}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := New(cfg, Deps{Permit: fakePermit{1: true}, Known: known, I18n: bundle,
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{api: api, mod: m, dir: dir, known: known}
	m.albums.after = func(_ time.Duration, f func()) { e.after = append(e.after, f) }
	b, err := bot.New(testToken, bot.WithServerURL(api.srv.URL), bot.WithSkipGetMe(), bot.WithNotAsyncHandlers())
	if err != nil {
		t.Fatal(err)
	}
	m.Register(b)
	e.bot = b
	return e
}

func (e *env) send(msg models.Message) {
	e.bot.ProcessUpdate(context.Background(), &models.Update{ID: 1, Message: &msg})
}

// fireTimers runs captured album timers in order; stale ones must be no-ops.
func (e *env) fireTimers() {
	for _, f := range e.after {
		f()
	}
	e.after = nil
}

func (e *env) ls(t *testing.T, rel string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	return names
}

func photoMsg(id int, fileID string) models.Message {
	return models.Message{
		ID: id, Date: int(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Unix()),
		Chat: models.Chat{ID: 1, Type: models.ChatTypePrivate},
		From: &models.User{ID: 1, Username: "alice"},
		Photo: []models.PhotoSize{
			{FileID: fileID + "-small", FileUniqueID: "u-small", Width: 10, Height: 10},
			{FileID: fileID, FileUniqueID: "u-" + fileID, Width: 100, Height: 100},
		},
	}
}

func TestMatch(t *testing.T) {
	e := newEnv(t, nil)
	base := photoMsg(1, "p")
	doc := func(mime string) models.Message {
		m := base
		m.Photo = nil
		m.Document = &models.Document{FileID: "d", FileUniqueID: "ud", MimeType: mime}
		return m
	}
	noPerm := base
	noPerm.From = &models.User{ID: 2}
	group := base
	group.Chat.Type = models.ChatTypeGroup
	sticker := base
	sticker.Photo = nil
	sticker.Sticker = &models.Sticker{FileID: "s"}
	for name, tc := range map[string]struct {
		msg  models.Message
		want bool
	}{
		"photo": {base, true}, "png document": {doc("image/png"), true}, "gif document": {doc("image/gif"), false},
		"pdf document": {doc("application/pdf"), false}, "no permission": {noPerm, false},
		"group chat": {group, false}, "sticker": {sticker, false},
	} {
		msg := tc.msg
		if got := e.mod.match(&models.Update{Message: &msg}); got != tc.want {
			t.Errorf("%s: match = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSavePhoto(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["p"] = []byte("big-photo-bytes")
	e.send(photoMsg(77, "p"))
	if got := e.api.reply(t); got != "Saved." {
		t.Fatalf("reply = %q", got)
	}
	want := filepath.Join(e.dir, "tg-alice", "2026-10-18_alice_77.jpg")
	data, err := os.ReadFile(want)
	if err != nil || string(data) != "big-photo-bytes" {
		t.Fatalf("file: %v %q", err, data)
	}
	if st, _ := os.Stat(want); st.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v", st.Mode().Perm())
	}
	if len(e.known.added) != 1 || e.known.added[0] != want {
		t.Errorf("AddLibraryFile calls = %v", e.known.added)
	}
	if names := e.ls(t, "tg-alice"); len(names) != 1 {
		t.Errorf("temp files left behind: %v", names)
	}
}

func TestSaveImageDocumentExtensionFromTelegramPath(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["d"] = []byte("png-bytes")
	e.api.paths["d"] = "files/d.png"
	m := photoMsg(5, "x")
	m.Photo = nil
	m.Document = &models.Document{FileID: "d", FileUniqueID: "ud", MimeType: "image/png", FileName: "../evil.exe"}
	e.send(m)
	e.api.reply(t)
	if _, err := os.Stat(filepath.Join(e.dir, "tg-alice", "2026-10-18_alice_5.png")); err != nil {
		t.Fatal(err)
	}
}

func TestSenderWithoutUsernameAndForwardOrigin(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["p"] = []byte("x")
	m := photoMsg(9, "p")
	m.From = &models.User{ID: 1}
	m.ForwardOrigin = &models.MessageOrigin{MessageOriginChannel: &models.MessageOriginChannel{Chat: models.Chat{Title: "Funny Memes"}}}
	e.send(m)
	e.api.reply(t)
	if _, err := os.Stat(filepath.Join(e.dir, "tg-id1", "2026-10-18_Funny_Memes_9.jpg")); err != nil {
		t.Fatal(err)
	}
}

func TestDateUsesConfigLocation(t *testing.T) {
	loc := time.FixedZone("plus3", 3*3600)
	e := newEnv(t, func(c *Config) { c.Location = loc })
	e.api.files["p"] = []byte("x")
	m := photoMsg(1, "p")
	m.Date = int(time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC).Unix()) // 01:00 next day in +3
	e.send(m)
	e.api.reply(t)
	if _, err := os.Stat(filepath.Join(e.dir, "tg-alice", "2026-10-19_alice_1.jpg")); err != nil {
		t.Fatal(err)
	}
}

func TestKnownUniqueIDSkipsDownload(t *testing.T) {
	e := newEnv(t, nil)
	e.known.uniq["u-p"] = true
	e.send(photoMsg(1, "p"))
	if got := e.api.reply(t); got != "This meme is already in the collection." {
		t.Fatalf("reply = %q", got)
	}
	if e.api.downloads != 0 {
		t.Errorf("downloads = %d, want 0", e.api.downloads)
	}
}

func TestKnownSHASkipsWrite(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["p"] = []byte("known-bytes")
	e.known.sums[sha256.Sum256([]byte("known-bytes"))] = true
	e.send(photoMsg(1, "p"))
	if got := e.api.reply(t); got != "This meme is already in the collection." {
		t.Fatalf("reply = %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "tg-alice")); err == nil {
		t.Error("sender folder must not be created")
	}
}

func TestFolderDuplicate(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["a"] = []byte("same")
	e.api.files["b"] = []byte("same")
	e.send(photoMsg(1, "a"))
	e.api.reply(t)
	e.send(photoMsg(2, "b"))
	if got := e.api.reply(t); got != "Already saved." {
		t.Fatalf("reply = %q", got)
	}
	if names := e.ls(t, "tg-alice"); len(names) != 1 {
		t.Errorf("files = %v", names)
	}
}

func TestFolderDuplicateSeesPreexistingFiles(t *testing.T) {
	e := newEnv(t, nil)
	folder := filepath.Join(e.dir, "tg-alice")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "old.png"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.api.files["a"] = []byte("old")
	e.send(photoMsg(1, "a"))
	if got := e.api.reply(t); got != "Already saved." {
		t.Fatalf("reply = %q", got)
	}
}

func TestNameCollisionGetsSuffix(t *testing.T) {
	e := newEnv(t, nil)
	e.api.files["a"] = []byte("one")
	e.api.files["b"] = []byte("two")
	e.send(photoMsg(1, "a"))
	e.api.reply(t)
	e.send(photoMsg(1, "b")) // same message id, different bytes
	e.api.reply(t)
	names := e.ls(t, "tg-alice")
	if len(names) != 2 || names[1] != "2026-10-18_alice_1_2.jpg" {
		t.Errorf("files = %v", names)
	}
}

func TestNoteAppended(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Note = "Indexing takes a while." })
	e.api.files["p"] = []byte("x")
	e.send(photoMsg(1, "p"))
	if got := e.api.reply(t); got != "Saved.\nIndexing takes a while." {
		t.Fatalf("reply = %q", got)
	}
}

func TestRussianReply(t *testing.T) {
	e := newEnv(t, nil)
	bundle, _ := i18n.NewBundle("en", nil, LocaleFS())
	e.mod.deps.I18n = bundle
	e.api.files["p"] = []byte("x")
	m := photoMsg(1, "p")
	m.From.LanguageCode = "ru"
	e.send(m)
	if got := e.api.reply(t); got != "\u0421\u043e\u0445\u0440\u0430\u043d\u0435\u043d\u043e." {
		t.Fatalf("reply = %q", got)
	}
}

func TestDownloadFailureReplyError(t *testing.T) {
	e := newEnv(t, nil) // file bytes missing -> 404
	e.send(photoMsg(1, "missing"))
	if got := e.api.reply(t); got != "Could not save, please try again later." {
		t.Fatalf("reply = %q", got)
	}
}

func TestDownloadErrorDoesNotLeakToken(t *testing.T) {
	var buf bytes.Buffer
	e := newEnv(t, nil)
	e.mod.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// A failing transport makes http.Client return a *url.Error that embeds the link.
	e.mod.httpc = &http.Client{Transport: failingTransport{}}
	e.api.files["p"] = []byte("x")
	e.send(photoMsg(1, "p"))
	if !strings.Contains(buf.String(), "download failed") {
		t.Fatalf("download path not exercised: %s", buf.String())
	}
	if strings.Contains(buf.String(), "SECRETTOKEN") {
		t.Fatalf("token leaked into logs: %s", buf.String())
	}
}

func TestAlbumOneReply(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Note = "Note." })
	e.api.files["a"] = []byte("A")
	e.api.files["b"] = []byte("B")
	e.api.files["c"] = []byte("A") // duplicate of a
	e.api.files["d"] = []byte("D")
	e.known.sums[sha256.Sum256([]byte("D"))] = true
	for i, id := range []string{"a", "b", "c", "d"} {
		m := photoMsg(10+i, id)
		m.MediaGroupID = "grp"
		e.send(m)
	}
	e.api.noMoreReplies(t)
	e.fireTimers()
	want := "Saved 2 of 4.\nNote.\nAlready in the collection: 1.\nAlready saved earlier: 1."
	if got := e.api.reply(t); got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	e.api.noMoreReplies(t)
	if len(e.mod.albums.items) != 0 {
		t.Error("album state not cleaned up")
	}
}

func TestAlbumRealTimer(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AlbumDebounce = 20 * time.Millisecond })
	e.mod.albums = newAlbums(20*time.Millisecond, e.mod.flushAlbum)
	e.api.files["a"] = []byte("A")
	e.api.files["b"] = []byte("B")
	for i, id := range []string{"a", "b"} {
		m := photoMsg(i+1, id)
		m.MediaGroupID = "g"
		e.send(m)
	}
	if got := e.api.reply(t); got != "Saved 2 of 2." {
		t.Fatalf("reply = %q", got)
	}
	e.api.noMoreReplies(t)
}

func TestConfigValidateAndNew(t *testing.T) {
	err := Config{AlbumDebounce: -1}.Validate()
	fields, rest := configerr.Fields(err)
	if len(fields) != 2 || len(rest) != 0 || fields[0].Name != "Dir" || fields[1].Name != "AlbumDebounce" {
		t.Fatalf("fields = %+v rest = %v", fields, rest)
	}
	if err := (Config{Dir: "x"}).Validate(); err != nil {
		t.Fatalf("zero values must validate: %v", err)
	}
	bundle, _ := i18n.NewBundle("en", nil, fstest.MapFS{"en.yaml": {Data: []byte("a: b")}})
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := New(Config{Dir: dir}, Deps{I18n: bundle}); err == nil {
		t.Error("nil Permit must fail")
	}
	if _, err := New(Config{Dir: dir}, Deps{Permit: fakePermit{}}); err == nil {
		t.Error("nil I18n must fail")
	}
	m, err := New(Config{Dir: dir}, Deps{Permit: fakePermit{}, I18n: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Error("Dir not created")
	}
	if m.cfg.AlbumDebounce != 2*time.Second || m.cfg.Location != time.UTC {
		t.Errorf("defaults not applied: %+v", m.cfg)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("boom")
}
