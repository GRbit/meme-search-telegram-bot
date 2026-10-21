// Package search is the module that answers text messages, inline queries and
// paging callbacks with memes found through Search API v1.
package search

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/module"
	"github.com/grbit/meme-search-telegram-bot/store"
)

//go:embed locales/*.yaml
var locales embed.FS

// LocaleFS returns this package's message files for i18n.NewBundle.
func LocaleFS() fs.FS {
	sub, err := fs.Sub(locales, "locales")
	if err != nil {
		panic(err) // embedded path is fixed at compile time
	}
	return sub
}

// CallbackPrefix starts the callback data of every button this module creates.
const CallbackPrefix = "srch:"

// Config tunes the module. Zero values mean the defaults noted per field, so a
// host only sets what it exposes. Inline mode is on when Deps.Media reports
// UploadsEnabled: inline results need file_ids from the cache channel.
type Config struct {
	ResultsPerPage int             // 1..10 (Telegram album limit); 0 means 5
	Mode           memesearch.Mode // "" means memesearch.ModeVector
	PageTTL        time.Duration   // how long fetched results stay pageable; 0 means 1h
	// InlineResults is how many memes one inline query asks for, 1..memesearch.MaxLimit (the API's cap); 0
	// means 3. Every miss among them is queued for upload, so a small number keeps the queue short.
	InlineResults  int
	InlineDebounce time.Duration
}

// Validate reports every invalid field as a *configerr.Field.
func (c Config) Validate() error {
	var errs []error
	if c.ResultsPerPage < 0 || c.ResultsPerPage > maxAlbum {
		errs = append(errs, &configerr.Field{Name: "ResultsPerPage", Reason: fmt.Sprintf("must be 1..%d (0 means %d)", maxAlbum, defaultResultsPerPage)})
	}
	switch c.Mode {
	case "", memesearch.ModeVector, memesearch.ModeKeyword:
	default:
		errs = append(errs, &configerr.Field{Name: "Mode", Reason: fmt.Sprintf("must be %q or %q, got %q", memesearch.ModeVector, memesearch.ModeKeyword, c.Mode)})
	}
	if c.PageTTL < 0 {
		errs = append(errs, &configerr.Field{Name: "PageTTL", Reason: "must not be negative"})
	}
	if c.InlineDebounce < 0 || c.InlineDebounce > maxInlineDebounce {
		errs = append(errs, &configerr.Field{Name: "InlineDebounce", Reason: fmt.Sprintf("must be 0..%s (0 disables it)", maxInlineDebounce)})
	}
	if c.InlineResults < 0 || c.InlineResults > memesearch.MaxLimit {
		errs = append(errs, &configerr.Field{Name: "InlineResults", Reason: fmt.Sprintf("must be 1..%d (0 means %d)", memesearch.MaxLimit, defaultInlineResults)})
	}
	return errors.Join(errs...)
}

const (
	maxAlbum              = 10 // Telegram media group limit
	defaultResultsPerPage = 5
	defaultPageTTL        = time.Hour
	defaultInlineResults  = 3
	// maxInlineDebounce leaves room for the search and the answer: Telegram
	// expires an inline query about 10s after it was sent.
	maxInlineDebounce = 5 * time.Second
)

// MediaCache is what the module needs from mediacache.Cache; mediacache.Direct
// implements it for hosts without a media cache.
//
// How a meme reaches Telegram (file_id, upload to the cache channel, library
// bytes, API bytes) is the cache's decision: albums use Prepare and report
// uploaded bytes back with Remember, inline uses Lookup and EnqueueFor, and
// UploadsEnabled only decides whether inline mode is registered.
type MediaCache interface {
	UploadsEnabled() bool
	Lookup(ctx context.Context, m memesearch.Meme) (fileID string, ok bool)
	EnqueueFor(userID int64, ms ...memesearch.Meme)
	Prepare(ctx context.Context, m memesearch.Meme) (mediacache.Photo, error)
	RememberPhoto(ctx context.Context, memeID int64, p mediacache.Photo, fileID, fileUniqueID string) error
}

