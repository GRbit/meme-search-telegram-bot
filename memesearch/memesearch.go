// Package memesearch is a client for meme-search Search API v1.
//
// The contract lives upstream in docs/search-api-openapi.yml. The API is
// read-only and additive: unknown response fields must be ignored.
package memesearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grbit/meme-search-telegram-bot/configerr"
)

// Limits enforced by the API; requests outside them get 422.
const (
	MaxQueryLen = 200
	MaxLimit    = 20
	MaxTags     = 10
	MaxTagLen   = 20
)

// Mode selects the search algorithm.
type Mode string

// Supported search modes.
const (
	ModeVector  Mode = "vector"
	ModeKeyword Mode = "keyword"
)

// Meme is one item of the collection as returned by the API.
type Meme struct {
	ID int64 `json:"id"`
	// Filename is a base name without directory; it can repeat across folders.
	Filename string `json:"filename"`
	// Description is empty when the meme has not been described yet (API null).
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	MediaType   string   `json:"media_type"`
	ContentURL  string   `json:"content_url"`
}

// SearchOptions tune one search request.
type SearchOptions struct {
	Mode  Mode
	Limit int // 1..MaxLimit
	// Tags restrict results to memes with any of them (at most MaxTags, each at
	// most MaxTagLen characters); the server filters before ranking.
	Tags []string
}

// Content is the original media of one meme.
type Content struct {
	Data      []byte
	MediaType string
}

// Client is the part of the API the bot uses.
type Client interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]Meme, error)
	Meme(ctx context.Context, id int64) (Meme, error)
	Content(ctx context.Context, id int64) (Content, error)
}

// Config locates the API.
type Config struct {
	URL     string // base URL, e.g. https://memes.example.com
	Token   string // bearer token with search:read and media:read
	Timeout time.Duration
}

// Sentinel errors matched with errors.Is; *APIError unwraps to one of them.
//
// ErrBadResponse means a 2xx answer that breaks the v1 contract (missing
// required field, wrong type, foreign content_url). ErrContentTooLarge means a
// media body above the client's cap.
var (
	ErrUnauthorized    = errors.New("memesearch: unauthorized")
	ErrForbidden       = errors.New("memesearch: forbidden")
	ErrNotFound        = errors.New("memesearch: not found")
	ErrInvalidRequest  = errors.New("memesearch: invalid request")
	ErrRateLimited     = errors.New("memesearch: rate limited")
	ErrBadResponse     = errors.New("memesearch: bad response")
	ErrContentTooLarge = errors.New("memesearch: content too large")
)

// APIError is a non-2xx answer from the API.
//
// RetryAfter is zero when the server sent no usable Retry-After header;
// upstream v1 does not send one, its rate limit window is one minute.
type APIError struct {
	Status     int
	Code       string // error.code from the body, if any
	Message    string // error.message from the body, if any
	RetryAfter time.Duration
}

// Error renders the status with the API's error code and message.
func (e *APIError) Error() string {
	return fmt.Sprintf("memesearch: http %d %s: %s", e.Status, e.Code, e.Message)
}

// Unwrap maps the HTTP status to a sentinel error, or nil for statuses without one.
func (e *APIError) Unwrap() error {
	switch e.Status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ErrInvalidRequest
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

const (
	// DefaultTimeout applies when Config.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// MaxContentBytes caps one media download; Telegram rejects photos above 10 MB
	// and documents above 50 MB anyway, and the whole body is held in memory.
	MaxContentBytes = 25 << 20

	maxJSONBytes  = 1 << 20
	maxErrorBytes = 64 << 10
)

var contentURLRe = regexp.MustCompile(`^/api/v1/memes/[1-9][0-9]*/content$`)

// Validate reports every invalid field as a *configerr.Field. A zero Timeout
// is valid and means DefaultTimeout.
func (c Config) Validate() error {
	var errs []error
	bad := func(field, reason string) { errs = append(errs, &configerr.Field{Name: field, Reason: reason}) }
	if c.URL == "" {
		bad("URL", "required")
	} else if u, err := url.Parse(c.URL); err != nil {
		bad("URL", fmt.Sprintf("not a URL: %v", err))
	} else {
		switch {
		case u.Scheme != "http" && u.Scheme != "https":
			bad("URL", fmt.Sprintf("must be http:// or https://, got %q", c.URL))
		case u.Host == "":
			bad("URL", fmt.Sprintf("has no host: %q", c.URL))
		case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
			// The token goes in a header; credentials or a query in the URL would
			// end up in logs and in every request.
			bad("URL", "must not contain credentials, a query or a fragment")
		}
	}
	if c.Token == "" {
		bad("Token", "required")
	}
	if c.Timeout < 0 {
		bad("Timeout", fmt.Sprintf("must not be negative, got %s", c.Timeout))
	}
	return errors.Join(errs...)
}

// HTTPClient implements Client over HTTP.
type HTTPClient struct {
	base            string // scheme://host[/prefix] without trailing slash
	token           string
	hc              *http.Client
	log             *slog.Logger
	maxContentBytes int64
}

var _ Client = (*HTTPClient)(nil)

// New builds an HTTP client. A nil httpClient means a default client with cfg.Timeout.
//
// The default client never follows redirects: the API does not redirect, and a
// redirect elsewhere would be a way to make the bot send its token to a third party.
func New(cfg Config, httpClient *http.Client, log *slog.Logger) (*HTTPClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("memesearch: %w", err)
	}
	u, _ := url.Parse(cfg.URL) // Validate parsed it already
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = DefaultTimeout
		}
		httpClient = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return &HTTPClient{
		base:            u.String(),
		token:           cfg.Token,
		hc:              httpClient,
		log:             log,
		maxContentBytes: MaxContentBytes,
	}, nil
}

