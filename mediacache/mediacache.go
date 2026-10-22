// Package mediacache maps memes to Telegram file_ids so each meme is uploaded to
// Telegram at most once.
//
// Telegram can send a photo by file_id only after it has been uploaded once, and
// inline results accept nothing but a file_id or a public URL. The cache keeps a
// private cache channel as upload target, indexes the library folder (base name
// and SHA-256 per file) so API results can be matched to files without
// downloading them, and warms the channel in the background.
//
// Two paths reach Telegram. A user waiting for a private chat album gets bytes
// from Prepare and never waits for the channel; the file_ids the album send
// returns are stored with RememberPhoto. The cache channel serves only what
// has no chat to be sent to: inline misses (EnqueueFor, Enqueue, Resolve) at
// real-time priority and warmup at background priority, through one rate
// limiter in which real-time jobs always jump ahead of warmup. Uploads only a
// user's older inline query wanted are demoted to background priority.
package mediacache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/mediacache/internal/limiter"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// ErrNoCacheChat is returned by Resolve when no cache channel is configured.
var ErrNoCacheChat = errors.New("mediacache: no cache chat configured")

// Errors returned by Resolve besides ErrNoCacheChat and context errors.
//
// ErrRejected means Telegram refused the file itself (too big, bad
// dimensions); it is not retried until the library file changes, or for a
// meme without a library file, for a day. ErrStopped means Run has ended.
// ErrQueueFull means too many real-time uploads are waiting.
var (
	ErrRejected  = errors.New("mediacache: telegram rejected the file")
	ErrNotPhoto  = errors.New("mediacache: meme is not a photo")
	ErrStopped   = errors.New("mediacache: stopped")
	ErrQueueFull = errors.New("mediacache: upload queue full")
)

// Defaults applied by New for zero Config fields.
const (
	DefaultWarmupInterval      = 6 * time.Hour
	DefaultUploadsPerMin       = 20
	DefaultWarmupUploadsPerMin = 10
)

const (
	realtimeQueueCap   = 1024
	backgroundQueueCap = 1 << 16
	defaultMaxEnqueued = 64
)

// Config tunes the cache. Zero CacheChatID disables uploads to a channel and
// warmup; other zero values mean the defaults noted per field.
type Config struct {
	CacheChatID int64
	// LibraryDir is the root of the meme-search image folders. Empty, or a
	// directory that does not exist, disables the library index.
	LibraryDir          string
	WarmupInterval      time.Duration // how often the library is rescanned; 0 means 6h
	UploadsPerMin       int           // all uploads to the cache channel; 0 means 20
	WarmupUploadsPerMin int           // background cap, at most UploadsPerMin; 0 means 10 (or UploadsPerMin if lower)
	// FeedInterval is how often the server's change feed is polled, when
	// Deps.Lister is set and the server has the listing; 0 means 1m.
	FeedInterval time.Duration
}

// Validate reports every invalid field as a *configerr.Field.
func (c Config) Validate() error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, &configerr.Field{Name: field, Reason: fmt.Sprintf(format, args...)})
	}
	if c.WarmupInterval < 0 {
		bad("WarmupInterval", "must not be negative, got %s", c.WarmupInterval)
	}
	if c.UploadsPerMin < 0 {
		bad("UploadsPerMin", "must not be negative, got %d", c.UploadsPerMin)
	}
	if c.WarmupUploadsPerMin < 0 {
		bad("WarmupUploadsPerMin", "must not be negative, got %d", c.WarmupUploadsPerMin)
	} else if total := c.withDefaults().UploadsPerMin; c.UploadsPerMin >= 0 && c.WarmupUploadsPerMin > total {
		bad("WarmupUploadsPerMin", "must be at most UploadsPerMin (%d), got %d", total, c.WarmupUploadsPerMin)
	}
	if c.FeedInterval < 0 {
		bad("FeedInterval", "must not be negative, got %s", c.FeedInterval)
	}
	return errors.Join(errs...)
}

func (c Config) withDefaults() Config {
	if c.WarmupInterval == 0 {
		c.WarmupInterval = DefaultWarmupInterval
	}
	if c.UploadsPerMin == 0 {
		c.UploadsPerMin = DefaultUploadsPerMin
	}
	if c.WarmupUploadsPerMin == 0 {
		c.WarmupUploadsPerMin = min(DefaultWarmupUploadsPerMin, c.UploadsPerMin)
	}
	if c.FeedInterval == 0 {
		c.FeedInterval = DefaultFeedInterval
	}
	return c
}

