package memesearch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testUploadToken = "ms_upload_secret_value"

// fakeAPI imitates both server generations behind one URL: with old set it
// routes like stock upstream v1 (search without the new meta fields, unknown
// parameters silently ignored, every other route a plain-text 404 like Rails'
// routing error); otherwise like the fork with every new endpoint.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server
	old atomic.Bool
	// failStatus, when non-zero, answers every request with that status.
	failStatus atomic.Int32
	// uploadScope false makes POST /api/v2/memes answer 403 like a token without media:write.
	uploadScope atomic.Bool

	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	method string
	path   string
	query  string
	auth   string
	form   map[string][]string // multipart fields (non-file)
	files  []string            // multipart file names under files[]
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t}
	f.uploadScope.Store(true)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client(t *testing.T, uploadToken string) *HTTPClient {
	t.Helper()
	c, err := New(Config{URL: f.srv.URL, Token: testToken, UploadToken: uploadToken, Timeout: 5 * time.Second}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeAPI) last() recorded {
	r := f.requests()
	if len(r) == 0 {
		f.t.Fatal("no request recorded")
	}
	return r[len(r)-1]
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
	if r.Method == http.MethodPost {
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			rec.form = r.MultipartForm.Value
			for _, fh := range r.MultipartForm.File["files[]"] {
				rec.files = append(rec.files, fh.Filename)
			}
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()

	if s := f.failStatus.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	if f.old.Load() {
		f.serveOld(w, r)
		return
	}
	f.serveNew(w, r)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// routeMissing answers like Rails' production routing error: not the API's JSON envelope.
func routeMissing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "<html><body>The page you were looking for doesn't exist.</body></html>")
}

func fakeMeme(id int, folder bool) string {
	extra := ""
	if folder {
		extra = `,"folder":"work/reactions","description_status":"ready"`
	}
	return fmt.Sprintf(`{"id":%d,"filename":"m%d.jpg","description":"meme %d","tags":["work"],`+
		`"media_type":"image/jpeg","content_url":"/api/v1/memes/%d/content"%s}`, id, id, id, id, extra)
}

func fakeMemes(n, first int, folder bool) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fakeMeme(first+i, folder)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func quoteAll(ss []string) string {
	b, _ := json.Marshal(ss)
	if ss == nil {
		return "[]"
	}
	return string(b)
}

func (f *fakeAPI) serveOld(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/search":
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		mode := q.Get("mode")
		if mode == "" {
			mode = "keyword"
		}
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":%s,"meta":{"query":%q,"mode":%q,"tags":%s,"limit":%d,"count":%d}}`,
			fakeMemes(limit, 1, false), q.Get("q"), mode, quoteAll(q["tag"]), limit, limit))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/memes/") &&
		!strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v1/memes/"), "/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v1/memes/"))
		writeJSON(w, http.StatusOK, `{"data":`+fakeMeme(id, false)+`}`)
	default:
		routeMissing(w)
	}
}

func (f *fakeAPI) serveNew(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/search":
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		mode := q.Get("mode")
		if mode == "" {
			mode = "keyword"
		}
		tm := q.Get("tag_match")
		if tm == "" {
			tm = "any"
		}
		next := ""
		if offset+limit <= 40 {
			next = fmt.Sprintf(`,"next_offset":%d`, offset+limit)
		}
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":%s,"meta":{"query":%q,"mode":%q,"tags":%s,"folders":%s,`+
			`"tag_match":%q,"limit":%d,"offset":%d,"count":%d%s}}`,
			fakeMemes(limit, offset+1, true), q.Get("q"), mode, quoteAll(q["tag"]), quoteAll(q["folder"]), tm,
			limit, offset, limit, next))
	case r.Method == http.MethodGet && path == "/api/v1/folders":
		writeJSON(w, http.StatusOK, `{"data":[{"name":"tg-alice","meme_count":3},{"name":"work/reactions","meme_count":0}],"meta":{"count":2}}`)
	case r.Method == http.MethodGet && path == "/api/v1/tags":
		writeJSON(w, http.StatusOK, `{"data":[{"name":"cat","meme_count":12},{"name":"work","meme_count":1}],"meta":{"count":2}}`)
	case r.Method == http.MethodGet && path == "/api/v1/memes":
		switch q.Get("cursor") {
		case "":
			writeJSON(w, http.StatusOK, `{"data":`+fakeMemes(2, 1, true)+`,"meta":{"limit":2,"count":2,"has_more":true,"next_cursor":"c2"}}`)
		case "c2":
			writeJSON(w, http.StatusOK, `{"data":`+fakeMemes(1, 3, true)+`,"meta":{"limit":2,"count":1,"has_more":false,"next_cursor":"c3"}}`)
		case "c3":
			writeJSON(w, http.StatusOK, `{"data":[],"meta":{"limit":2,"count":0,"has_more":false}}`)
		default:
			writeJSON(w, http.StatusUnprocessableEntity, `{"error":{"code":"invalid_cursor","message":"bad"}}`)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/memes/") && strings.HasSuffix(path, "/similar"):
		id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/memes/"), "/similar"))
		if id == 999 {
			writeJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"The requested meme was not found."}}`)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":%s,"meta":{"meme_id":%d,"limit":%d,"count":%d}}`,
			fakeMemes(limit, id+1, true), id, limit, limit))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/memes/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/api/v1/memes/"))
		writeJSON(w, http.StatusOK, `{"data":`+fakeMeme(id, true)+`}`)
	case r.Method == http.MethodPost && path == "/api/v2/memes":
		f.serveUpload(w, r)
	default:
		routeMissing(w)
	}
}

func (f *fakeAPI) serveUpload(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testUploadToken || !f.uploadScope.Load() {
		writeJSON(w, http.StatusForbidden, `{"error":{"code":"forbidden","message":"This token does not grant media:write."}}`)
		return
	}
	var files []string
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["files[]"] {
			files = append(files, fh.Filename)
		}
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, `{"error":{"code":"invalid_files","message":"files must contain 1 to 10 uploaded files."}}`)
		return
	}
	folder := r.FormValue("folder")
	if folder == "" {
		folder = "direct-uploads"
	}
	// First file created, second a duplicate of a not yet indexed file, the rest fail.
	var created, dups, errs []string
	for i, name := range files {
		switch i {
		case 0:
			created = append(created, fmt.Sprintf(`{"filename":%q,"description_queued":true,"meme":%s}`, name, fakeMeme(43, true)))
		case 1:
			dups = append(dups, fmt.Sprintf(`{"filename":%q,"meme":null}`, name))
		default:
			errs = append(errs, fmt.Sprintf(`{"filename":%q,"error":{"code":"invalid_image","message":"Invalid image file."}}`, name))
		}
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":{"created":[%s],"duplicates":[%s],"errors":[%s]},"meta":{"folder":%q,"generate_description":%t}}`,
		strings.Join(created, ","), strings.Join(dups, ","), strings.Join(errs, ","), folder, r.FormValue("generate_description") == "true"))
}
