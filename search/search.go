// Package search is the module that answers text messages, inline queries and
// paging callbacks with memes found through Search API v1.
package search

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/module"
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
	return errors.Join(errs...)
}

const (
	maxAlbum              = 10 // Telegram media group limit
	defaultResultsPerPage = 5
	defaultPageTTL        = time.Hour
)

// MediaCache is what the module needs from mediacache.Cache; mediacache.Direct
// implements it for hosts without a media cache.
//
// How a meme reaches Telegram (file_id, upload to the cache channel, library
// bytes, API bytes) is the cache's decision: albums use Prepare and report
// uploaded bytes back with Remember, inline uses Lookup and Enqueue, and
// UploadsEnabled only decides whether inline mode is registered.
type MediaCache interface {
	UploadsEnabled() bool
	Lookup(ctx context.Context, m memesearch.Meme) (fileID string, ok bool)
	Enqueue(ms ...memesearch.Meme)
	Prepare(ctx context.Context, m memesearch.Meme) (mediacache.Photo, error)
	Remember(ctx context.Context, memeID int64, fileID, fileUniqueID string) error
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
}

// Module implements module.Module.
type Module struct {
	cfg   Config
	deps  Deps
	log   *slog.Logger
	now   func() time.Time
	pages *pageStore
	me    botName
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
		return nil, errors.New("search: Deps.Media is nil (use mediacache.NewDirect(api) to run without a media cache)")
	case deps.I18n == nil:
		return nil, errors.New("search: Deps.I18n is nil")
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
	}, nil
}

// Register attaches the module's handlers.
//
// The inline handler exists only with uploads enabled: without a cache channel
// most results would have no file_id and the inline grid would stay empty.
// Commands skip Deps.Middlewares because they never run a search.
func (m *Module) Register(b *bot.Bot) {
	mw := m.deps.Middlewares
	b.RegisterHandlerMatchFunc(isTextSearch, m.handleText, mw...)
	b.RegisterHandlerMatchFunc(isCommand, m.handleCommand)
	b.RegisterHandlerMatchFunc(isOwnCallback, m.handleCallback, mw...)
	inline := m.deps.Media.UploadsEnabled()
	if inline {
		b.RegisterHandlerMatchFunc(isInline, m.handleInline, mw...)
	}
	m.log.Info("search module registered", "inline", inline, "results_per_page", m.cfg.ResultsPerPage,
		"mode", string(m.cfg.Mode), "page_ttl", m.cfg.PageTTL)
}

// Chargeable reports whether an update runs a new search and therefore counts
// against access limits: text searches, inline queries and "similar" callbacks,
// but not paging. Hosts pass it to access.Access.Middleware.
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
	case isOwnCallback(u):
		return strings.HasPrefix(u.CallbackQuery.Data, similarPrefix)
	}
	return false
}
