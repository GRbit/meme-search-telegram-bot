package memesearch

import (
	"github.com/grbit/meme-search-telegram-bot/configerr"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "ms_secret_token_value"

// conformanceFixture mirrors upstream integrations/shared/search-response-conformance.json
// (meme-search commit 1b50472), copied to testdata/ so tests need no upstream checkout.
type conformanceFixture struct {
	Valid []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	} `json:"valid"`
	Invalid []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	} `json:"invalid"`
}

func loadConformance(t *testing.T) conformanceFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/search-response-conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var f conformanceFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Valid) == 0 || len(f.Invalid) == 0 {
		t.Fatal("fixture has no cases")
	}
	return f
}

func newTestClient(t *testing.T, baseURL string, log *slog.Logger) *HTTPClient {
	t.Helper()
	c, err := New(Config{URL: baseURL, Token: testToken, Timeout: 5 * time.Second}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// serve answers every request with status and body and records the last request.
func serve(t *testing.T, status int, body string, hdr map[string]string) (*httptest.Server, *atomic.Pointer[http.Request], *atomic.Int32) {
	t.Helper()
	var last atomic.Pointer[http.Request]
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store(r.Clone(context.Background()))
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &last, &hits
}

func TestSearchConformanceValid(t *testing.T) {
	f := loadConformance(t)
	for _, tc := range f.Valid {
		t.Run(tc.Name, func(t *testing.T) {
			srv, _, _ := serve(t, http.StatusOK, string(tc.Payload), map[string]string{"Content-Type": "application/json"})
			c := newTestClient(t, srv.URL, nil)
			memes, err := c.Search(t.Context(), "ship it", SearchOptions{Mode: ModeKeyword, Limit: 10})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(memes) != 1 {
				t.Fatalf("got %d memes, want 1", len(memes))
			}
		})
	}
}

func TestSearchConformanceInvalid(t *testing.T) {
	f := loadConformance(t)
	for _, tc := range f.Invalid {
		t.Run(tc.Name, func(t *testing.T) {
			srv, _, _ := serve(t, http.StatusOK, string(tc.Payload), nil)
			c := newTestClient(t, srv.URL, nil)
			_, err := c.Search(t.Context(), "ship it", SearchOptions{Mode: ModeKeyword, Limit: 10})
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err = %v, want ErrBadResponse", err)
			}
		})
	}
}

func TestSearchDecodesFields(t *testing.T) {
	f := loadConformance(t)
	srv, _, _ := serve(t, http.StatusOK, string(f.Valid[1].Payload), nil)
	c := newTestClient(t, srv.URL, nil)
	memes, err := c.Search(t.Context(), "incident", SearchOptions{Mode: ModeVector, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	want := Meme{
		ID: 7, Filename: "incident.jpg", Description: "", Tags: []string{"work"},
		MediaType: "image/jpeg", ContentURL: "/api/v1/memes/7/content",
	}
	got := memes[0]
	if got.ID != want.ID || got.Filename != want.Filename || got.Description != want.Description ||
		got.MediaType != want.MediaType || got.ContentURL != want.ContentURL ||
		len(got.Tags) != 1 || got.Tags[0] != "work" {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	srv2, _, _ := serve(t, http.StatusOK, string(f.Valid[0].Payload), nil)
	memes, err = newTestClient(t, srv2.URL, nil).Search(t.Context(), "ship it", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if memes[0].Description != "A celebratory launch" {
		t.Fatalf("description = %q", memes[0].Description)
	}
}

func TestSearchEmptyResult(t *testing.T) {
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":[],"limit":5,"count":0}}`
	srv, _, _ := serve(t, http.StatusOK, body, nil)
	memes, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Mode: ModeVector, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(memes) != 0 {
		t.Fatalf("got %d memes", len(memes))
	}
}

func TestSearchRequest(t *testing.T) {
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":[],"limit":5,"count":0}}`
	srv, last, _ := serve(t, http.StatusOK, body, nil)
	c := newTestClient(t, srv.URL, nil)
	q := "\u043a\u043e\u0442 & dog?=1"
	if _, err := c.Search(t.Context(), q, SearchOptions{Mode: ModeVector, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	r := last.Load()
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/search" {
		t.Fatalf("got %s %s", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	v := r.URL.Query()
	if v.Get("q") != q || v.Get("mode") != "vector" || v.Get("limit") != "5" {
		t.Fatalf("query = %v", v)
	}
}

// Each tag is its own repeated parameter: commas are part of a tag, not separators.
func TestSearchSendsTags(t *testing.T) {
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":["a,b","кот"],"limit":5,"count":0}}`
	srv, last, _ := serve(t, http.StatusOK, body, nil)
	tags := []string{"a,b", "кот"}
	if _, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Limit: 5, Tags: tags}); err != nil {
		t.Fatal(err)
	}
	if got := last.Load().URL.Query()["tag"]; !slices.Equal(got, tags) {
		t.Fatalf("tag params = %q, want %q", got, tags)
	}
}

func TestSearchOmitsEmptyMode(t *testing.T) {
	body := `{"data":[],"meta":{"query":"x","mode":"keyword","tags":[],"limit":5,"count":0}}`
	srv, last, _ := serve(t, http.StatusOK, body, nil)
	if _, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if last.Load().URL.Query().Has("mode") {
		t.Fatal("mode sent although empty")
	}
}

func TestBaseURLForms(t *testing.T) {
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":[],"limit":5,"count":0}}`
	for _, tc := range []struct{ suffix, wantPath string }{
		{"", "/api/v1/search"},
		{"/", "/api/v1/search"},
		{"/memes", "/memes/api/v1/search"},
		{"/memes/", "/memes/api/v1/search"},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			srv, last, _ := serve(t, http.StatusOK, body, nil)
			c := newTestClient(t, srv.URL+tc.suffix, nil)
			if _, err := c.Search(t.Context(), "x", SearchOptions{Limit: 5}); err != nil {
				t.Fatal(err)
			}
			if got := last.Load().URL.Path; got != tc.wantPath {
				t.Fatalf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

func TestSearchLocalValidation(t *testing.T) {
	srv, _, hits := serve(t, http.StatusOK, `{}`, nil)
	c := newTestClient(t, srv.URL, nil)
	long := strings.Repeat("\u044f", MaxQueryLen+1)
	for _, tc := range []struct {
		name  string
		query string
		opts  SearchOptions
	}{
		{"empty", "", SearchOptions{Limit: 5}},
		{"whitespace", " \t\n\u00a0", SearchOptions{Limit: 5}},
		{"too long", long, SearchOptions{Limit: 5}},
		{"limit zero", "x", SearchOptions{Limit: 0}},
		{"limit negative", "x", SearchOptions{Limit: -1}},
		{"limit too big", "x", SearchOptions{Limit: MaxLimit + 1}},
		{"bad mode", "x", SearchOptions{Mode: "hybrid", Limit: 5}},
		{"too many tags", "x", SearchOptions{Limit: 5, Tags: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}},
		{"tag too long", "x", SearchOptions{Limit: 5, Tags: []string{strings.Repeat("я", MaxTagLen+1)}}},
		{"empty tag", "x", SearchOptions{Limit: 5, Tags: []string{" "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Search(t.Context(), tc.query, tc.opts)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server hit %d times", n)
	}
}

func TestSearchAcceptsMaxRuneQuery(t *testing.T) {
	// 200 Cyrillic runes are 400 bytes: the limit is in characters, like upstream's Ruby String#length.
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":[],"limit":20,"count":0}}`
	srv, _, hits := serve(t, http.StatusOK, body, nil)
	q := strings.Repeat("\u044f", MaxQueryLen)
	if _, err := newTestClient(t, srv.URL, nil).Search(t.Context(), q, SearchOptions{Limit: MaxLimit}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatal("no request made")
	}
}

func TestErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusBadRequest, ErrInvalidRequest},
		{http.StatusUnprocessableEntity, ErrInvalidRequest},
		{http.StatusTooManyRequests, ErrRateLimited},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			body := `{"error":{"code":"some_code","message":"some message"}}`
			srv, _, _ := serve(t, tc.status, body, map[string]string{"Content-Type": "application/json"})
			_, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Limit: 5})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err %T is not *APIError", err)
			}
			if apiErr.Status != tc.status || apiErr.Code != "some_code" || apiErr.Message != "some message" {
				t.Fatalf("got %+v", apiErr)
			}
		})
	}
}

func TestServerErrorMatchesNoSentinel(t *testing.T) {
	srv, _, _ := serve(t, http.StatusBadGateway, "<html>bad gateway</html>", nil)
	_, err := newTestClient(t, srv.URL, nil).Meme(t.Context(), 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || apiErr.Code != "" {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []error{ErrUnauthorized, ErrForbidden, ErrNotFound, ErrInvalidRequest, ErrRateLimited} {
		if errors.Is(err, s) {
			t.Fatalf("502 matched %v", s)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	body := `{"error":{"code":"rate_limited","message":"Too many"}}`
	srv, _, _ := serve(t, http.StatusTooManyRequests, body, map[string]string{"Retry-After": "17"})
	_, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Limit: 5})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if apiErr.RetryAfter != 17*time.Second {
		t.Fatalf("RetryAfter = %v", apiErr.RetryAfter)
	}
}

func TestRetryAfterAbsentOrGarbage(t *testing.T) {
	for _, v := range []string{"", "soon", "-5"} {
		hdr := map[string]string{}
		if v != "" {
			hdr["Retry-After"] = v
		}
		srv, _, _ := serve(t, http.StatusTooManyRequests, `{}`, hdr)
		_, err := newTestClient(t, srv.URL, nil).Search(t.Context(), "x", SearchOptions{Limit: 5})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.RetryAfter != 0 {
			t.Fatalf("Retry-After %q: err = %v", v, err)
		}
	}
}

func TestMeme(t *testing.T) {
	body := `{"data":{"id":42,"filename":"ship-it.gif","description":null,"tags":["a"],` +
		`"media_type":"image/gif","content_url":"/api/v1/memes/42/content","extra":1}}`
	srv, last, _ := serve(t, http.StatusOK, body, nil)
	m, err := newTestClient(t, srv.URL+"/", nil).Meme(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != 42 || m.Filename != "ship-it.gif" || m.Description != "" || m.MediaType != "image/gif" {
		t.Fatalf("got %+v", m)
	}
	r := last.Load()
	if r.URL.Path != "/api/v1/memes/42" || r.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
	}
}

func TestMemeBadResponse(t *testing.T) {
	for name, body := range map[string]string{
		"no data":      `{}`,
		"not json":     `oops`,
		"absolute url": `{"data":{"id":1,"filename":"a","description":null,"tags":[],"media_type":"x","content_url":"http://evil/x"}}`,
		"missing tags": `{"data":{"id":1,"filename":"a","description":null,"media_type":"x","content_url":"/api/v1/memes/1/content"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := serve(t, http.StatusOK, body, nil)
			_, err := newTestClient(t, srv.URL, nil).Meme(t.Context(), 1)
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestInvalidIDNoRequest(t *testing.T) {
	srv, _, hits := serve(t, http.StatusOK, `{}`, nil)
	c := newTestClient(t, srv.URL, nil)
	if _, err := c.Meme(t.Context(), 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Meme(0) err = %v", err)
	}
	if _, err := c.Content(t.Context(), -3); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Content(-3) err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("server was hit")
	}
}

func TestMemeNotFound(t *testing.T) {
	srv, _, _ := serve(t, http.StatusNotFound, `{"error":{"code":"not_found","message":"Meme not found."}}`, nil)
	_, err := newTestClient(t, srv.URL, nil).Meme(t.Context(), 5)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestContent(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nfake")
	srv, last, _ := serve(t, http.StatusOK, string(data), map[string]string{"Content-Type": "image/png"})
	ct, err := newTestClient(t, srv.URL, nil).Content(t.Context(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ct.Data, data) || ct.MediaType != "image/png" {
		t.Fatalf("got %q %q", ct.Data, ct.MediaType)
	}
	r := last.Load()
	if r.URL.Path != "/api/v1/memes/9/content" || r.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
	}
}

func TestContentMediaTypeParams(t *testing.T) {
	srv, _, _ := serve(t, http.StatusOK, "x", map[string]string{"Content-Type": "image/jpeg; foo=bar"})
	ct, err := newTestClient(t, srv.URL, nil).Content(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if ct.MediaType != "image/jpeg" {
		t.Fatalf("MediaType = %q", ct.MediaType)
	}
}

func TestContentTooLarge(t *testing.T) {
	big := strings.Repeat("a", 101)
	for name, hdr := range map[string]map[string]string{
		// With Content-Length the client can refuse before reading; chunked bodies are cut by the reader cap.
		"content-length": {"Content-Type": "image/png"},
		"chunked":        {"Content-Type": "image/png", "Transfer-Encoding": "chunked"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := serve(t, http.StatusOK, big, hdr)
			c := newTestClient(t, srv.URL, nil)
			c.maxContentBytes = 100
			_, err := c.Content(t.Context(), 1)
			if !errors.Is(err, ErrContentTooLarge) {
				t.Fatalf("err = %v, want ErrContentTooLarge", err)
			}
		})
	}
}

func TestContentAtCap(t *testing.T) {
	srv, _, _ := serve(t, http.StatusOK, strings.Repeat("a", 100), map[string]string{"Content-Type": "image/png"})
	c := newTestClient(t, srv.URL, nil)
	c.maxContentBytes = 100
	ct, err := c.Content(t.Context(), 1)
	if err != nil || len(ct.Data) != 100 {
		t.Fatalf("err = %v len = %d", err, len(ct.Data))
	}
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Add(1)
	}))
	t.Cleanup(other.Close)
	srv, _, _ := serve(t, http.StatusFound, "", map[string]string{"Location": other.URL + "/steal"})
	_, err := newTestClient(t, srv.URL, nil).Content(t.Context(), 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusFound {
		t.Fatalf("err = %v", err)
	}
	if target.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no url":     {Token: "t"},
		"bad scheme": {URL: "ftp://x", Token: "t"},
		"no host":    {URL: "http://", Token: "t"},
		"no token":   {URL: "http://x"},
		"query":      {URL: "http://x/?a=1", Token: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg, nil, nil); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestLogsNeverContainToken(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := `{"data":[],"meta":{"query":"x","mode":"vector","tags":[],"limit":5,"count":0}}`
	srv, _, _ := serve(t, http.StatusOK, body, nil)
	c := newTestClient(t, srv.URL, log)
	if _, err := c.Search(t.Context(), "funny cat", SearchOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	srv2, _, _ := serve(t, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"no"}}`, nil)
	_, _ = newTestClient(t, srv2.URL, log).Meme(t.Context(), 3)
	out := buf.String()
	if strings.Contains(out, testToken) {
		t.Fatalf("token leaked into logs: %s", out)
	}
	for _, want := range []string{"/api/v1/search", "funny cat", "status=200", "count=0", "/api/v1/memes/3", "status=401"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q: %s", want, out)
		}
	}
}

func TestConfigValidateNamesFields(t *testing.T) {
	err := Config{URL: "https://u:p@x", Timeout: -time.Second}.Validate()
	fields, rest := configerr.Fields(err)
	if len(rest) != 0 {
		t.Fatalf("non-field errors: %v", rest)
	}
	got := map[string]bool{}
	for _, f := range fields {
		got[f.Name] = true
	}
	for _, want := range []string{"URL", "Token", "Timeout"} {
		if !got[want] {
			t.Errorf("Validate did not reject %s: %v", want, err)
		}
	}
	if err := (Config{URL: "http://x", Token: "t"}).Validate(); err != nil {
		t.Errorf("zero timeout must be valid: %v", err)
	}
}
