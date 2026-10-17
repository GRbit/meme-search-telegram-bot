// Package access decides who may search and save, and enforces daily search limits.
package access

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/i18n"
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

// Validate reports every invalid field as a *configerr.Field. Zero limits are
// valid and turn public search off; a nil Location means UTC.
func (c Config) Validate() error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, &configerr.Field{Name: field, Reason: fmt.Sprintf(format, args...)})
	}
	for _, id := range c.SaveUserIDs {
		if id <= 0 {
			bad("SaveUserIDs", "user ids must be positive, got %d", id)
		}
	}
	for _, id := range c.TrustedUserIDs {
		if id <= 0 {
			bad("TrustedUserIDs", "user ids must be positive, got %d", id)
		}
	}
	if c.PublicDailyPerUser < 0 {
		bad("PublicDailyPerUser", "must not be negative, got %d", c.PublicDailyPerUser)
	}
	if c.PublicDailyTotal < 0 {
		bad("PublicDailyTotal", "must not be negative, got %d", c.PublicDailyTotal)
	}
	if c.InlineChargeWindow < 0 {
		bad("InlineChargeWindow", "must not be negative, got %s", c.InlineChargeWindow)
	}
	return errors.Join(errs...)
}

// Deps are the collaborators the host passes in.
type Deps struct {
	DB   *sql.DB
	I18n *i18n.Bundle
	Log  *slog.Logger
	Now  func() time.Time // nil means time.Now
}

// migrations are append-only; see store.Migrate.
var migrations = []string{
	`CREATE TABLE access_usage (
		day TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		count INTEGER NOT NULL,
		PRIMARY KEY (day, user_id)
	)`,
	`CREATE TABLE access_usage_total (
		day TEXT PRIMARY KEY,
		count INTEGER NOT NULL
	)`,
}

const (
	dayLayout = "2006-01-02"
	// keepDays of counters are kept for debugging; only today's row is ever read.
	keepDays = 7
	// inlineSweepAt bounds the inline window map: above it, expired entries are dropped.
	inlineSweepAt = 1024
)

// Access resolves tiers and charges searches.
type Access struct {
	cfg     Config
	db      *sql.DB
	i18n    *i18n.Bundle
	log     *slog.Logger
	now     func() time.Time
	savers  map[int64]bool
	trusted map[int64]bool

	// inlineMu serializes inline charges so concurrent keystrokes of one user
	// cannot each start a window. lastInline lives in memory only: a restart
	// at worst charges one extra inline query per user, which is acceptable.
	inlineMu   sync.Mutex
	lastInline map[int64]time.Time

	pruneMu   sync.Mutex
	prunedDay string
}