// PhotoSender is the slice of the Telegram API the cache needs; *bot.Bot implements it.
type PhotoSender interface {
	SendPhoto(ctx context.Context, params *bot.SendPhotoParams) (*models.Message, error)
}

// Deps are the collaborators the host passes in.
type Deps struct {
	DB     *sql.DB
	API    memesearch.Client
	Sender PhotoSender
	Log    *slog.Logger
	Now    func() time.Time // nil means time.Now
	// Lister and Caps enable the change feed; both are optional. The feed
	// polls only while Caps reports the listing, so an older server gets no
	// extra requests.
	Lister MemeLister
	Caps   memesearch.CapabilitySource
}

// Cache is the media cache. Run must be running for uploads and warmup to happen.
type Cache struct {
	cfg    Config
	db     *sql.DB
	api    memesearch.Client
	sender PhotoSender
	log    *slog.Logger
	now    func() time.Time
	clock  limiter.Clock
	queue  *limiter.Queue[*job]

	// life outlives every caller of Resolve and ends with Run, so a shared
	// upload is neither cancelled by its first caller nor left running.
	life     context.Context
	stopLife context.CancelFunc
	started  atomic.Bool

	mu          sync.Mutex
	calls       map[int64]*call
	enqueued    int
	maxEnqueued int
	// current holds the meme ids of each user's latest EnqueueFor.
	current map[int64]map[int64]struct{}

	scanMu  sync.Mutex
	indexOn *bool // nil until the first scan; logs only on change
	scans   atomic.Int64
	rescan  chan struct{} // the feed saw files the index lacks

	lister       MemeLister
	caps         memesearch.CapabilitySource
	feedListener func(context.Context, []memesearch.Meme) // guarded by mu
	feedMu       sync.Mutex                               // one poll at a time: the cursor is shared
	feedLimit    int
	feedPages    int
}

// New validates the config and applies the cache's migrations.
func New(ctx context.Context, cfg Config, deps Deps) (*Cache, error) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return newCache(ctx, cfg, deps, realClock{now})
}

func newCache(ctx context.Context, cfg Config, deps Deps, clock limiter.Clock) (*Cache, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("mediacache: %w", err)
	}
	cfg = cfg.withDefaults()
	switch {
	case deps.DB == nil:
		return nil, errors.New("mediacache: Deps.DB is required")
	case deps.API == nil:
		return nil, errors.New("mediacache: Deps.API is required")
	case deps.Sender == nil && cfg.CacheChatID != 0:
		return nil, errors.New("mediacache: Deps.Sender is required with a cache chat")
	}
	if err := store.Check(ctx, deps.DB); err != nil {
		return nil, fmt.Errorf("mediacache: %w", err)
	}
	if err := store.Migrate(ctx, deps.DB, "mediacache", migrations); err != nil {
		return nil, fmt.Errorf("mediacache: %w", err)
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	life, stop := context.WithCancel(context.Background())
	queue := limiter.New[*job](limiter.Config{
		PerMin:           cfg.UploadsPerMin,
		BackgroundPerMin: cfg.WarmupUploadsPerMin,
		RealtimeCap:      realtimeQueueCap,
		BackgroundCap:    backgroundQueueCap,
	}, clock, log)
	queue.SetOrder(jobBefore)
	return &Cache{
		cfg:         cfg,
		db:          deps.DB,
		api:         deps.API,
		sender:      deps.Sender,
		log:         log,
		now:         now,
		clock:       clock,
		queue:       queue,
		life:        life,
		stopLife:    stop,
		calls:       map[int64]*call{},
		current:     map[int64]map[int64]struct{}{},
		maxEnqueued: defaultMaxEnqueued,
		rescan:      make(chan struct{}, 1),
		lister:      deps.Lister,
		caps:        deps.Caps,
		feedLimit:   memesearch.MaxListLimit,
		feedPages:   defaultFeedPages,
	}, nil
}

type realClock struct{ now func() time.Time }

func (r realClock) Now() time.Time { return r.now() }

