// Package saver is the module that stores photos sent to the bot as files in a
// meme-search library folder.
package saver

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/go-telegram/bot"

	"github.com/grbit/meme-search-telegram-bot/configerr"
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

// Config tunes the module. Zero values mean the defaults noted per field.
type Config struct {
	Dir           string         // library root, required; save folders are created inside
	DirPrefix     string         // per_user folder name prefix, e.g. "tg-"; may be empty
	Note          string         // appended to the "saved" reply
	AlbumDebounce time.Duration  // wait after the last album item before replying; 0 means 2s
	Location      *time.Location // date in file names; nil means UTC
	Mode          Mode           // "" means ModePerUser
	// Folder is the one folder of ModeSingle and the parent of ModeNested:
	// letters, digits, "-" and "_" only, so it is a single path element
	// meme-search can register by name. "" means "telegram".
	Folder string
}

// Mode chooses the save folder layout.
type Mode string

// Save modes.
//
// meme-search collections are weak, so an owner may prefer one folder for
// everyone (registered once, deduplicated across users) to a folder per user.
const (
	ModePerUser Mode = "per_user" // Dir/<DirPrefix><sender>
	ModeSingle  Mode = "single"   // Dir/<Folder> for every sender
	// ModeNested is Dir/<Folder>/<sender>: meme-search registers the parent once
	// with "include subfolders" and picks up every new sender's folder itself.
	ModeNested Mode = "nested"
)

const defaultFolder = "telegram"

// validFolder accepts what sanitize would keep plus "-" and "_", so the
// folder is never nested, hidden (the library scan skips dot folders) or "..".
func validFolder(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
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
	switch c.Mode {
	case "", ModePerUser, ModeSingle, ModeNested:
	default:
		errs = append(errs, &configerr.Field{Name: "Mode",
			Reason: fmt.Sprintf("must be %q, %q or %q, got %q", ModePerUser, ModeSingle, ModeNested, c.Mode)})
	}
	if c.Folder != "" && !validFolder(c.Folder) {
		errs = append(errs, &configerr.Field{Name: "Folder",
			Reason: fmt.Sprintf("must be one folder name of letters, digits, - and _, got %q", c.Folder)})
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
	Now    func() time.Time // nil means time.Now; dates messages without one, ages pending notices
	// DB and Caps enable the "searchable now" notice; both are optional. It
	// is used only while Caps reports the server's change feed (listing),
	// where description_status arrives; feed pages come in via MemesChanged.
	DB   *sql.DB
	Caps memesearch.CapabilitySource
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
	// bot is the one Register got; MemesChanged runs outside any update.
	bot atomic.Pointer[bot.Bot]
	// downloadTimeout bounds getFile plus the download: an album reply waits for
	// every item, so one stalled connection must not hold it forever.
	downloadTimeout time.Duration
}

var _ module.Module = (*Module)(nil)

// defaultDownloadTimeout is generous: bot downloads are at most 20 MB.
const defaultDownloadTimeout = 2 * time.Minute

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
	if cfg.Mode == "" {
		cfg.Mode = ModePerUser
	}
	if cfg.Folder == "" {
		cfg.Folder = defaultFolder
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
	if deps.DB != nil {
		if err := migrate(deps.DB); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("saver: create dir: %w", err)
	}
	m := &Module{cfg: cfg, deps: deps, log: deps.Log, httpc: httpc, downloadTimeout: defaultDownloadTimeout, hashes: folderHashes{}}
	m.albums = newAlbums(cfg.AlbumDebounce, m.flushAlbum)
	if cfg.Mode == ModeSingle || cfg.Mode == ModeNested {
		deps.Log.Info("saver: enabled", "mode", string(cfg.Mode), "dir", cfg.Dir, "folder", cfg.Folder)
	} else {
		deps.Log.Info("saver: enabled", "mode", string(cfg.Mode), "dir", cfg.Dir, "dir_prefix", cfg.DirPrefix)
	}
	return m, nil
}

// Register attaches the module's handlers.
func (m *Module) Register(b *bot.Bot) {
	m.bot.Store(b)
	b.RegisterHandlerMatchFunc(m.match, m.handle)
}