// New validates the config and applies the package's migrations.
func New(ctx context.Context, cfg Config, deps Deps) (*Access, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.DB == nil {
		return nil, errors.New("access: Deps.DB is nil")
	}
	if deps.I18n == nil {
		return nil, errors.New("access: Deps.I18n is nil")
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
	if err := store.Check(ctx, deps.DB); err != nil {
		return nil, fmt.Errorf("access: %w", err)
	}
	if err := store.Migrate(ctx, deps.DB, "access", migrations); err != nil {
		return nil, fmt.Errorf("access: %w", err)
	}
	a := &Access{
		cfg:        cfg,
		db:         deps.DB,
		i18n:       deps.I18n,
		log:        deps.Log,
		now:        deps.Now,
		savers:     make(map[int64]bool, len(cfg.SaveUserIDs)),
		trusted:    make(map[int64]bool, len(cfg.TrustedUserIDs)),
		lastInline: map[int64]time.Time{},
	}
	for _, id := range cfg.SaveUserIDs {
		a.savers[id] = true
	}
	for _, id := range cfg.TrustedUserIDs {
		a.trusted[id] = true
	}
	a.log.Info("access ready", "savers", len(a.savers), "trusted", len(a.trusted),
		"public_daily_per_user", cfg.PublicDailyPerUser, "public_daily_total", cfg.PublicDailyTotal,
		"public_enabled", a.publicEnabled(), "tz", cfg.Location.String())
	return a, nil
}

func (a *Access) publicEnabled() bool {
	return a.cfg.PublicDailyPerUser > 0 && a.cfg.PublicDailyTotal > 0
}

// Tier returns the user's tier.
func (a *Access) Tier(userID int64) Tier {
	switch {
	case a.savers[userID]:
		return TierSaver
	case a.trusted[userID]:
		return TierTrusted
	default:
		return TierPublic
	}
}

// CanSave reports whether the user may save memes.
func (a *Access) CanSave(userID int64) bool { return a.Tier(userID) == TierSaver }

// verdict is the outcome of charging one search.
type verdict int

const (
	allowed verdict = iota
	deniedPublicOff
	deniedUser
	deniedTotal
)

func (v verdict) String() string {
	return [...]string{"allowed", "public_off", "limit_user", "limit_total"}[v]
}

// updateKind is the part of an update the middleware knows how to answer.
type updateKind int

const (
	kindNone updateKind = iota
	kindMessage
	kindInline
	kindCallback
)

func (k updateKind) String() string {
	return [...]string{"none", "message", "inline_query", "callback_query"}[k]
}

func sender(u *models.Update) (*models.User, updateKind) {
	switch {
	case u.Message != nil && u.Message.From != nil:
		return u.Message.From, kindMessage
	case u.InlineQuery != nil && u.InlineQuery.From != nil:
		return u.InlineQuery.From, kindInline
	case u.CallbackQuery != nil && u.CallbackQuery.From.ID != 0:
		return &u.CallbackQuery.From, kindCallback
	}
	return nil, kindNone
}

// Middleware charges updates for which chargeable returns true against the
// user's tier and daily limits. Over the limit it answers the update itself
// (message, inline query or callback query) with the localized limit text and
// does not call next.
func (a *Access) Middleware(chargeable func(*models.Update) bool) bot.Middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, u *models.Update) {
			if !chargeable(u) {
				next(ctx, b, u)
				return
			}
			from, kind := sender(u)
			if from == nil {
				a.log.Debug("access: chargeable update without a user, passing", "update_id", u.ID)
				next(ctx, b, u)
				return
			}
			tier := a.Tier(from.ID)
			if tier != TierPublic {
				a.log.Debug("access: privileged, not charged", "user_id", from.ID, "tier", int(tier), "kind", kind.String())
				next(ctx, b, u)
				return
			}
			v, err := a.charge(ctx, from.ID, kind == kindInline)
			if err != nil {
				// Fail open: a broken counter store should not take search down
				// for everyone, and the API has its own rate limit behind us.
				a.log.Error("access: charge failed, allowing", "user_id", from.ID, "err", err)
				next(ctx, b, u)
				return
			}
			if v == allowed {
				next(ctx, b, u)
				return
			}
			a.deny(ctx, b, u, kind, from, v)
		}
	}
}

// charge decides one public search and records it if allowed.
func (a *Access) charge(ctx context.Context, userID int64, inline bool) (verdict, error) {
	if !a.publicEnabled() {
		a.log.Debug("access: denied", "user_id", userID, "reason", deniedPublicOff.String())
		return deniedPublicOff, nil
	}
	now := a.now()
	if !inline || a.cfg.InlineChargeWindow <= 0 {
		return a.chargeDB(ctx, userID, now)
	}

	a.inlineMu.Lock()
	defer a.inlineMu.Unlock()
	if last, ok := a.lastInline[userID]; ok && now.Sub(last) < a.cfg.InlineChargeWindow {
		a.log.Debug("access: inline query inside charge window, free", "user_id", userID,
			"since_charge", now.Sub(last).String())
		return allowed, nil
	}
	v, err := a.chargeDB(ctx, userID, now)
	if err == nil && v == allowed {
		a.lastInline[userID] = now
		a.sweepInline(now)
	}
	return v, err
}

// sweepInline drops expired windows once the map grows; callers hold inlineMu.
func (a *Access) sweepInline(now time.Time) {
	if len(a.lastInline) < inlineSweepAt {
		return
	}
	for id, t := range a.lastInline {
		if now.Sub(t) >= a.cfg.InlineChargeWindow {
			delete(a.lastInline, id)
		}
	}
}