func (realClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Run serves the upload queue and, when a cache chat and library dir are set,
// rescans and warms the library every WarmupInterval. It returns when ctx ends.
//
// Resolve calls made before Run starts wait for it; once Run has returned they
// fail with ErrStopped. Run may be called once.
func (c *Cache) Run(ctx context.Context) error {
	return c.run(ctx, true)
}

// run lets tests serve the queue without the scan loop, whose warmup uploads
// would race with the uploads a test asserts on.
func (c *Cache) run(ctx context.Context, scanLoop bool) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("mediacache: Run called twice")
	}
	c.log.InfoContext(ctx, "media cache started", "cache_chat_id", c.cfg.CacheChatID,
		"library_dir", c.cfg.LibraryDir, "warmup_interval", c.cfg.WarmupInterval,
		"uploads_per_min", c.cfg.UploadsPerMin, "warmup_uploads_per_min", c.cfg.WarmupUploadsPerMin,
		"feed", c.feedEnabled(), "feed_interval", c.cfg.FeedInterval)
	var wg sync.WaitGroup
	wg.Go(func() { c.work(ctx) })
	if scanLoop {
		wg.Go(func() { c.scanLoop(ctx) })
		if c.feedEnabled() {
			wg.Go(func() { c.feedLoop(ctx) })
		}
	}
	<-ctx.Done()
	for _, j := range c.queue.Close() {
		j.finish(result{err: ErrStopped})
	}
	wg.Wait()
	c.stopLife()
	c.log.InfoContext(ctx, "media cache stopped")
	return nil
}

func (c *Cache) scanLoop(ctx context.Context) {
	for {
		st, err := c.scan(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.WarnContext(ctx, "library scan failed", "err", err)
		}
		if err == nil && !st.enabled && c.cfg.CacheChatID != 0 && c.feedEnabled() {
			if err := c.queueAPIWarmup(ctx); err != nil && ctx.Err() == nil {
				c.log.WarnContext(ctx, "API warmup failed", "err", err)
			}
		}
		tick, stop := c.clock.After(c.cfg.WarmupInterval)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-tick:
		case <-c.rescan:
			stop()
			c.log.DebugContext(ctx, "library rescan requested by the feed")
		}
	}
}

// UploadsEnabled reports whether a cache channel is configured (inline mode needs it).
func (c *Cache) UploadsEnabled() bool { return c.cfg.CacheChatID != 0 }

// Lookup returns a known file_id for m without any network call: by meme id,
// or by a library file with a unique base name. Ambiguous names are a miss.
func (c *Cache) Lookup(ctx context.Context, m memesearch.Meme) (fileID string, ok bool) {
	mr, found, err := c.memeRow(ctx, m.ID)
	if err != nil {
		c.lookupErr(ctx, m, err)
		return "", false
	}
	if found && mr.fileID != "" {
		c.log.DebugContext(ctx, "cache hit by meme id", "meme_id", m.ID)
		return mr.fileID, true
	}
	rows, err := c.libraryByName(ctx, m.Filename)
	if err != nil {
		c.lookupErr(ctx, m, err)
		return "", false
	}
	if len(rows) == 1 && rows[0].fileID != "" {
		r := rows[0]
		if err := c.rememberMeme(ctx, m.ID, r.fileID, r.fileUniqueID, r.relPath); err != nil {
			c.log.WarnContext(ctx, "lookup: remember meme", "meme_id", m.ID, "err", err)
		}
		c.log.DebugContext(ctx, "cache hit by library name", "meme_id", m.ID, "rel_path", r.relPath)
		return r.fileID, true
	}
	c.log.DebugContext(ctx, "cache miss", "meme_id", m.ID, "filename", m.Filename, "library_candidates", len(rows))
	return "", false
}

func (c *Cache) lookupErr(ctx context.Context, m memesearch.Meme, err error) {
	if ctx.Err() != nil {
		c.log.DebugContext(ctx, "lookup aborted", "meme_id", m.ID, "err", err)
		return
	}
	c.log.WarnContext(ctx, "lookup failed", "meme_id", m.ID, "err", err)
}