// Search runs a search query.
//
// Invalid input is rejected locally with ErrInvalidRequest, without spending
// a request of the per-token rate limit.
func (c *HTTPClient) Search(ctx context.Context, query string, opts SearchOptions) ([]Meme, error) {
	if err := validateSearch(query, opts); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("q", query)
	if opts.Mode != "" {
		params.Set("mode", string(opts.Mode))
	}
	params.Set("limit", strconv.Itoa(opts.Limit))
	for _, t := range opts.Tags {
		params.Add("tag", t)
	}

	start := time.Now()
	const path = "/api/v1/search"
	var body searchResponse
	status, err := c.getJSON(ctx, path, params, &body)
	if err == nil {
		err = body.validate()
	}
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"query", query, "mode", string(opts.Mode), "limit", opts.Limit, "tags", opts.Tags,
			"status", status, "duration", time.Since(start), "err", err)
		return nil, err
	}
	memes := make([]Meme, len(*body.Data))
	for i, w := range *body.Data {
		memes[i] = w.meme()
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"query", query, "mode", string(opts.Mode), "limit", opts.Limit, "tags", opts.Tags,
		"status", status, "duration", time.Since(start), "count", len(memes))
	return memes, nil
}

// Meme fetches one meme's metadata.
func (c *HTTPClient) Meme(ctx context.Context, id int64) (Meme, error) {
	if id < 1 {
		return Meme{}, fmt.Errorf("%w: meme id %d", ErrInvalidRequest, id)
	}
	path := "/api/v1/memes/" + strconv.FormatInt(id, 10)
	start := time.Now()
	var body struct {
		Data *wireMeme `json:"data"`
	}
	status, err := c.getJSON(ctx, path, nil, &body)
	if err == nil {
		if body.Data == nil {
			err = fmt.Errorf("%w: missing data", ErrBadResponse)
		} else {
			err = body.Data.validate()
		}
	}
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"status", status, "duration", time.Since(start), "err", err)
		return Meme{}, err
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"status", status, "duration", time.Since(start))
	return body.Data.meme(), nil
}

// Content fetches one meme's original bytes.
//
// Bodies above MaxContentBytes fail with ErrContentTooLarge instead of being
// truncated, since a cut image is worse than none.
func (c *HTTPClient) Content(ctx context.Context, id int64) (Content, error) {
	if id < 1 {
		return Content{}, fmt.Errorf("%w: meme id %d", ErrInvalidRequest, id)
	}
	path := "/api/v1/memes/" + strconv.FormatInt(id, 10) + "/content"
	start := time.Now()
	ct, status, err := c.content(ctx, path)
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"status", status, "duration", time.Since(start), "err", err)
		return Content{}, err
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"status", status, "duration", time.Since(start), "bytes", len(ct.Data), "media_type", ct.MediaType)
	return ct, nil
}

func (c *HTTPClient) content(ctx context.Context, path string) (Content, int, error) {
	resp, err := c.do(ctx, path, nil, "*/*")
	if err != nil {
		return Content{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Content{}, resp.StatusCode, apiError(resp)
	}
	if resp.ContentLength > c.maxContentBytes {
		return Content{}, resp.StatusCode, fmt.Errorf("%w: %d bytes, limit %d", ErrContentTooLarge, resp.ContentLength, c.maxContentBytes)
	}
	// Content-Length is absent on chunked responses, so the reader cap is the real guard.
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxContentBytes+1))
	if err != nil {
		return Content{}, resp.StatusCode, fmt.Errorf("memesearch: read %s: %w", path, err)
	}
	if int64(len(data)) > c.maxContentBytes {
		return Content{}, resp.StatusCode, fmt.Errorf("%w: limit %d bytes", ErrContentTooLarge, c.maxContentBytes)
	}
	mt := resp.Header.Get("Content-Type")
	if parsed, _, err := mime.ParseMediaType(mt); err == nil {
		mt = parsed
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	return Content{Data: data, MediaType: mt}, resp.StatusCode, nil
}

