package memesearch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMemeOptionalFields(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	memes, err := c.Search(t.Context(), "cat", SearchOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if memes[0].Folder != "work/reactions" || memes[0].DescriptionStatus != StatusReady || !memes[0].Searchable() {
		t.Fatalf("new server meme = %+v", memes[0])
	}

	f.old.Store(true)
	memes, err = c.Search(t.Context(), "cat", SearchOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if memes[0].Folder != "" || memes[0].DescriptionStatus != "" || memes[0].Searchable() {
		t.Fatalf("old server meme = %+v", memes[0])
	}
}

func TestDescriptionStatusUnknownIsPending(t *testing.T) {
	body := `{"data":{"id":1,"filename":"a.jpg","description":null,"tags":[],"media_type":"image/jpeg",` +
		`"content_url":"/api/v1/memes/1/content","description_status":"embedding"}}`
	srv, _, _ := serve(t, http.StatusOK, body, nil)
	m, err := newTestClient(t, srv.URL, nil).Meme(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.DescriptionStatus != StatusPending {
		t.Fatalf("status = %q, want pending", m.DescriptionStatus)
	}
}

func TestSearchOmitsNewParamsByDefault(t *testing.T) {
	f := newFakeAPI(t)
	if _, err := f.client(t).Search(t.Context(), "cat", SearchOptions{Limit: 3, Tags: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(f.last().query)
	for _, k := range []string{"folder", "tag_match", "offset"} {
		if q.Has(k) {
			t.Fatalf("query %q carries %s", f.last().query, k)
		}
	}
}

func TestSearchPageNewServer(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	page, err := c.SearchPage(t.Context(), "cat", SearchOptions{
		Mode: ModeVector, Limit: 20, Offset: 20, Tags: []string{"a", "b"},
		Folders: []string{"tg-alice", "work/reactions"}, TagMatch: TagMatchAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(f.last().query)
	if !slices.Equal(q["folder"], []string{"tg-alice", "work/reactions"}) || q.Get("tag_match") != "all" || q.Get("offset") != "20" {
		t.Fatalf("query = %q", f.last().query)
	}
	if len(page.Ignored) != 0 {
		t.Fatalf("ignored = %v", page.Ignored)
	}
	if page.NextOffset != 40 || len(page.Memes) != 20 || page.Memes[0].ID != 21 {
		t.Fatalf("page next=%d len=%d first=%d", page.NextOffset, len(page.Memes), page.Memes[0].ID)
	}

	last, err := c.SearchPage(t.Context(), "cat", SearchOptions{Limit: 20, Offset: 40})
	if err != nil {
		t.Fatal(err)
	}
	if last.NextOffset != 0 {
		t.Fatalf("last page next = %d", last.NextOffset)
	}
}

func TestSearchPageOldServerReportsIgnoredParams(t *testing.T) {
	f := newFakeAPI(t)
	f.old.Store(true)
	c := f.client(t)
	page, err := c.SearchPage(t.Context(), "cat", SearchOptions{
		Limit: 5, Offset: 5, Tags: []string{"a"}, Folders: []string{"x"}, TagMatch: TagMatchAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Param{ParamFolder, ParamTagMatch, ParamOffset}
	if !slices.Equal(page.Ignored, want) {
		t.Fatalf("ignored = %v, want %v", page.Ignored, want)
	}
	if len(page.Memes) != 5 || page.NextOffset != 0 {
		t.Fatalf("page = %+v", page)
	}
	// tag_match=any is the old behaviour, so an old server applying "any" is not a mismatch.
	page, err = c.SearchPage(t.Context(), "cat", SearchOptions{Limit: 5, Tags: []string{"a"}, TagMatch: TagMatchAny})
	if err != nil || len(page.Ignored) != 0 {
		t.Fatalf("any on old server: ignored=%v err=%v", page.Ignored, err)
	}
}

func TestSearchPageEchoMismatchIsIgnored(t *testing.T) {
	body := `{"data":[],"meta":{"query":"q","mode":"keyword","tags":[],"folders":["other"],"tag_match":"any",` +
		`"limit":5,"offset":0,"count":0}}`
	srv, _, _ := serve(t, http.StatusOK, body, nil)
	page, err := newTestClient(t, srv.URL, nil).SearchPage(t.Context(), "q",
		SearchOptions{Limit: 5, Folders: []string{"mine"}, TagMatch: TagMatchAll, Offset: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(page.Ignored, []Param{ParamFolder, ParamTagMatch, ParamOffset}) {
		t.Fatalf("ignored = %v", page.Ignored)
	}
}

func TestSearchFailsWhenParamIgnored(t *testing.T) {
	f := newFakeAPI(t)
	f.old.Store(true)
	_, err := f.client(t).Search(t.Context(), "cat", SearchOptions{Limit: 5, Folders: []string{"x"}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestSearchValidatesNewOptions(t *testing.T) {
	long := strings.Repeat("f", MaxFolderLen+1)
	many := make([]string, MaxFolders+1)
	for i := range many {
		many[i] = "f" + string(rune('a'+i))
	}
	cases := map[string]SearchOptions{
		"too many folders": {Limit: 1, Folders: many},
		"long folder":      {Limit: 1, Folders: []string{long}},
		"blank folder":     {Limit: 1, Folders: []string{" "}},
		"bad tag_match":    {Limit: 1, TagMatch: "some"},
		"negative offset":  {Limit: 1, Offset: -1},
		"offset too big":   {Limit: 1, Offset: MaxOffset + 1},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, hits := serve(t, http.StatusOK, "{}", nil)
			_, err := newTestClient(t, srv.URL, nil).SearchPage(t.Context(), "q", opts)
			if !errors.Is(err, ErrInvalidRequest) || hits.Load() != 0 {
				t.Fatalf("err = %v, hits = %d", err, hits.Load())
			}
		})
	}
}

func TestFoldersAndTags(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	folders, err := c.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []Folder{{Name: "tg-alice", MemeCount: 3}, {Name: "work/reactions", MemeCount: 0}}; !slices.Equal(folders, want) {
		t.Fatalf("folders = %+v", folders)
	}
	tags, err := c.Tags(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []Tag{{Name: "cat", MemeCount: 12}, {Name: "work", MemeCount: 1}}; !slices.Equal(tags, want) {
		t.Fatalf("tags = %+v", tags)
	}

	f.old.Store(true)
	if _, err := c.Folders(t.Context()); !errors.Is(err, ErrUnsupported) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("old folders err = %v", err)
	}
	if _, err := c.Tags(t.Context()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old tags err = %v", err)
	}
}

func TestFoldersBadResponse(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"name":"a"}],"meta":{"count":1}}`,
		`{"data":[{"meme_count":1}],"meta":{"count":1}}`,
		`{"data":[]}`,
	} {
		srv, _, _ := serve(t, http.StatusOK, body, nil)
		if _, err := newTestClient(t, srv.URL, nil).Folders(t.Context()); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("%s: err = %v", body, err)
		}
	}
}

func TestListMemes(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	page, err := c.ListMemes(t.Context(), ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := url.ParseQuery(f.last().query); q.Get("limit") != "2" || q.Has("cursor") {
		t.Fatalf("query = %q", f.last().query)
	}
	if !page.HasMore || page.NextCursor != "c2" || len(page.Memes) != 2 || page.Memes[0].Folder == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	page, err = c.ListMemes(t.Context(), ListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || page.NextCursor != "c3" || len(page.Memes) != 1 || page.Memes[0].ID != 3 {
		t.Fatalf("page 2 = %+v", page)
	}
	page, err = c.ListMemes(t.Context(), ListOptions{Cursor: "c3"})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := url.ParseQuery(f.last().query); q.Has("limit") {
		t.Fatalf("zero limit sent: %q", f.last().query)
	}
	if page.NextCursor != "" || len(page.Memes) != 0 {
		t.Fatalf("empty page = %+v", page)
	}
	if _, err := c.ListMemes(t.Context(), ListOptions{Cursor: "garbage"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("bad cursor err = %v", err)
	}

	f.old.Store(true)
	if _, err := c.ListMemes(t.Context(), ListOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old listing err = %v", err)
	}
}

func TestListMemesLocalValidation(t *testing.T) {
	for _, opts := range []ListOptions{{Limit: -1}, {Limit: MaxListLimit + 1}, {Cursor: strings.Repeat("c", 201)}} {
		srv, _, hits := serve(t, http.StatusOK, "{}", nil)
		if _, err := newTestClient(t, srv.URL, nil).ListMemes(t.Context(), opts); !errors.Is(err, ErrInvalidRequest) || hits.Load() != 0 {
			t.Fatalf("%+v: err = %v hits = %d", opts, err, hits.Load())
		}
	}
}

func TestSimilar(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	memes, err := c.Similar(t.Context(), 42, 5)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.path != "/api/v1/memes/42/similar" || r.query != "limit=5" {
		t.Fatalf("request = %s?%s", r.path, r.query)
	}
	if len(memes) != 5 || memes[0].ID != 43 {
		t.Fatalf("memes = %+v", memes)
	}
	_, err = c.Similar(t.Context(), 999, 5)
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown meme err = %v", err)
	}
	if !c.Capabilities().Similar {
		t.Fatal("a JSON 404 is a missing meme, not a missing endpoint")
	}

	f.old.Store(true)
	if _, err := c.Similar(t.Context(), 42, 5); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old similar err = %v", err)
	}
	if c.Capabilities().Similar {
		t.Fatal("old server answer must turn Similar off")
	}
	for _, args := range [][2]int64{{0, 5}, {1, 0}, {1, MaxLimit + 1}} {
		if _, err := c.Similar(t.Context(), args[0], int(args[1])); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestUpload(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	res, err := c.Upload(t.Context(), []UploadFile{
		{Name: "a.jpg", Data: []byte("aaa")}, {Name: "b.png", Data: []byte("bbb")}, {Name: "c.webp", Data: []byte("ccc")},
	}, UploadOptions{Folder: "tg-alice", GenerateDescription: true})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.method != http.MethodPost || r.path != "/api/v2/memes" || r.auth != "Bearer "+testToken {
		t.Fatalf("request = %s %s auth=%q", r.method, r.path, r.auth)
	}
	if !slices.Equal(r.files, []string{"a.jpg", "b.png", "c.webp"}) ||
		!slices.Equal(r.form["folder"], []string{"tg-alice"}) || !slices.Equal(r.form["generate_description"], []string{"true"}) {
		t.Fatalf("form = %v files = %v", r.form, r.files)
	}
	if res.Folder != "tg-alice" || !res.GenerateDescription {
		t.Fatalf("meta = %+v", res)
	}
	if len(res.Created) != 1 || res.Created[0].Filename != "a.jpg" || !res.Created[0].DescriptionQueued || res.Created[0].Meme.ID != 43 {
		t.Fatalf("created = %+v", res.Created)
	}
	if len(res.Duplicates) != 1 || res.Duplicates[0].Filename != "b.png" || res.Duplicates[0].Meme != nil {
		t.Fatalf("duplicates = %+v", res.Duplicates)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "invalid_image" || res.Errors[0].Filename != "c.webp" {
		t.Fatalf("errors = %+v", res.Errors)
	}

	if _, err := c.Upload(t.Context(), []UploadFile{{Name: "a.jpg", Data: []byte("a")}}, UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.form["folder"] != nil || r.form["generate_description"] != nil {
		t.Fatalf("defaults sent: %v", r.form)
	}
}

func TestUploadUnsupported(t *testing.T) {
	f := newFakeAPI(t)
	f.old.Store(true)
	if _, err := f.client(t).Upload(t.Context(), []UploadFile{{Name: "a.jpg", Data: []byte("a")}}, UploadOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old server err = %v", err)
	}
}

// Without media:write on the one API token the server answers 403: not an
// unsupported server, so the error stays a plain ErrForbidden.
func TestUploadWithoutScope(t *testing.T) {
	f := newFakeAPI(t)
	f.uploadScope.Store(false)
	_, err := f.client(t).Upload(t.Context(), []UploadFile{{Name: "a.jpg", Data: []byte("a")}}, UploadOptions{})
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadLocalValidation(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	eleven := make([]UploadFile, MaxUploadFiles+1)
	for i := range eleven {
		eleven[i] = UploadFile{Name: "a.jpg", Data: []byte("a")}
	}
	for name, files := range map[string][]UploadFile{
		"none":     nil,
		"too many": eleven,
		"no name":  {{Data: []byte("a")}},
		"empty":    {{Name: "a.jpg"}},
	} {
		if _, err := c.Upload(t.Context(), files, UploadOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if len(f.requests()) != 0 {
		t.Fatal("invalid uploads must not reach the server")
	}
}

func TestProbe(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if got := c.Capabilities(); got != (Capabilities{}) {
		t.Fatalf("before probe = %+v", got)
	}
	got := c.Probe(t.Context())
	want := Capabilities{FolderList: true, TagList: true, FolderFilter: true, TagMatch: true, Offset: true,
		Listing: true, Similar: true, Upload: true, CheckedAt: now}
	if got != want || c.Capabilities() != want {
		t.Fatalf("new server = %+v", got)
	}
	// The upload probe sends an empty form, which the server rejects before storing anything.
	for _, r := range f.requests() {
		if r.method == http.MethodPost && len(r.files) != 0 {
			t.Fatalf("probe uploaded files: %v", r.files)
		}
	}

	f.old.Store(true)
	got = c.Probe(t.Context())
	if want := (Capabilities{CheckedAt: now}); got != want {
		t.Fatalf("old server = %+v", got)
	}
}

func TestProbeUploadScope(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := newFakeAPI(t)
	f.uploadScope.Store(false)
	c, err := New(Config{URL: f.srv.URL, Token: testToken}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if c.Probe(t.Context()).Upload {
		t.Fatal("a token without media:write must mean no upload")
	}
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "media:write") {
		t.Fatalf("no info log about the missing scope:\n%s", buf.String())
	}
	f.uploadScope.Store(true)
	if !c.Probe(t.Context()).Upload {
		t.Fatal("scope granted later must turn uploads on")
	}
}

func TestProbeKeepsValuesOnTransientErrors(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	before := c.Probe(t.Context())
	for _, s := range []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusUnauthorized} {
		f.failStatus.Store(int32(s))
		after := c.Probe(t.Context())
		after.CheckedAt = before.CheckedAt
		if after != before {
			t.Fatalf("status %d changed capabilities: %+v", s, after)
		}
	}
}

func TestSearchObservesCapabilities(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newFakeAPI(t)
	f.old.Store(true)
	c, err := New(Config{URL: f.srv.URL, Token: testToken}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	c.Probe(t.Context())
	f.old.Store(false)
	if _, err := c.Search(t.Context(), "cat", SearchOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	got := c.Capabilities()
	if !got.FolderFilter || !got.TagMatch || !got.Offset {
		t.Fatalf("after new-server search = %+v", got)
	}
	if !strings.Contains(buf.String(), "level=INFO msg=\"memesearch capability changed\" capability=folder_filter available=true") {
		t.Fatalf("no info log on flip:\n%s", buf.String())
	}

	f.old.Store(true)
	page, err := c.SearchPage(t.Context(), "cat", SearchOptions{Limit: 1, Folders: []string{"x"}})
	if err != nil || len(page.Ignored) != 1 {
		t.Fatalf("ignored = %v err = %v", page.Ignored, err)
	}
	if c.Capabilities().FolderFilter {
		t.Fatal("an ignored folder filter must turn FolderFilter off")
	}
}

func TestWatchCapabilitiesReprobes(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		c.WatchCapabilities(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for countPath(f.requests(), "/api/v1/folders") < 2 {
		select {
		case <-deadline:
			t.Fatal("no second probe")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if !c.Capabilities().FolderList {
		t.Fatal("watch did not store the probe result")
	}
}

func countPath(rs []recorded, path string) int {
	n := 0
	for _, r := range rs {
		if r.path == path {
			n++
		}
	}
	return n
}

func TestStaticCapabilities(t *testing.T) {
	var src CapabilitySource = StaticCapabilities{Similar: true}
	if !src.Capabilities().Similar {
		t.Fatal("static source lost its value")
	}
	var _ CapabilitySource = (*HTTPClient)(nil)
}

func TestLogsNeverContainTokenOnNewEndpoints(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newFakeAPI(t)
	c, err := New(Config{URL: f.srv.URL, Token: testToken}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	c.Probe(t.Context())
	_, _ = c.Upload(t.Context(), []UploadFile{{Name: "a.jpg", Data: []byte("a")}}, UploadOptions{})
	_, _ = c.ListMemes(t.Context(), ListOptions{})
	_, _ = c.Similar(t.Context(), 1, 1)
	if strings.Contains(buf.String(), testToken) {
		t.Fatal("token in logs")
	}
	if !strings.Contains(buf.String(), "path=/api/v2/memes") {
		t.Fatalf("upload call not logged:\n%s", buf.String())
	}
}

func TestWatchCapabilitiesZeroIntervalUsesDefault(t *testing.T) {
	f := newFakeAPI(t)
	c := f.client(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.WatchCapabilities(ctx, 0)
	}()
	for countPath(f.requests(), "/api/v1/folders") < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestFirstProbeLogsSummaryAtInfo(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := newFakeAPI(t)
	f.old.Store(true)
	c, err := New(Config{URL: f.srv.URL, Token: testToken}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	c.Probe(t.Context())
	if !strings.Contains(buf.String(), "memesearch capabilities") {
		t.Fatalf("no startup summary:\n%s", buf.String())
	}
	buf.Reset()
	c.Probe(t.Context())
	if buf.Len() != 0 {
		t.Fatalf("unchanged re-probe logged at info:\n%s", buf.String())
	}
}