// chargeDB checks and bumps both counters in one write transaction, so
// concurrent searches can never push a counter past its limit.
func (a *Access) chargeDB(ctx context.Context, userID int64, now time.Time) (verdict, error) {
	local := now.In(a.cfg.Location)
	day := local.Format(dayLayout)
	a.pruneMu.Lock()
	prune := a.prunedDay != day
	a.pruneMu.Unlock()

	var v verdict
	var userCount, total int
	err := store.WriteTx(ctx, a.db, func(tx *sql.Tx) error {
		if prune {
			cutoff := local.AddDate(0, 0, -keepDays).Format(dayLayout)
			if _, err := tx.ExecContext(ctx, `DELETE FROM access_usage WHERE day < ?`, cutoff); err != nil {
				return fmt.Errorf("prune usage: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM access_usage_total WHERE day < ?`, cutoff); err != nil {
				return fmt.Errorf("prune totals: %w", err)
			}
		}
		if err := scanCount(tx.QueryRowContext(ctx,
			`SELECT count FROM access_usage WHERE day = ? AND user_id = ?`, day, userID), &userCount); err != nil {
			return fmt.Errorf("read usage: %w", err)
		}
		if err := scanCount(tx.QueryRowContext(ctx,
			`SELECT count FROM access_usage_total WHERE day = ?`, day), &total); err != nil {
			return fmt.Errorf("read total: %w", err)
		}
		switch {
		case userCount >= a.cfg.PublicDailyPerUser:
			v = deniedUser
			return nil
		case total >= a.cfg.PublicDailyTotal:
			v = deniedTotal
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO access_usage (day, user_id, count) VALUES (?, ?, 1)
			ON CONFLICT (day, user_id) DO UPDATE SET count = count + 1`, day, userID); err != nil {
			return fmt.Errorf("bump usage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO access_usage_total (day, count) VALUES (?, 1)
			ON CONFLICT (day) DO UPDATE SET count = count + 1`, day); err != nil {
			return fmt.Errorf("bump total: %w", err)
		}
		userCount, total = userCount+1, total+1
		v = allowed
		return nil
	})
	if err != nil {
		return allowed, fmt.Errorf("access: charge: %w", err)
	}
	if prune {
		a.pruneMu.Lock()
		a.prunedDay = day
		a.pruneMu.Unlock()
		a.log.Debug("access: pruned old counters", "day", day, "keep_days", keepDays)
	}
	a.log.Debug("access: charge decided", "user_id", userID, "tier", int(TierPublic), "day", day,
		"verdict", v.String(), "user_count", userCount, "user_limit", a.cfg.PublicDailyPerUser,
		"total", total, "total_limit", a.cfg.PublicDailyTotal)
	return v, nil
}

func scanCount(row *sql.Row, dst *int) error {
	err := row.Scan(dst)
	if errors.Is(err, sql.ErrNoRows) {
		*dst = 0
		return nil
	}
	return err
}

// deny answers the update with the localized reason in the form its kind allows.
func (a *Access) deny(ctx context.Context, b *bot.Bot, u *models.Update, kind updateKind, from *models.User, v verdict) {
	loc := a.i18n.Localizer(from.LanguageCode)
	data := map[string]any{"Admin": a.cfg.AdminContact, "Limit": a.cfg.PublicDailyPerUser}
	var text, short string
	switch v {
	case deniedUser:
		text = loc.N("access.limit_user", a.cfg.PublicDailyPerUser, data)
		short = loc.T("access.limit_user_short", data)
	case deniedTotal:
		text = loc.T("access.limit_total", data)
		short = loc.T("access.limit_total_short", data)
	default:
		text = loc.T("access.public_off", data)
		short = loc.T("access.public_off_short", data)
	}

	var err error
	switch kind {
	case kindMessage:
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          u.Message.Chat.ID,
			Text:            text,
			ReplyParameters: &models.ReplyParameters{MessageID: u.Message.ID},
		})
	case kindInline:
		_, err = b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
			InlineQueryID: u.InlineQuery.ID,
			// Telegram rejects a null results field; it must be an empty array.
			Results:    []models.InlineQueryResult{},
			IsPersonal: true,
			// 1, not 0: the library omits a zero cache_time and Telegram then
			// caches the denial for its default 300s.
			CacheTime: 1,
			Button:    &models.InlineQueryResultsButton{Text: short, StartParameter: "limit"},
		})
	case kindCallback:
		_, err = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: u.CallbackQuery.ID,
			Text:            text,
			ShowAlert:       true,
		})
	}
	if err != nil {
		a.log.Warn("access: sending denial failed", "user_id", from.ID, "kind", kind.String(), "err", err)
		return
	}
	a.log.Debug("access: denial sent", "user_id", from.ID, "kind", kind.String(), "reason", v.String(),
		"lang", from.LanguageCode)
}
