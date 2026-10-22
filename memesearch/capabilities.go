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
	Upload       bool // POST /api/v2/memes, and the API token has media:write
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
	// summarized is set once a probe decided every capability and said so at Info.
	summarized bool
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

// observeEndpoint records what a call to an optional endpoint revealed and
// reports whether it decided the capability. Errors that say nothing about the
// route (5xx, 429, 401, network) keep the old value, so a hiccup never hides a
// feature.
func (c *HTTPClient) observeEndpoint(ctx context.Context, name string, field func(*Capabilities) *bool, err error) bool {
	switch {
	case err == nil:
		c.setCap(ctx, name, field, true)
	case errors.Is(err, ErrUnsupported):
		c.setCap(ctx, name, field, false)
	default:
		c.log.DebugContext(ctx, "memesearch capability unchanged", "capability", name, "err", err)
		return false
	}
	return true
}

// Probe checks every capability now, caches and returns the result.
//
// It costs one request on each optional endpoint's own rate limit bucket and
// two on the search bucket (one search, one similar), so run it at startup and
// every few hours, not per user action. The upload check posts an empty form,
// which the server rejects before storing anything; 403 means the token lacks
// media:write.
func (c *HTTPClient) Probe(ctx context.Context) Capabilities {
	got, _ := c.probe(ctx)
	return got
}

// probe is Probe that also reports whether every capability was decided; an
// undecided one keeps its old value (false before the first answer).
func (c *HTTPClient) probe(ctx context.Context) (Capabilities, bool) {
	c.log.DebugContext(ctx, "memesearch probe start")
	_, err := c.Folders(ctx)
	decided := c.observeEndpoint(ctx, "folder_list", func(p *Capabilities) *bool { return &p.FolderList }, err)
	_, err = c.Tags(ctx)
	decided = c.observeEndpoint(ctx, "tag_list", func(p *Capabilities) *bool { return &p.TagList }, err) && decided

	// Any query does: a current server echoes every newer parameter in meta even when unused.
	similarID := int64(1)
	page, err := c.SearchPage(ctx, "a", SearchOptions{Mode: ModeKeyword, Limit: 1})
	if err == nil && len(page.Memes) > 0 {
		similarID = page.Memes[0].ID
	}
	// Only an answered search tells the search parameters apart.
	decided = decided && err == nil
	_, err = c.ListMemes(ctx, ListOptions{Limit: 1})
	decided = c.observeEndpoint(ctx, "listing", func(p *Capabilities) *bool { return &p.Listing }, err) && decided
	// Similar records the capability itself; a missing meme (JSON 404) still proves the route.
	_, err = c.Similar(ctx, similarID, 1)
	if ev := similarEvidence(err); ev != nil && !errors.Is(ev, ErrUnsupported) {
		decided = false
	}
	decided = c.probeUpload(ctx) && decided

	c.caps.mu.Lock()
	c.caps.caps.CheckedAt = c.now()
	got := c.caps.caps
	summary := decided && !c.caps.summarized
	if summary {
		c.caps.summarized = true
	}
	c.caps.mu.Unlock()
	switch {
	case summary:
		// Flips are logged one by one; on an older server nothing flips, so say it once.
		c.log.InfoContext(ctx, "memesearch capabilities", "folder_list", got.FolderList, "tag_list", got.TagList,
			"folder_filter", got.FolderFilter, "tag_match", got.TagMatch, "offset", got.Offset,
			"listing", got.Listing, "similar", got.Similar, "upload", got.Upload)
	case !decided:
		// Typical right after start when meme-search is still booting; the
		// features it lacks stay off until a probe gets through.
		c.log.WarnContext(ctx, "memesearch capabilities could not be checked yet, will retry", "capabilities", got)
	}
	c.log.DebugContext(ctx, "memesearch probe done", "capabilities", got, "decided", decided)
	return got, decided
}

func (c *HTTPClient) probeUpload(ctx context.Context) bool {
	field := func(p *Capabilities) *bool { return &p.Upload }
	_, err := c.upload(ctx, nil, UploadOptions{})
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity:
		// The empty form passed auth and scope and failed only file validation.
		c.setCap(ctx, "upload", field, true)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden:
		// A normal setup for a bot that only searches, so not a warning.
		c.log.InfoContext(ctx, "memesearch uploads off: the API token lacks the media:write scope")
		c.setCap(ctx, "upload", field, false)
	default:
		return c.observeEndpoint(ctx, "upload", field, err)
	}
	return true
}

// DefaultProbeInterval applies when WatchCapabilities gets a non-positive interval.
const DefaultProbeInterval = 6 * time.Hour

// firstRetry is the wait after a probe that could not decide everything,
// doubled on every further such probe up to the normal interval.
const firstRetry = time.Minute

// WatchCapabilities probes now and then every interval until ctx ends.
//
// A probe that leaves a capability undecided (server down or still booting,
// 5xx, 429, 401) is retried after 1m, 2m, 4m and so on up to interval: a bot
// started next to a booting meme-search would otherwise run without its
// features for a whole interval. A decided probe restores the normal schedule.
//
// Downgrades and upgrades of search parameters are also seen between probes,
// from every search answer; the interval matters for the other endpoints.
func (c *HTTPClient) WatchCapabilities(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	retry := firstRetry
	for {
		_, decided := c.probe(ctx)
		wait := interval
		if decided {
			retry = firstRetry
		} else {
			wait = min(retry, interval)
			retry = min(retry*2, interval)
			c.log.DebugContext(ctx, "memesearch probe retry scheduled", "in", wait)
		}
		tick, stop := c.after(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-tick:
		}
	}
}

func afterTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
