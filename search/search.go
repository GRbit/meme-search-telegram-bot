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
	"strings"
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
	// ResultsMemoryLimit caps the memes kept for paging over all searches, at least
	// memesearch.MaxLimit (one fetch); 0 means 20000. The least recently used
	// search goes first; one without results counts as one meme. A stored meme
	// with its description takes roughly 0.5-1 KB.
	ResultsMemoryLimit int
	// InlineResults is how many memes one inline query asks for, 1..memesearch.MaxLimit (the API's cap); 0
	// means 3. Every miss among them is queued for upload, so a small number keeps the queue short.
	InlineResults  int
	InlineDebounce time.Duration
	// InlineGroupScope is the default scope of inline queries sent from groups,
	// written like query filters: "#tag #other dir:folder". It applies only
	// when neither the query nor the user's settings name tags (or folders).
	InlineGroupScope string
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
	if c.ResultsMemoryLimit < 0 || (c.ResultsMemoryLimit > 0 && c.ResultsMemoryLimit < memesearch.MaxLimit) {
		// Below one fetch every new search would evict all others and could not hold itself.
		errs = append(errs, &configerr.Field{Name: "ResultsMemoryLimit",
			Reason: fmt.Sprintf("must be at least %d, one fetch of results (0 means %d)", memesearch.MaxLimit, maxStoredMemes)})
	}
	if c.PageTTL < 0 {
		errs = append(errs, &configerr.Field{Name: "PageTTL", Reason: "must not be negative"})
	}
	if c.InlineDebounce < 0 || c.InlineDebounce > maxInlineDebounce {
		errs = append(errs, &configerr.Field{Name: "InlineDebounce", Reason: fmt.Sprintf("must be 0..%s (0 disables it)", maxInlineDebounce)})
	}
	if o := parseOverrides(c.InlineGroupScope); o.query != "" {
		errs = append(errs, &configerr.Field{Name: "InlineGroupScope",
			Reason: fmt.Sprintf("must hold only #tag and dir:folder filters, found %q", o.query)})
	} else {
		switch o.limitProblem() {
		case tagsTooMany, tagsTooLong:
			errs = append(errs, &configerr.Field{Name: "InlineGroupScope",
				Reason: fmt.Sprintf("at most %d tags of at most %d characters", memesearch.MaxTags, memesearch.MaxTagLen)})
		case foldersTooMany:
			errs = append(errs, &configerr.Field{Name: "InlineGroupScope",
				Reason: fmt.Sprintf("at most %d folders of at most %d characters", memesearch.MaxFolders, memesearch.MaxFolderLen)})
		}
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

// Server is the part of the meme-search API that only newer servers have;
// *memesearch.HTTPClient implements it. The module checks Capabilities before
// offering a feature and trusts a search filter only when Page.Ignored says
// the server applied it.
type Server interface {
	memesearch.CapabilitySource
	SearchPage(ctx context.Context, q string, opts memesearch.SearchOptions) (memesearch.Page, error)
	Folders(ctx context.Context) ([]memesearch.Folder, error)
	Tags(ctx context.Context) ([]memesearch.Tag, error)
	Similar(ctx context.Context, id int64, limit int) ([]memesearch.Meme, error)
}

var _ Server = (*memesearch.HTTPClient)(nil)

// Deps are the collaborators the host passes in.
type Deps struct {
	API memesearch.Client
	// Server is optional; nil means Deps.API when it implements Server, else a
	// stock server, where the module behaves as before part 2.
	Server Server
	// OwnFolder returns the library folder the user's saved memes go to, or ""
	// when the user cannot save. Optional: nil hides the "Only mine" shortcut.
	OwnFolder func(u *models.User) string
	// Trusted reports whether a user may page past the first memesearch.MaxLimit
	// results (offset fetches) and use /settings. Optional: nil means everybody may.
	//
	// Every fetch is an API request on the shared per-token limit, and paging is
	// not charged, so a host with a free tier keeps it to one request per search.
	// Settings are the owner's choice for trusted users only; a public user's
	// saved settings are ignored, not deleted, so they return with trust.
	Trusted func(userID int64) bool
	Media   MediaCache
	I18n    *i18n.Bundle
	Log     *slog.Logger
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
	srv    Server // nil on a stock server
	// folderList caches the server's folders for pickers and dir: checks.
	folderList listCache[memesearch.Folder]
	tagList    listCache[memesearch.Tag]
	// groupDefault is Config.InlineGroupScope parsed once.
	groupDefault overrides
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
	if cfg.ResultsMemoryLimit == 0 {
		cfg.ResultsMemoryLimit = maxStoredMemes
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
	srv := deps.Server
	if srv == nil {
		srv, _ = deps.API.(Server)
	}
	return &Module{
		srv:   srv,
		cfg:   cfg,
		deps:  deps,
		log:   log,
		now:   now,
		pages: newPageStore(cfg.PageTTL, cfg.ResultsMemoryLimit),
		after: realAfter,

		groupDefault: parseOverrides(cfg.InlineGroupScope),
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
	// Ahead of the host's middlewares, so an unknown folder is refused before it is charged.
	textMW := append([]bot.Middleware{m.refuseUnknownFolders}, mw...)
	b.RegisterHandlerMatchFunc(isTextSearch, m.handleText, textMW...)
	b.RegisterHandlerMatchFunc(isCommand, m.handleCommand)
	b.RegisterHandlerMatchFunc(isSettingsCommand, m.handleSettingsCommand)
	// Group admin commands and membership changes never search: no middleware.
	b.RegisterHandlerMatchFunc(isGroupScopeCommand, m.handleGroupScope)
	b.RegisterHandlerMatchFunc(isBotRemoved, m.handleBotRemoved)
	b.RegisterHandlerMatchFunc(isOwnCallback, m.handleCallback, mw...)
	inline := m.deps.Media.UploadsEnabled()
	if inline {
		b.RegisterHandlerMatchFunc(isInline, m.handleInline, mw...)
	}
	m.log.Info("search module registered", "inline", inline, "results_per_page", m.cfg.ResultsPerPage,
		"mode", string(m.cfg.Mode), "page_ttl", m.cfg.PageTTL, "inline_results", m.cfg.InlineResults,
		"inline_debounce", m.cfg.InlineDebounce, "inline_group_scope", m.cfg.InlineGroupScope)
}

// Chargeable reports whether an update runs a new search and therefore counts
// against access limits: text searches, inline queries, "Similar" and the
// "Search everywhere" rerun, but not paging or settings. Hosts pass it to
// access.Access.Middleware.
//
// It calls the handlers' own rule (overrides.searchable), so blank or
// over-long queries and filters over the API limits, which never reach the
// API, are not charged.
func Chargeable(u *models.Update) bool {
	switch {
	case isTextSearch(u):
		_, ok := parseOverrides(u.Message.Text).searchable()
		return ok
	case isInline(u):
		_, ok := parseOverrides(u.InlineQuery.Query).searchable()
		return ok
	case u.CallbackQuery != nil:
		d := u.CallbackQuery.Data
		return strings.HasPrefix(d, everywherePrefix) || strings.HasPrefix(d, similarPrefix)
	}
	return false
}
