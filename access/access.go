// Package access decides who may search and save, and enforces daily search limits.
package access

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
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

// Tier is a user's access level.
type Tier int

// Tiers from least to most privileged.
const (
	TierPublic Tier = iota
	TierTrusted
	TierSaver
)

// Config lists privileged users and public limits. Zero limits disable public
// search; a zero InlineChargeWindow charges every inline query.
type Config struct {
	SaveUserIDs        []int64
	TrustedUserIDs     []int64
	PublicDailyPerUser int
	PublicDailyTotal   int
	// InlineChargeWindow: inline queries from one user within this window count
	// as one search, because Telegram sends a query per keystroke.
	InlineChargeWindow time.Duration
	AdminContact       string
	Location           *time.Location // day boundary for counters; nil means UTC
}

// Validate reports every invalid field as a *configerr.Field.
func (c Config) Validate() error { return nil }

// Deps are the collaborators the host passes in.
type Deps struct {
	DB   *sql.DB
	I18n *i18n.Bundle
	Log  *slog.Logger
	Now  func() time.Time // nil means time.Now
}

// Access resolves tiers and charges searches.
type Access struct{}

// New validates the config and applies the package's migrations.
func New(ctx context.Context, cfg Config, deps Deps) (*Access, error) {
	return nil, errors.New("access: not implemented")
}

// Tier returns the user's tier.
func (a *Access) Tier(userID int64) Tier { return TierPublic }

// CanSave reports whether the user may save memes.
func (a *Access) CanSave(userID int64) bool { return false }

// Middleware charges updates for which chargeable returns true against the
// user's tier and daily limits. Over the limit it answers the update itself
// (message or inline query) with the localized limit text and does not call next.
func (a *Access) Middleware(chargeable func(*models.Update) bool) bot.Middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc { return next }
}
