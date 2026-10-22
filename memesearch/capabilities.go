package memesearch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Capabilities says which features beyond stock API v1 the server has.
//
// The zero value is a stock server: callers keep the old behaviour for every
// false field, so a bot that has not probed yet behaves as before.
type Capabilities struct {
	FolderList   bool // GET /api/v1/folders
	TagList      bool // GET /api/v1/tags
	FolderFilter bool // search applies folder (meta.folders)
	TagMatch     bool // search applies tag_match (meta.tag_match)
	Offset       bool // search applies offset (meta.offset)
	Listing      bool // GET /api/v1/memes cursor listing and change feed
	Similar      bool // GET /api/v1/memes/:id/similar
	Upload       bool // POST /api/v2/memes, and the upload token may use it
	CheckedAt    time.Time
}

// CapabilitySource reports the last known Capabilities without blocking.
type CapabilitySource interface {
	Capabilities() Capabilities
}

// StaticCapabilities is a CapabilitySource that never changes, for hosts and
// tests that know the server.
type StaticCapabilities Capabilities

// Capabilities returns c.
func (c StaticCapabilities) Capabilities() Capabilities { return Capabilities(c) }

var _ CapabilitySource = (*HTTPClient)(nil)

type capState struct {
	mu   sync.Mutex
	caps Capabilities
}

// Capabilities returns the cached result of the last Probe, updated by every
// answer that reveals a capability (a search meta, a 404 from a new endpoint).
func (c *HTTPClient) Capabilities() Capabilities {
	c.caps.mu.Lock()
	defer c.caps.mu.Unlock()
	return c.caps.caps
}

// setCap stores one capability, logging at Info only when it flips: the server
// was upgraded or downgraded under the running bot.
func (c *HTTPClient) setCap(ctx context.Context, name string, field func(*Capabilities) *bool, v bool) {
	c.caps.mu.Lock()
	p := field(&c.caps.caps)
	old := *p
	*p = v
	c.caps.mu.Unlock()
	if old != v {
		c.log.InfoContext(ctx, "memesearch capability changed", "capability", name, "available", v)
		return
	}
	c.log.DebugContext(ctx, "memesearch capability", "capability", name, "available", v)
}

func (c *HTTPClient) observeSearch(ctx context.Context, folders, tagMatch, offset bool) {
	c.setCap(ctx, "folder_filter", func(p *Capabilities) *bool { return &p.FolderFilter }, folders)
	c.setCap(ctx, "tag_match", func(p *Capabilities) *bool { return &p.TagMatch }, tagMatch)
	c.setCap(ctx, "offset", func(p *Capabilities) *bool { return &p.Offset }, offset)
}

// observeEndpoint records what a call to an optional endpoint revealed. Errors
// that say nothing about the route (5xx, 429, 401, network) keep the old value,
// so a hiccup never hides a feature.
func (c *HTTPClient) observeEndpoint(ctx context.Context, name string, field func(*Capabilities) *bool, err error) {
	switch {
	case err == nil:
		c.setCap(ctx, name, field, true)
	case errors.Is(err, ErrUnsupported):
		c.setCap(ctx, name, field, false)
	default:
		c.log.DebugContext(ctx, "memesearch capability unchanged", "capability", name, "err", err)
	}
}

// Probe checks every capability now, caches and returns the result.
//
// It costs one request on each optional endpoint's own rate limit bucket and
// two on the search bucket (one search, one similar), so run it at startup and
// every few hours, not per user action. The upload check posts an empty form,
// which the server rejects before storing anything.
func (c *HTTPClient) Probe(ctx context.Context) Capabilities {
	c.log.DebugContext(ctx, "memesearch probe start")
	_, err := c.Folders(ctx)
	c.observeEndpoint(ctx, "folder_list", func(p *Capabilities) *bool { return &p.FolderList }, err)
	_, err = c.Tags(ctx)
	c.observeEndpoint(ctx, "tag_list", func(p *Capabilities) *bool { return &p.TagList }, err)

	// Any query does: a current server echoes every newer parameter in meta even when unused.
	similarID := int64(1)
	if page, err := c.SearchPage(ctx, "a", SearchOptions{Mode: ModeKeyword, Limit: 1}); err == nil && len(page.Memes) > 0 {
		similarID = page.Memes[0].ID
	}
	_, err = c.ListMemes(ctx, ListOptions{Limit: 1})
	c.observeEndpoint(ctx, "listing", func(p *Capabilities) *bool { return &p.Listing }, err)
	// Similar records the capability itself; a missing meme (JSON 404) still proves the route.
	_, _ = c.Similar(ctx, similarID, 1)
	c.probeUpload(ctx)

	c.caps.mu.Lock()
	first := c.caps.caps.CheckedAt.IsZero()
	c.caps.caps.CheckedAt = c.now()
	got := c.caps.caps
	c.caps.mu.Unlock()
	if first {
		// Flips are logged one by one; on an older server nothing flips, so say it once.
		c.log.InfoContext(ctx, "memesearch capabilities", "folder_list", got.FolderList, "tag_list", got.TagList,
			"folder_filter", got.FolderFilter, "tag_match", got.TagMatch, "offset", got.Offset,
			"listing", got.Listing, "similar", got.Similar, "upload", got.Upload)
	}
	c.log.DebugContext(ctx, "memesearch probe done", "capabilities", got)
	return got
}

func (c *HTTPClient) probeUpload(ctx context.Context) {
	field := func(p *Capabilities) *bool { return &p.Upload }
	if c.uploadToken == "" {
		c.setCap(ctx, "upload", field, false)
		return
	}
	_, err := c.upload(ctx, nil, UploadOptions{})
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity:
		// The empty form passed auth and scope and failed only file validation.
		c.setCap(ctx, "upload", field, true)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden:
		c.log.WarnContext(ctx, "memesearch upload token lacks the media:write scope")
		c.setCap(ctx, "upload", field, false)
	default:
		c.observeEndpoint(ctx, "upload", field, err)
	}
}

// DefaultProbeInterval applies when WatchCapabilities gets a non-positive interval.
const DefaultProbeInterval = 6 * time.Hour

// WatchCapabilities probes now and then every interval until ctx ends.
//
// Downgrades and upgrades of search parameters are also seen between probes,
// from every search answer; the interval matters for the other endpoints.
func (c *HTTPClient) WatchCapabilities(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	c.Probe(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Probe(ctx)
		}
	}
}
