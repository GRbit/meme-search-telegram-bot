// Package search is the module that answers text messages, inline queries and
// paging callbacks with memes found through Search API v1.
package search

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
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

// Config tunes the module.
type Config struct {
	ResultsPerPage int
	Mode           memesearch.Mode
	PageTTL        time.Duration // how long fetched results stay pageable
	InlineEnabled  bool          // the host enables it only with a cache channel
}

// MediaCache is what the module needs from mediacache.Cache.
type MediaCache interface {
	UploadsEnabled() bool
	Lookup(ctx context.Context, m memesearch.Meme) (fileID string, ok bool)
	Resolve(ctx context.Context, m memesearch.Meme) (string, error)
	Enqueue(ms ...memesearch.Meme)
	Remember(ctx context.Context, memeID int64, fileID, fileUniqueID string) error
}

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
type Module struct{}

var _ module.Module = (*Module)(nil)

// New validates the config.
func New(cfg Config, deps Deps) (*Module, error) {
	return nil, errors.New("search: not implemented")
}

// Register attaches the module's handlers.
func (m *Module) Register(b *bot.Bot) {}

// Chargeable reports whether an update runs a new search and therefore counts
// against access limits: text searches, inline queries and "similar" callbacks,
// but not paging. Hosts pass it to access.Access.Middleware.
func Chargeable(u *models.Update) bool { return false }