// getJSON returns the HTTP status (0 when no response arrived) and decodes a 2xx body into out.
func (c *HTTPClient) getJSON(ctx context.Context, path string, params url.Values, out any) (int, error) {
	resp, err := c.do(ctx, path, params, "application/json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, apiError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("memesearch: read %s: %w", path, err)
	}
	if len(data) > maxJSONBytes {
		return resp.StatusCode, fmt.Errorf("%w: body above %d bytes", ErrBadResponse, maxJSONBytes)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: %w", ErrBadResponse, err)
	}
	return resp.StatusCode, nil
}

func (c *HTTPClient) do(ctx context.Context, path string, params url.Values, accept string) (*http.Response, error) {
	target := c.base + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("memesearch: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memesearch: GET %s: %w", path, err)
	}
	return resp, nil
}

func apiError(resp *http.Response) *APIError {
	e := &APIError{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// A proxy in front of the API may answer with HTML; then Code and Message stay empty.
	if json.Unmarshal(data, &body) == nil {
		e.Code = body.Error.Code
		e.Message = body.Error.Message
	}
	// Only the delta-seconds form: the HTTP-date form needs a trusted clock and nobody sends it here.
	if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}

func validateSearch(query string, opts SearchOptions) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("%w: empty query", ErrInvalidRequest)
	}
	// Upstream counts characters (Ruby String#length), not bytes.
	if n := utf8.RuneCountInString(query); n > MaxQueryLen {
		return fmt.Errorf("%w: query is %d characters, limit %d", ErrInvalidRequest, n, MaxQueryLen)
	}
	if opts.Limit < 1 || opts.Limit > MaxLimit {
		return fmt.Errorf("%w: limit %d outside 1..%d", ErrInvalidRequest, opts.Limit, MaxLimit)
	}
	switch opts.Mode {
	case "", ModeVector, ModeKeyword:
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidRequest, opts.Mode)
	}
	if len(opts.Tags) > MaxTags {
		return fmt.Errorf("%w: %d tags, limit %d", ErrInvalidRequest, len(opts.Tags), MaxTags)
	}
	for _, t := range opts.Tags {
		if strings.TrimSpace(t) == "" || utf8.RuneCountInString(t) > MaxTagLen {
			return fmt.Errorf("%w: tag %q is empty or longer than %d characters", ErrInvalidRequest, t, MaxTagLen)
		}
	}
	return nil
}

// Wire types use pointers so a missing required field is distinguishable from a zero value.
type wireMeme struct {
	ID          *int64    `json:"id"`
	Filename    *string   `json:"filename"`
	Description *string   `json:"description"`
	Tags        *[]string `json:"tags"`
	MediaType   *string   `json:"media_type"`
	ContentURL  *string   `json:"content_url"`
}

// validate mirrors the checks of upstream's CLI client, which the shared conformance fixtures target.
// content_url must stay a relative v1 path: an absolute one could point the token elsewhere.
func (w *wireMeme) validate() error {
	switch {
	case w.ID == nil || *w.ID < 1:
		return fmt.Errorf("%w: meme id", ErrBadResponse)
	case w.Filename == nil:
		return fmt.Errorf("%w: meme filename", ErrBadResponse)
	case w.Tags == nil:
		return fmt.Errorf("%w: meme tags", ErrBadResponse)
	case w.MediaType == nil:
		return fmt.Errorf("%w: meme media_type", ErrBadResponse)
	case w.ContentURL == nil || !contentURLRe.MatchString(*w.ContentURL):
		return fmt.Errorf("%w: meme content_url", ErrBadResponse)
	}
	return nil
}

func (w *wireMeme) meme() Meme {
	m := Meme{
		ID:         *w.ID,
		Filename:   *w.Filename,
		Tags:       *w.Tags,
		MediaType:  *w.MediaType,
		ContentURL: *w.ContentURL,
	}
	if w.Description != nil {
		m.Description = *w.Description
	}
	return m
}

type searchResponse struct {
	Data *[]wireMeme `json:"data"`
	Meta *struct {
		Query *string   `json:"query"`
		Mode  *string   `json:"mode"`
		Tags  *[]string `json:"tags"`
		Limit *int      `json:"limit"`
		Count *int      `json:"count"`
	} `json:"meta"`
}

func (r *searchResponse) validate() error {
	if r.Data == nil || r.Meta == nil {
		return fmt.Errorf("%w: missing data or meta", ErrBadResponse)
	}
	m := r.Meta
	if m.Query == nil || m.Mode == nil || m.Tags == nil || m.Limit == nil || m.Count == nil {
		return fmt.Errorf("%w: missing meta field", ErrBadResponse)
	}
	if Mode(*m.Mode) != ModeKeyword && Mode(*m.Mode) != ModeVector {
		return fmt.Errorf("%w: meta mode %q", ErrBadResponse, *m.Mode)
	}
	if *m.Limit < 1 || *m.Limit > MaxLimit || *m.Count < 0 || *m.Count > MaxLimit {
		return fmt.Errorf("%w: meta limit or count out of range", ErrBadResponse)
	}
	for i := range *r.Data {
		if err := (*r.Data)[i].validate(); err != nil {
			return err
		}
	}
	return nil
}
