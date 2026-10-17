// Package saver is the module that stores photos sent to the bot as files in a
// meme-search library folder.
package saver

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
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

// Config tunes the module. Zero values mean the defaults noted per field.
type Config struct {
	Dir           string         // library root, required; per-sender folders are created inside
	DirPrefix     string         // folder name prefix, e.g. "tg-"; may be empty
	Note          string         // appended to the "saved" reply
	AlbumDebounce time.Duration  // wait after the last album item before replying; 0 means 2s
	Location      *time.Location // date in file names; nil means UTC
}

// Validate reports every invalid field as a *configerr.Field.
func (c Config) Validate() error {
	var errs []error
	if c.Dir == "" {
		errs = append(errs, &configerr.Field{Name: "Dir", Reason: "is required"})
	}
	if c.AlbumDebounce < 0 {
		errs = append(errs, &configerr.Field{Name: "AlbumDebounce", Reason: "must not be negative"})
	}
	return errors.Join(errs...)
}

// Permit decides who may save; access.Access implements it.
type Permit interface {
	CanSave(userID int64) bool
}

// KnownMedia finds memes already in the collection; mediacache.Cache implements
// it. Optional: without it the saver deduplicates within the sender's folder only.
type KnownMedia interface {
	KnownUniqueID(ctx context.Context, fileUniqueID string) (bool, error)
	KnownSHA256(ctx context.Context, sum [32]byte) (bool, error)
	AddLibraryFile(ctx context.Context, path string) error
}

// Deps are the collaborators the host passes in.
type Deps struct {
	Permit Permit
	Known  KnownMedia // may be nil
	I18n   *i18n.Bundle
	Log    *slog.Logger
	HTTP   *http.Client     // downloads files from Telegram; nil means http.DefaultClient
	Now    func() time.Time // nil means time.Now; used only for messages without a date
}

// Module implements module.Module.
type Module struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	// mu serializes the dedup check, name choice and write so two album items with
	// equal bytes cannot both pass the folder check. Downloads happen outside it.
	mu     sync.Mutex
	hashes folderHashes
	albums *albums
	httpc  *http.Client
}

var _ module.Module = (*Module)(nil)

// New validates the config, applies defaults and creates Dir if missing.
func New(cfg Config, deps Deps) (*Module, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("saver: %w", err)
	}
	if deps.Permit == nil {
		return nil, errors.New("saver: Deps.Permit is required")
	}
	if deps.I18n == nil {
		return nil, errors.New("saver: Deps.I18n is required")
	}
	if cfg.AlbumDebounce == 0 {
		cfg.AlbumDebounce = 2 * time.Second
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	httpc := deps.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("saver: create dir: %w", err)
	}
	m := &Module{cfg: cfg, deps: deps, log: deps.Log, httpc: httpc, hashes: folderHashes{}}
	m.albums = newAlbums(cfg.AlbumDebounce, m.flushAlbum)
	deps.Log.Info("saver: enabled", "dir", cfg.Dir, "dir_prefix", cfg.DirPrefix)
	return m, nil
}

// Register attaches the module's handlers.
func (m *Module) Register(b *bot.Bot) {
	b.RegisterHandlerMatchFunc(m.match, m.handle)
}