var (
	_ MediaCache = (*mediacache.Cache)(nil)
	_ MediaCache = (*mediacache.Direct)(nil)
)

// Deps are the collaborators the host passes in.
type Deps struct {
	API   memesearch.Client
	Media MediaCache
	I18n  *i18n.Bundle
	Log   *slog.Logger
	// Middlewares wrap every handler that may run a search (access limits).
	Middlewares []bot.Middleware
	Now         func() time.Time // nil means time.Now
	// DB holds the search_ tables (user settings); required.
	DB *sql.DB
}

// Module implements module.Module.
type Module struct {
	cfg    Config
	deps   Deps
	log    *slog.Logger
	now    func() time.Time
	pages  *pageStore
	me     botName
	after  func(time.Duration) (<-chan time.Time, func())
	inline latestQueries
	inputs pendingInputs
}

var _ module.Module = (*Module)(nil)

// New validates the config and applies defaults to its zero fields.
func New(cfg Config, deps Deps) (*Module, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	switch {
	case deps.API == nil:
		return nil, errors.New("search: Deps.API is nil")
	case deps.Media == nil:
		return nil, errors.New("search: Deps.Media is nil (use mediacache.NewDirect(api, log) to run without a media cache)")
	case deps.I18n == nil:
		return nil, errors.New("search: Deps.I18n is nil")
	case deps.DB == nil:
		return nil, errors.New("search: Deps.DB is nil")
	}
	// New takes no context: the migration is a local SQLite step at startup.
	ctx := context.Background()
	if err := store.Check(ctx, deps.DB); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if err := store.Migrate(ctx, deps.DB, "search", migrations); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if cfg.ResultsPerPage == 0 {
		cfg.ResultsPerPage = defaultResultsPerPage
	}
	if cfg.Mode == "" {
		cfg.Mode = memesearch.ModeVector
	}
	if cfg.PageTTL == 0 {
		cfg.PageTTL = defaultPageTTL
	}
	if cfg.InlineResults == 0 {
		cfg.InlineResults = defaultInlineResults
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Module{
		cfg:   cfg,
		deps:  deps,
		log:   log,
		now:   now,
		pages: newPageStore(cfg.PageTTL, maxSessions),
		after: realAfter,
	}, nil
}

// Register attaches the module's handlers.
//
// The inline handler exists only with uploads enabled: without a cache channel
// most results would have no file_id and the inline grid would stay empty.
// Commands skip Deps.Middlewares because they never run a search.
func (m *Module) Register(b *bot.Bot) {
	mw := m.deps.Middlewares
	// First: a tag list typed after "Set tags" is not a search and not charged.
	b.RegisterHandlerMatchFunc(m.isPendingInput, m.handlePendingInput)
	b.RegisterHandlerMatchFunc(isTextSearch, m.handleText, mw...)
	b.RegisterHandlerMatchFunc(isCommand, m.handleCommand)
	b.RegisterHandlerMatchFunc(isSettingsCommand, m.handleSettingsCommand)
	b.RegisterHandlerMatchFunc(isOwnCallback, m.handleCallback, mw...)
	inline := m.deps.Media.UploadsEnabled()
	if inline {
		b.RegisterHandlerMatchFunc(isInline, m.handleInline, mw...)
	}
	m.log.Info("search module registered", "inline", inline, "results_per_page", m.cfg.ResultsPerPage,
		"mode", string(m.cfg.Mode), "page_ttl", m.cfg.PageTTL, "inline_results", m.cfg.InlineResults,
		"inline_debounce", m.cfg.InlineDebounce)
}

// Chargeable reports whether an update runs a new search and therefore counts
// against access limits: text searches and inline queries, but not paging.
// Hosts pass it to access.Access.Middleware. "similar" callbacks will count once
// they run a search; until then a press is a no-op and must not burn quota.
//
// It mirrors the handlers' own checks, so blank or over-long queries, which
// never reach the API, are not charged.
func Chargeable(u *models.Update) bool {
	switch {
	case isTextSearch(u):
		_, ok := searchQuery(u.Message.Text)
		return ok
	case isInline(u):
		_, ok := searchQuery(u.InlineQuery.Query)
		return ok
	}
	return false
}
