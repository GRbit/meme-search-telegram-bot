// Package memesearch is a client for meme-search Search API v1.
//
// The contract lives upstream in docs/search-api-openapi.yml. The API is
// read-only and additive: unknown response fields must be ignored.
package memesearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Limits enforced by the API; requests outside them get 422.
const (
	MaxQueryLen = 200
	MaxLimit    = 20
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
var (
	ErrUnauthorized   = errors.New("memesearch: unauthorized")
	ErrForbidden      = errors.New("memesearch: forbidden")
	ErrNotFound       = errors.New("memesearch: not found")
	ErrInvalidRequest = errors.New("memesearch: invalid request")
	ErrRateLimited    = errors.New("memesearch: rate limited")
)

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status     int
	Code       string // error.code from the body, if any
	Message    string // error.message from the body, if any
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("memesearch: http %d %s: %s", e.Status, e.Code, e.Message)
}

// HTTPClient implements Client over HTTP.
type HTTPClient struct{}

var _ Client = (*HTTPClient)(nil)

// New builds an HTTP client. A nil httpClient means a default client with cfg.Timeout.
func New(cfg Config, httpClient *http.Client, log *slog.Logger) (*HTTPClient, error) {
	return nil, errors.New("memesearch: not implemented")
}

// Search runs a search query.
func (c *HTTPClient) Search(ctx context.Context, query string, opts SearchOptions) ([]Meme, error) {
	return nil, errors.New("memesearch: not implemented")
}

// Meme fetches one meme's metadata.
func (c *HTTPClient) Meme(ctx context.Context, id int64) (Meme, error) {
	return Meme{}, errors.New("memesearch: not implemented")
}

// Content fetches one meme's original bytes.
func (c *HTTPClient) Content(ctx context.Context, id int64) (Content, error) {
	return Content{}, errors.New("memesearch: not implemented")
}