// Resolve returns a file_id for m, uploading it to the cache channel with
// real-time priority if needed; it blocks until the upload is done or ctx ends.
// It waits behind other real-time uploads and the channel rate limit, so it is
// for inline misses, not albums: those use Prepare.
//
// Concurrent Resolve and Enqueue calls for one meme share a single upload. The
// bytes are read or downloaded only when the upload's turn comes.
func (c *Cache) Resolve(ctx context.Context, m memesearch.Meme) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := notPhoto(m); err != nil {
		return "", err
	}
	if id, ok := c.Lookup(ctx, m); ok {
		return id, nil
	}
	if c.cfg.CacheChatID == 0 {
		return "", ErrNoCacheChat
	}
	if c.life.Err() != nil {
		return "", ErrStopped
	}
	cl := c.join(m, false)
	c.pin(cl)
	select {
	case <-cl.done:
		return cl.fileID, cl.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Enqueue resolves memes asynchronously with real-time priority (inline misses:
// the user is waiting for the next query, not for this one).
//
// At most a fixed number of enqueued memes are in flight; more are dropped,
// as the next query enqueues them again.
func (c *Cache) Enqueue(ms ...memesearch.Meme) {
	c.enqueue(0, false, ms)
}

func (c *Cache) enqueue(userID int64, owned bool, ms []memesearch.Meme) {
	if c.cfg.CacheChatID == 0 || c.life.Err() != nil {
		c.log.Debug("enqueue ignored", "count", len(ms), "cache_chat", c.cfg.CacheChatID != 0)
		return
	}
	want := map[int64]struct{}{}
	for _, m := range ms {
		if err := notPhoto(m); err != nil {
			c.log.Debug("enqueue skipped", "meme_id", m.ID, "err", err)
			continue
		}
		if _, ok := c.Lookup(c.life, m); ok {
			continue
		}
		cl := c.join(m, true)
		if cl == nil {
			c.log.Debug("enqueue dropped: too many in flight", "meme_id", m.ID, "max", c.maxEnqueued)
			continue
		}
		if !owned {
			c.pin(cl)
		} else {
			c.mu.Lock()
			// A call that already finished must not be recorded: its cleanup
			// has run and would never remove the entry.
			if c.calls[m.ID] == cl {
				if cl.owners == nil {
					cl.owners = map[int64]struct{}{}
				}
				cl.owners[userID] = struct{}{}
				want[m.ID] = struct{}{}
				c.promote(cl)
			}
			c.mu.Unlock()
		}
		c.log.Debug("enqueued", "meme_id", m.ID, "user_id", userID)
		go func() {
			<-cl.done
			if cl.err != nil {
				c.log.Debug("enqueued meme not cached", "meme_id", m.ID, "err", cl.err)
			}
		}()
	}
	if owned {
		c.replaceCurrent(userID, want)
	}
}

// replaceCurrent makes want the user's current query and demotes what only
// the previous one wanted.
func (c *Cache) replaceCurrent(userID int64, want map[int64]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	demoted := 0
	for id := range c.current[userID] {
		if _, still := want[id]; still {
			continue
		}
		cl := c.calls[id]
		if cl == nil {
			continue
		}
		delete(cl.owners, userID)
		if !cl.demoted {
			c.demote(cl)
			if cl.demoted {
				demoted++
			}
		}
	}
	if len(want) == 0 {
		delete(c.current, userID)
	} else {
		c.current[userID] = want
	}
	c.log.Debug("inline query uploads updated", "user_id", userID, "wanted", len(want), "demoted", demoted)
}

// EnqueueFor is Enqueue for the misses of one user's inline query. The
// user's earlier query is over: its uploads that no other user's current
// query, Resolve caller or anonymous Enqueue wants are demoted to background
// priority (not dropped), so they stop delaying the query the user sees now.
// A demoted upload wanted again by a current query goes back to real time.
func (c *Cache) EnqueueFor(userID int64, ms ...memesearch.Meme) {
	c.enqueue(userID, true, ms)
}

func (c *Cache) pin(cl *call) {
	c.mu.Lock()
	cl.pinned = true
	c.promote(cl)
	c.mu.Unlock()
}

// Remember records the file_id Telegram returned when m was sent elsewhere.
//
// After sending a Photo from Prepare, use RememberPhoto instead: it also
// records the library file the bytes came from.
func (c *Cache) Remember(ctx context.Context, memeID int64, fileID, fileUniqueID string) error {
	if fileID == "" {
		return errors.New("mediacache: remember: empty file_id")
	}
	if err := c.rememberMeme(ctx, memeID, fileID, fileUniqueID, ""); err != nil {
		return err
	}
	c.log.DebugContext(ctx, "remembered file_id", "meme_id", memeID)
	return nil
}

// KnownUniqueID reports whether a Telegram file_unique_id belongs to a cached meme.
func (c *Cache) KnownUniqueID(ctx context.Context, fileUniqueID string) (bool, error) {
	if fileUniqueID == "" {
		return false, nil
	}
	return c.knownUniqueID(ctx, fileUniqueID)
}

// KnownSHA256 reports whether a file with this content exists in the library index.
func (c *Cache) KnownSHA256(ctx context.Context, sum [32]byte) (bool, error) {
	return c.knownSHA256(ctx, sum)
}
