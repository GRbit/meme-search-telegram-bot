// Package config loads the host configuration: defaults, then an optional YAML
// file, then environment variables, each layer overriding the previous one.
//
// Environment names map to YAML keys through an explicit table rather than by
// splitting on "_", because keys themselves contain underscores
// (MEME_SEARCH_API_TOKEN -> meme_search.api_token).
package config

import (
	"errors"
	"log/slog"
	"time"

	"github.com/grbit/meme-search-telegram-bot/access"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/saver"
	"github.com/grbit/meme-search-telegram-bot/search"
)

// Config is the union of the host settings and every module's Config.
type Config struct {
	TelegramToken string
	LogLevel      slog.Level
	DBPath        string
	DefaultLang   string
	Location      *time.Location

	MemeSearch memesearch.Config
	Search     search.Config
	Media      mediacache.Config
	Access     access.Config
	Saver      saver.Config
	// SaverEnabled is true when at least one user may save.
	SaverEnabled bool
}

// Load builds the config. environ is os.Environ() in production; exeDir is the
// directory of the executable, the base for relative default paths.
func Load(environ []string, exeDir string) (Config, error) {
	return Config{}, errors.New("config: not implemented")
}

// Summary renders the effective config for the startup log with secrets masked.
func (c Config) Summary() string { return "" }
