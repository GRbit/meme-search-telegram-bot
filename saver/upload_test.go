package saver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// uploadServer fakes POST /api/v2/memes. answer picks the response shape:
// "created" (default), "duplicate", "file_error", "old" (a stock server's
// HTML 404), "invalid_folder" or "broken" (500).
type uploadServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	answer  string
	uploads []upload
}

type upload struct {
	folder, generate, name string
	data                   []byte
}

func newUploadServer(t *testing.T) *uploadServer {
	t.Helper()
	us := &uploadServer{}
	us.srv = httptest.NewServer(http.HandlerFunc(us.serve))
	t.Cleanup(us.srv.Close)
	return us
}

func (us *uploadServer) serve(w http.ResponseWriter, r *http.Request) {
	us.mu.Lock()
	defer us.mu.Unlock()
	if r.URL.Path != "/api/v2/memes" || us.answer == "old" {
		us.uploads = append(us.uploads, upload{}) // counted: the request was made
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>Not Found</html>"))
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fh := r.MultipartForm.File["files[]"][0]
	f, _ := fh.Open()
	data, _ := io.ReadAll(f)
	_ = f.Close()
	up := upload{folder: r.FormValue("folder"), generate: r.FormValue("generate_description"), name: fh.Filename, data: data}
	us.uploads = append(us.uploads, up)

	w.Header().Set("Content-Type", "application/json")
	errBody := func(status int, code string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"x"}}`))
	}
	meme := map[string]any{"id": 42, "filename": up.name, "description": nil, "tags": []string{},
		"media_type": "image/jpeg", "content_url": "/api/v1/memes/42/content", "folder": up.folder,
		"description_status": "pending"}
	data2 := map[string]any{"created": []any{}, "duplicates": []any{}, "errors": []any{}}
	switch us.answer {
	case "invalid_folder":
		errBody(http.StatusUnprocessableEntity, "invalid_folder")
		return
	case "broken":
		errBody(http.StatusInternalServerError, "internal_error")
		return
	case "duplicate":
		data2["duplicates"] = []any{map[string]any{"filename": up.name, "meme": meme}}
	case "file_error":
		data2["errors"] = []any{map[string]any{"filename": up.name,
			"error": map[string]any{"code": "invalid_image", "message": "Invalid image file."}}}
	default:
		data2["created"] = []any{map[string]any{"filename": up.name, "description_queued": true, "meme": meme}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data2,
		"meta": map[string]any{"folder": up.folder, "generate_description": up.generate == "true"}})
}

func (us *uploadServer) got() []upload {
	us.mu.Lock()
	defer us.mu.Unlock()
	return append([]upload(nil), us.uploads...)
}

func (us *uploadServer) client(t *testing.T) *memesearch.HTTPClient {
	t.Helper()
	// UploadToken goes away when the client sends the API token for uploads too;
	// then drop it here, nothing else in the saver reads it.
	c, err := memesearch.New(memesearch.Config{URL: us.srv.URL, Token: "read", UploadToken: "write"}, us.srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// apiEnv is an env with SAVE_BACKEND=api against us; caps is what the probe
// last saw.
func apiEnv(t *testing.T, us *uploadServer, caps memesearch.Capabilities, mut func(*Config)) *env {
	t.Helper()
	return newEnvWith(t, func(c *Config) {
		c.Backend = BackendAPI
		if mut != nil {
			mut(c)
		}
	}, func(d *Deps) {
		d.Uploader = us.client(t)
		d.Caps = memesearch.StaticCapabilities(caps)
	})
}

var canUpload = memesearch.Capabilities{Upload: true}

func (e *env) exists(t *testing.T, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(e.dir, rel))
	return err == nil
}

func TestAPIBackendUploadsIntoTheSaveFolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mut    func(*Config)
		folder string
	}{
		{"per_user", nil, "tg-alice"},
		{"single", func(c *Config) { c.Mode = ModeSingle }, "telegram"},
		{"nested", func(c *Config) { c.Mode = ModeNested }, "telegram/alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			us := newUploadServer(t)
			e := apiEnv(t, us, canUpload, tc.mut)
			e.api.files["p"] = []byte("meme")
			e.send(photoMsg(5, "p"))
			if got := e.api.reply(t); got != "Saved." {
				t.Fatalf("reply = %q", got)
			}
			ups := us.got()
			if len(ups) != 1 {
				t.Fatalf("uploads = %d", len(ups))
			}
			up := ups[0]
			if up.folder != tc.folder || up.generate != "true" || string(up.data) != "meme" ||
				up.name != "2026-10-18_alice_5_"+hash20("meme")+".jpg" {
				t.Fatalf("upload = folder %q, generate %q, name %q, data %q", up.folder, up.generate, up.name, up.data)
			}
			if e.exists(t, tc.folder) {
				t.Fatal("the API backend also wrote a file")
			}
			if len(e.known.added) != 0 {
				t.Fatalf("library files added = %v: there is no local file", e.known.added)
			}
		})
	}
}

func TestAPIBackendDuplicateInFolder(t *testing.T) {
	us := newUploadServer(t)
	us.answer = "duplicate"
	e := apiEnv(t, us, canUpload, nil)
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	if got := e.api.reply(t); got != "Already saved." {
		t.Fatalf("reply = %q", got)
	}
}

func TestAPIBackendFailures(t *testing.T) {
	for _, answer := range []string{"broken", "file_error"} {
		t.Run(answer, func(t *testing.T) {
			us := newUploadServer(t)
			us.answer = answer
			e := apiEnv(t, us, canUpload, nil)
			e.api.files["p"] = []byte("meme")
			e.send(photoMsg(5, "p"))
			if got := e.api.reply(t); got != "Could not save, please try again later." {
				t.Fatalf("reply = %q", got)
			}
			// A transient server error must not land the meme where nobody may look.
			if e.exists(t, "tg-alice") {
				t.Fatal("fell back to the folder writer")
			}
		})
	}
}

// Without the write API the folder writer saves, exactly as before.
func TestAPIBackendFallsBackToTheFolderWriter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		caps     memesearch.Capabilities
		answer   string
		requests int
	}{
		{"old server: no request", memesearch.Capabilities{}, "old", 0},
		{"stale capability: 404", canUpload, "old", 1},
		{"folder not registered", canUpload, "invalid_folder", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			us := newUploadServer(t)
			us.answer = tc.answer
			e := apiEnv(t, us, tc.caps, nil)
			e.api.files["p"] = []byte("meme")
			e.send(photoMsg(5, "p"))
			if got := e.api.reply(t); got != "Saved." {
				t.Fatalf("reply = %q", got)
			}
			if n := len(us.got()); n != tc.requests {
				t.Fatalf("upload requests = %d, want %d", n, tc.requests)
			}
			if got := e.ls(t, "tg-alice"); len(got) != 1 {
				t.Fatalf("files = %v", got)
			}
			if len(e.known.added) != 1 {
				t.Fatalf("library files added = %v", e.known.added)
			}
		})
	}
}

func TestFolderBackendIsDefault(t *testing.T) {
	us := newUploadServer(t)
	e := newEnvWith(t, nil, func(d *Deps) {
		d.Uploader = us.client(t)
		d.Caps = memesearch.StaticCapabilities(canUpload)
	})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	if n := len(us.got()); n != 0 {
		t.Fatalf("upload requests = %d with the default backend", n)
	}
	if got := e.ls(t, "tg-alice"); len(got) != 1 {
		t.Fatalf("files = %v", got)
	}
}

func TestAPIBackendSearchableNotice(t *testing.T) {
	us := newUploadServer(t)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bot.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := newEnvWith(t, func(c *Config) { c.Backend = BackendAPI }, func(d *Deps) {
		d.Uploader = us.client(t)
		d.Caps = memesearch.StaticCapabilities{Upload: true, Listing: true}
		d.DB = db
	})
	e.api.files["p"] = []byte("meme")
	e.send(photoMsg(5, "p"))
	e.api.reply(t)
	e.changed(described(savedName(5, "meme"), memesearch.StatusReady))
	if got := e.api.reply(t); got != "Search finds this meme now." {
		t.Fatalf("notice = %q", got)
	}
}

func TestBackendValidation(t *testing.T) {
	fields, _ := configerr.Fields(Config{Dir: "x", Backend: "s3"}.Validate())
	if len(fields) != 1 || fields[0].Name != "Backend" {
		t.Fatalf("fields = %+v", fields)
	}
	for _, b := range []Backend{"", BackendFolder, BackendAPI} {
		if err := (Config{Dir: "x", Backend: b}).Validate(); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
}

func TestAPIBackendNeedsUploaderAndCaps(t *testing.T) {
	e := newEnv(t, nil) // borrows a valid bundle and permit
	deps := e.mod.deps
	deps.Uploader, deps.Caps = nil, nil
	if _, err := New(Config{Dir: t.TempDir(), Backend: BackendAPI}, deps); err == nil {
		t.Fatal("New accepted the API backend without an uploader")
	}
}
